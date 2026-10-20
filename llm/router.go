package llm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/compforge/agentgo"
)

const (
	RoutePriority   = "priority"
	RouteRoundRobin = "round-robin"
)

// Route binds a safe endpoint label to an already configured model.
// Models must honor context cancellation and disable their own retries when the
// router owns the attempt budget.
type Route struct {
	Name  string
	Model agentgo.ChatModel
}

type RouterConfig struct {
	Policy         string
	CallTimeout    time.Duration // zero inherits the caller's deadline
	AttemptTimeout time.Duration // zero adds no per-endpoint deadline
	Cooldown       time.Duration // zero defaults to 30s; negative disables
	MaxAttempts    int           // zero visits each route at most once
	// Observe is synchronous and may run concurrently across calls. It must not
	// block or mutate the payload. Loop events are emitted independently.
	Observe func(context.Context, agentgo.ModelAttempt, error)
}

// Router composes models without owning application model selection policy.
//
// +spec=`One call has one deadline and a bounded number of endpoint attempts; published streams and length responses never trigger transparent failover`
// +why=`Routing is a ChatModel composition so loops and one-shot calls share the same availability contract`
// +link=component://docs/routing.md
type Router struct {
	routes []Route
	config RouterConfig
	mu     sync.Mutex
	until  []time.Time
	next   uint64
}

func NewRouter(routes []Route, config RouterConfig) (*Router, error) {
	if len(routes) == 0 {
		return nil, fmt.Errorf("llm router: empty route pool")
	}
	if config.Policy == "" {
		config.Policy = RoutePriority
	}
	if config.Policy != RoutePriority && config.Policy != RouteRoundRobin {
		return nil, fmt.Errorf("llm router: unknown policy %q", config.Policy)
	}
	if config.CallTimeout < 0 || config.AttemptTimeout < 0 || config.MaxAttempts < 0 {
		return nil, fmt.Errorf("llm router: timeouts and max attempts must be nonnegative")
	}
	seen := make(map[string]bool)
	for _, route := range routes {
		if route.Name == "" || route.Model == nil || seen[route.Name] {
			return nil, fmt.Errorf("llm router: routes require unique nonempty names and models")
		}
		seen[route.Name] = true
	}
	if config.Cooldown == 0 {
		config.Cooldown = 30 * time.Second
	}
	return &Router{routes: append([]Route(nil), routes...), config: config, until: make([]time.Time, len(routes))}, nil
}

// RoutingError preserves the final cause and prevents outer retry loops from
// silently restarting an already consumed routing budget.
type RoutingError struct {
	Attempt agentgo.ModelAttempt
	Cause   error
}

func (e *RoutingError) Error() string {
	return fmt.Sprintf("llm route %q attempt %d failed (timeout_scope=%s): %v", e.Attempt.Endpoint, e.Attempt.Index, e.Attempt.TimeoutScope, e.Cause)
}
func (e *RoutingError) Unwrap() error   { return e.Cause }
func (e *RoutingError) Retryable() bool { return false }

func (r *Router) SupportsTools() bool {
	for _, route := range r.routes {
		if !route.Model.SupportsTools() {
			return false
		}
	}
	return true
}

func (r *Router) Generate(ctx context.Context, messages []agentgo.Message, tools []agentgo.ToolSpec, opts ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
	parent := ctx
	ctx, cancel := routeContext(ctx, r.config.CallTimeout)
	defer cancel()
	order := r.order()
	var last error
	for index, member := range order {
		if err := ctx.Err(); err != nil {
			return nil, r.deadlineError(parent, ctx, index, member, err)
		}
		attemptCtx, stop := routeContext(ctx, r.config.AttemptTimeout)
		started := time.Now()
		response, err := r.routes[member].Model.Generate(attemptCtx, messages, tools, opts...)
		if err == nil {
			err = attemptCtx.Err()
		}
		if err == nil && response == nil {
			err = errors.New("llm router: model returned a nil response")
		}
		var message agentgo.Message
		if response != nil {
			message = response.Message
		}
		fact := r.finish(parent, ctx, attemptCtx, member, index, started, message, false, err, index+1 < len(order))
		stop()
		if err == nil {
			copy := *response
			copy.Message = routedMessage(response.Message, fact.Endpoint)
			return &copy, nil
		}
		last = &RoutingError{Attempt: fact, Cause: err}
		if !fact.WillFailover {
			return response, last
		}
	}
	return nil, last
}

func (r *Router) GenerateStream(ctx context.Context, messages []agentgo.Message, tools []agentgo.ToolSpec, opts ...agentgo.CallOption) (<-chan agentgo.StreamEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	events := make(chan agentgo.StreamEvent)
	go func() {
		defer close(events)
		parent := ctx
		ctx, cancel := routeContext(ctx, r.config.CallTimeout)
		defer cancel()
		order := r.order()
		for index, member := range order {
			if err := ctx.Err(); err != nil {
				sendRouteEvent(parent, events, agentgo.StreamEvent{Type: agentgo.StreamEventError, Err: r.deadlineError(parent, ctx, index, member, err)})
				return
			}
			attemptCtx, stop := routeContext(ctx, r.config.AttemptTimeout)
			started := time.Now()
			stream, err := r.routes[member].Model.GenerateStream(attemptCtx, messages, tools, opts...)
			var message agentgo.Message
			published, done := false, false
			if err == nil {
				message, published, done, err = relayRouteStream(attemptCtx, events, stream, r.routes[member].Name)
			}
			fact := r.finish(parent, ctx, attemptCtx, member, index, started, message, published, err, index+1 < len(order))
			stop()
			if done {
				sendRouteEvent(parent, events, agentgo.StreamEvent{Type: agentgo.StreamEventDone, Message: message, StopReason: message.StopReason})
				return
			}
			if !fact.WillFailover {
				sendRouteEvent(parent, events, agentgo.StreamEvent{Type: agentgo.StreamEventError, Message: message, Err: &RoutingError{Attempt: fact, Cause: err}})
				return
			}
		}
	}()
	return events, nil
}

func relayRouteStream(ctx context.Context, out chan<- agentgo.StreamEvent, in <-chan agentgo.StreamEvent, endpoint string) (message agentgo.Message, published, done bool, err error) {
	for {
		select {
		case <-ctx.Done():
			return message, published, false, ctx.Err()
		case event, ok := <-in:
			if !ok {
				return message, published, false, &agentgo.PartialStreamError{Partial: message}
			}
			if event.Type == agentgo.StreamEventError {
				if event.Err == nil {
					event.Err = errors.New("llm router: stream error without cause")
				}
				if event.Message.Role != "" || len(event.Message.Content) > 0 || event.Message.Usage != nil {
					message = routedMessage(event.Message, endpoint)
				}
				return message, published, false, event.Err
			}
			event.Message = routedMessage(event.Message, endpoint)
			message = event.Message
			if event.Type == agentgo.StreamEventDone {
				if err := ctx.Err(); err != nil {
					return message, published, false, err
				}
				return message, published, true, nil
			}
			if !sendRouteEvent(ctx, out, event) {
				return message, published, false, ctx.Err()
			}
			published = true
		}
	}
}

func sendRouteEvent(ctx context.Context, out chan<- agentgo.StreamEvent, event agentgo.StreamEvent) bool {
	select {
	case out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *Router) finish(parent, call, attempt context.Context, member, index int, started time.Time, message agentgo.Message, published bool, err error, hasNext bool) agentgo.ModelAttempt {
	fact := agentgo.ModelAttempt{Index: index + 1, Endpoint: r.routes[member].Name, Duration: time.Since(started), Published: published}
	if model, ok := r.routes[member].Model.(agentgo.ModelNamer); ok {
		fact.Model = model.ModelName()
	}
	if message.Usage != nil {
		usage := *message.Usage
		fact.Usage = &usage
		fact.Model = usage.Model
	}
	if err != nil {
		fact.ErrorKind = agentgo.ErrorKind(err)
		switch {
		case parent.Err() != nil:
			fact.TimeoutScope = "caller"
		case call.Err() != nil:
			fact.TimeoutScope = "routing"
		case attempt.Err() != nil || errors.Is(err, agentgo.ErrProviderTimeout) || errors.Is(err, context.DeadlineExceeded):
			fact.TimeoutScope = "endpoint"
		}
		eligible := failoverEligible(err)
		if eligible && call.Err() == nil {
			r.mu.Lock()
			r.until[member] = time.Now().Add(r.config.Cooldown)
			r.mu.Unlock()
		}
		fact.WillFailover = hasNext && !published && call.Err() == nil && eligible
	}
	agentgo.ReportModelAttempt(parent, fact, err)
	if r.config.Observe != nil {
		r.config.Observe(parent, fact, err)
	}
	return fact
}

func failoverEligible(err error) bool {
	var retryable agentgo.RetryableError
	if errors.As(err, &retryable) && !retryable.Retryable() {
		return false
	}
	if agentgo.IsContextOverflow(err) {
		return false
	}
	return agentgo.IsFailoverEligible(err) || errors.Is(err, agentgo.ErrStreamPartial) || (retryable != nil && retryable.Retryable())
}

func (r *Router) deadlineError(parent, call context.Context, index, member int, err error) error {
	scope := "routing"
	if parent.Err() != nil {
		scope = "caller"
		err = parent.Err()
	}
	return &RoutingError{Attempt: agentgo.ModelAttempt{Index: index + 1, Endpoint: r.routes[member].Name, TimeoutScope: scope}, Cause: err}
}

func routedMessage(message agentgo.Message, endpoint string) agentgo.Message {
	message.Metadata = maps.Clone(message.Metadata)
	if message.Metadata == nil {
		message.Metadata = make(map[string]any)
	}
	message.Metadata["route_endpoint"] = endpoint
	return message
}

func routeContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

func (r *Router) order() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	live, parked := make([]int, 0, len(r.routes)), make([]int, 0, len(r.routes))
	for i := range r.routes {
		if now.Before(r.until[i]) {
			parked = append(parked, i)
		} else {
			live = append(live, i)
		}
	}
	if r.config.Policy == RouteRoundRobin && len(live) > 1 {
		start := int(r.next % uint64(len(live)))
		r.next++
		live = append(append(make([]int, 0, len(live)), live[start:]...), live[:start]...)
	}
	order := append(live, parked...)
	if r.config.MaxAttempts > 0 && len(order) > r.config.MaxAttempts {
		order = order[:r.config.MaxAttempts]
	}
	return order
}
