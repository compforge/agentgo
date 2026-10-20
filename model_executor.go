package agentgo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// ModelExecutorConfig configures shared model execution policy. Callbacks may run
// concurrently across calls; events within one call are serialized. Observers
// own their recording errors and must return promptly.
type ModelExecutorConfig struct {
	Middlewares []ModelMiddleware
	OnEvent     func(Event)
}

// ModelExecutor executes a single streaming model attempt and returns its final
// result. It owns middleware and observation, not routing, retries or history.
// A nil or zero executor uses the default execution policy.
type ModelExecutor struct {
	middlewares []ModelMiddleware
	onEvent     func(Event)
}

func NewModelExecutor(config ModelExecutorConfig) *ModelExecutor {
	return &ModelExecutor{middlewares: slices.Clone(config.Middlewares), onEvent: config.OnEvent}
}

// modelEventSinkKey routes facts to the owning run. It never supplies executor
// policy: nested callers must receive their executor explicitly.
type modelEventSinkKey struct{}
type modelAttemptReporterKey struct{}

func (e *ModelExecutor) emit(ctx context.Context, event Event) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if e != nil && e.onEvent != nil {
		e.onEvent(event)
	}
	if emit, ok := ctx.Value(modelEventSinkKey{}).(func(Event)); ok {
		emit(event)
	}
}

// Call consumes GenerateStream, publishes transient model_stream facts, and
// returns the final message or partial failure evidence. It never commits a
// conversation message. Callers own retries and supply execution coordinates;
// an omitted ParentID inherits the enclosing execution.
//
// +spec=`Model calls always consume a stream; executor policy is explicit and failed responses remain diagnostic evidence`
// +link=component://docs/session.md
func (e *ModelExecutor) Call(ctx context.Context, model ChatModel, execution ModelExecution) (ModelResult, error) {
	return e.execute(ctx, execution, func(ctx context.Context, call ModelExecution, emit func(Event)) (ModelResult, error) {
		return consumeModelStream(ctx, model, call, func(event StreamEvent) {
			emit(Event{Type: EventModelStream, Execution: executionRef(call.Execution), ModelStream: &event})
		})
	})
}

// +spec=`Request.Config is the sole call input; model_request freezes it after middleware with credentials redacted; short-circuits have no executor request`
// +link=component://docs/session.md
func (e *ModelExecutor) execute(ctx context.Context, execution ModelExecution, next func(context.Context, ModelExecution, func(Event)) (ModelResult, error)) (result ModelResult, err error) {
	if parent, ok := ExecutionFromContext(ctx); ok && execution.ParentID == "" && parent.ID != execution.ID {
		execution.ParentID = parent.ID
	}
	if execution.Request.Config == nil {
		execution.Request.Config = &CallConfig{}
	} else {
		config := *execution.Request.Config
		execution.Request.Config = &config
	}
	ctx = ContextWithExecution(ctx, execution.Execution)
	// Routers may report from their producer goroutine. Serialize facts within a
	// call and stop late reports before the owning run can close its event stream.
	var eventMu sync.Mutex
	closed := false
	emit := func(event Event) {
		eventMu.Lock()
		defer eventMu.Unlock()
		if !closed {
			e.emit(ctx, event)
		}
	}
	ctx = context.WithValue(ctx, modelAttemptReporterKey{}, func(attempt ModelAttempt, err error) {
		emit(Event{Type: EventModelAttempt, Execution: executionRef(execution.Execution), ModelAttempt: &attempt, Err: err})
	})
	emit(Event{Type: EventModelExecStart, Execution: executionRef(execution.Execution), ModelCall: modelCallSettings(execution.Request.Config)})
	var dispatched *ModelCallSettings
	defer func() {
		if recovered := recover(); recovered != nil {
			result, err = ModelResult{}, fmt.Errorf("model execution %s panicked: %v", execution.ID, recovered)
		}
		var partial *PartialStreamError
		if errors.As(err, &partial) && result.Message.Role == "" && len(result.Message.Content) == 0 && result.Message.Usage == nil {
			result.Message = partial.Partial
		}
		eventMu.Lock()
		defer eventMu.Unlock()
		closed = true
		e.emit(ctx, Event{Type: EventModelExecEnd, Execution: executionRef(execution.Execution), Message: result.Message, ModelCall: dispatched, Err: err})
	}()
	execute := func(ctx context.Context, call ModelExecution) (ModelResult, error) {
		dispatched = modelCallSettings(call.Request.Config)
		request := snapshotModelRequest(call.Request)
		emit(Event{Type: EventModelRequest, Execution: executionRef(call.Execution), ModelRequest: &request})
		return next(ctx, call, emit)
	}
	if e != nil {
		execute = buildModelMiddlewareChain(execute, e.middlewares)
	}
	return execute(ctx, execution)
}

// consumeModelStream is shared by conversation and auxiliary calls. Done is the
// only successful terminal event; a closed or failed stream preserves evidence.
func consumeModelStream(ctx context.Context, model ChatModel, call ModelExecution, observe func(StreamEvent)) (ModelResult, error) {
	if model == nil {
		return ModelResult{}, ErrNoModel
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := model.GenerateStream(ctx, call.Request.Messages, call.Request.Tools, requestConfigOption(call.Request))
	if err != nil {
		return ModelResult{}, err
	}
	var result ModelResult
	for {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case event, ok := <-stream:
			if !ok {
				return result, &PartialStreamError{Partial: result.Message}
			}
			switch event.Type {
			case StreamEventError:
				if event.Message.Role != "" || len(event.Message.Content) > 0 || event.Message.Usage != nil {
					result.Message = event.Message
				}
				if event.Err == nil {
					event.Err = &PartialStreamError{Partial: result.Message}
				}
				return result, event.Err
			case StreamEventDone:
				result.Message = event.Message
				result.Message.Timestamp = time.Now()
				return result, nil
			default:
				result.Message = event.Message
				if event.CompletedToolCall != nil {
					result.HasCompletedToolCalls = true
				}
				observe(event)
			}
		}
	}
}
