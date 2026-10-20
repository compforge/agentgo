package llm_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compforge/agentgo"
	"github.com/compforge/agentgo/llm"
)

type routeModel struct {
	generate func(context.Context, []agentgo.CallOption) (*agentgo.LLMResponse, error)
	stream   func(context.Context) (<-chan agentgo.StreamEvent, error)
	calls    atomic.Int32
}

func (m *routeModel) SupportsTools() bool { return true }
func (m *routeModel) Generate(ctx context.Context, _ []agentgo.Message, _ []agentgo.ToolSpec, opts ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
	m.calls.Add(1)
	if m.generate != nil {
		return m.generate(ctx, opts)
	}
	return &agentgo.LLMResponse{Message: agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("ok")}, StopReason: agentgo.StopReasonStop}}, nil
}
func (m *routeModel) GenerateStream(ctx context.Context, _ []agentgo.Message, _ []agentgo.ToolSpec, opts ...agentgo.CallOption) (<-chan agentgo.StreamEvent, error) {
	if m.stream != nil {
		m.calls.Add(1)
		return m.stream(ctx)
	}
	response, err := m.Generate(ctx, nil, nil, opts...)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentgo.StreamEvent, 1)
	ch <- agentgo.StreamEvent{Type: agentgo.StreamEventDone, Message: response.Message}
	close(ch)
	return ch, nil
}

func router(t *testing.T, a, b agentgo.ChatModel, config llm.RouterConfig) *llm.Router {
	t.Helper()
	r, err := llm.NewRouter([]llm.Route{{Name: "primary", Model: a}, {Name: "backup", Model: b}}, config)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRouterTimeoutLeavesFallbackAndPreservesOptions(t *testing.T) {
	first := &routeModel{generate: func(ctx context.Context, _ []agentgo.CallOption) (*agentgo.LLMResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	var facts []agentgo.ModelAttempt
	second := &routeModel{generate: func(_ context.Context, opts []agentgo.CallOption) (*agentgo.LLMResponse, error) {
		cfg := agentgo.ResolveCallConfig(opts)
		if cfg.ThinkingLevel != agentgo.ThinkingMedium || cfg.MaxTokens != 123 || cfg.PromptCacheKey != "session" {
			t.Errorf("lost config: %+v", cfg)
		}
		return &agentgo.LLMResponse{Message: agentgo.Message{StopReason: agentgo.StopReasonLength}}, nil
	}}
	r := router(t, first, second, llm.RouterConfig{CallTimeout: time.Second, AttemptTimeout: 10 * time.Millisecond, Observe: func(_ context.Context, a agentgo.ModelAttempt, _ error) { facts = append(facts, a) }})
	response, err := r.Generate(t.Context(), nil, nil, agentgo.WithThinking(agentgo.ThinkingMedium), agentgo.WithMaxTokens(123), agentgo.WithCallPromptCacheKey("session"))
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.StopReason != agentgo.StopReasonLength || response.Message.Metadata["route_endpoint"] != "backup" {
		t.Fatalf("response=%+v", response)
	}
	if len(facts) != 2 || !facts[0].WillFailover || facts[0].TimeoutScope != "endpoint" {
		t.Fatalf("facts=%+v", facts)
	}
	// Cooling down a failed primary is shared with the next call.
	_, err = r.Generate(t.Context(), nil, nil, agentgo.WithThinking(agentgo.ThinkingMedium), agentgo.WithMaxTokens(123), agentgo.WithCallPromptCacheKey("session"))
	if err != nil || first.calls.Load() != 1 {
		t.Fatalf("cooldown: calls=%d err=%v", first.calls.Load(), err)
	}
}

func TestRouterDoesNotFailoverDeterministicErrorsOrLength(t *testing.T) {
	for _, err := range []error{agentgo.ErrProviderAuth, agentgo.ErrContextOverflow, errors.New("unsupported effort"), context.Canceled, nil} {
		t.Run(agentgo.ErrorKind(err), func(t *testing.T) {
			first := &routeModel{generate: func(context.Context, []agentgo.CallOption) (*agentgo.LLMResponse, error) {
				if err != nil {
					return nil, err
				}
				return &agentgo.LLMResponse{Message: agentgo.Message{StopReason: agentgo.StopReasonLength}}, nil
			}}
			second := &routeModel{}
			_, got := router(t, first, second, llm.RouterConfig{}).Generate(t.Context(), nil, nil)
			if second.calls.Load() != 0 || !errors.Is(got, err) {
				t.Fatalf("calls=%d err=%v", second.calls.Load(), got)
			}
		})
	}
}

func TestRouterDeadlineAndAttemptBudget(t *testing.T) {
	for _, caller := range []bool{true, false} {
		first := &routeModel{generate: func(ctx context.Context, _ []agentgo.CallOption) (*agentgo.LLMResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		second := &routeModel{}
		ctx := t.Context()
		timeout := 10 * time.Millisecond
		if caller {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
			timeout = time.Second
		}
		_, err := router(t, first, second, llm.RouterConfig{CallTimeout: timeout}).Generate(ctx, nil, nil)
		var routed *llm.RoutingError
		if !errors.As(err, &routed) || !errors.Is(err, context.DeadlineExceeded) || second.calls.Load() != 0 {
			t.Fatalf("error=%v calls=%d", err, second.calls.Load())
		}
		want := "routing"
		if caller {
			want = "caller"
		}
		if routed.Attempt.TimeoutScope != want || routed.Retryable() {
			t.Fatalf("error=%+v", routed)
		}
	}
	first := &routeModel{generate: func(context.Context, []agentgo.CallOption) (*agentgo.LLMResponse, error) {
		return nil, agentgo.ErrProviderRateLimit
	}}
	second := &routeModel{}
	_, err := router(t, first, second, llm.RouterConfig{MaxAttempts: 1}).Generate(t.Context(), nil, nil)
	if err == nil || second.calls.Load() != 0 {
		t.Fatalf("attempt bound: %v", err)
	}
}

func TestRouterStreamPublicationBoundary(t *testing.T) {
	for _, publish := range []bool{false, true} {
		for _, closeEarly := range []bool{false, true} {
			first := &routeModel{stream: func(context.Context) (<-chan agentgo.StreamEvent, error) {
				ch := make(chan agentgo.StreamEvent, 2)
				if publish {
					ch <- agentgo.StreamEvent{Type: agentgo.StreamEventTextDelta, Delta: "partial", Message: agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("partial")}}}
				}
				if !closeEarly {
					ch <- agentgo.StreamEvent{Type: agentgo.StreamEventError, Err: agentgo.ErrProviderNetwork}
				}
				close(ch)
				return ch, nil
			}}
			second := &routeModel{}
			r := router(t, first, second, llm.RouterConfig{})
			// A consumed router budget must also stop AgentLoop retries, including
			// PartialStreamError's legacy automatic-retry path.
			var attemptFacts int
			var failed bool
			for ev := range agentgo.AgentLoop(t.Context(), []agentgo.AgentMessage{agentgo.UserMsg("test")}, agentgo.AgentContext{}, agentgo.LoopConfig{Model: r, MaxRetries: 3, MaxTurns: 1}) {
				if ev.Type == agentgo.EventModelAttempt {
					attemptFacts++
				}
				if ev.Type == agentgo.EventError {
					failed = true
				}
			}
			if publish {
				if second.calls.Load() != 0 || first.calls.Load() != 1 || !failed || attemptFacts != 1 {
					t.Fatalf("published stream retried: first=%d second=%d failed=%v facts=%d", first.calls.Load(), second.calls.Load(), failed, attemptFacts)
				}
			} else if second.calls.Load() != 1 || failed || attemptFacts != 2 {
				t.Fatalf("fallback failed: calls=%d failed=%v facts=%d", second.calls.Load(), failed, attemptFacts)
			}
		}
	}
}

func TestRouterRoundRobinConcurrent(t *testing.T) {
	first, second := &routeModel{}, &routeModel{}
	r := router(t, first, second, llm.RouterConfig{Policy: llm.RouteRoundRobin})
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			if _, err := r.Generate(t.Context(), nil, nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if first.calls.Load() != 20 || second.calls.Load() != 20 {
		t.Fatalf("distribution=%d/%d", first.calls.Load(), second.calls.Load())
	}
}

func TestRouterAllParkedAndCancellation(t *testing.T) {
	first := &routeModel{generate: func(context.Context, []agentgo.CallOption) (*agentgo.LLMResponse, error) {
		return nil, agentgo.ErrProviderRateLimit
	}}
	second := &routeModel{generate: func(context.Context, []agentgo.CallOption) (*agentgo.LLMResponse, error) {
		return nil, agentgo.ErrProviderNetwork
	}}
	r := router(t, first, second, llm.RouterConfig{})
	for range 2 {
		if _, err := r.Generate(t.Context(), nil, nil); err == nil {
			t.Fatal("expected exhausted pool")
		}
	}
	if first.calls.Load() != 2 || second.calls.Load() != 2 {
		t.Fatal("all parked routes must remain available as last resort")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.Generate(ctx, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if first.calls.Load() != 2 {
		t.Fatal("called provider after cancellation")
	}
}
