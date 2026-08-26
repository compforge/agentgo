package agentgo

import "context"

// ModelExecution describes one physical model attempt. Execution.ID
// identifies the logical model call and remains stable across retries and
// recovery of the same turn. Request and Options may be adjusted by
// middleware; the embedded Execution coordinate must be preserved.
type ModelExecution struct {
	Execution
	Request LLMRequest
	Options []CallOption
}

// ModelResult is the complete outcome consumed by the loop. Middleware may
// return a known result without invoking next.
type ModelResult struct {
	Message               Message
	HasCompletedToolCalls bool
}

type ModelExecuteFunc func(context.Context, ModelExecution) (ModelResult, error)

// ModelMiddleware wraps one physical provider attempt. It may adjust call
// options, observe next's result, or return a known result without calling next.
type ModelMiddleware func(context.Context, ModelExecution, ModelExecuteFunc) (ModelResult, error)

type modelExecutionRuntime struct {
	middlewares []ModelMiddleware
	emit        func(Event)
}

type modelExecutionRuntimeKey struct{}

func withModelExecutionRuntime(ctx context.Context, middlewares []ModelMiddleware, emit func(Event)) context.Context {
	runtime := modelExecutionRuntime{
		middlewares: append([]ModelMiddleware(nil), middlewares...),
		emit:        emit,
	}
	return context.WithValue(ctx, modelExecutionRuntimeKey{}, runtime)
}

// ExecuteModel runs one model attempt through the execution runtime installed
// by AgentLoop. Internal model consumers such as context summarization use the
// same entry so model middleware and execution events cover them as well. When
// called outside AgentLoop it simply invokes next.
func ExecuteModel(ctx context.Context, execution ModelExecution, next ModelExecuteFunc) (ModelResult, error) {
	runtime, _ := ctx.Value(modelExecutionRuntimeKey{}).(modelExecutionRuntime)
	if runtime.emit != nil {
		runtime.emit(Event{Type: EventModelExecStart, Execution: executionRef(execution.Execution)})
	}

	execute := next
	if len(runtime.middlewares) > 0 {
		execute = buildModelMiddlewareChain(execute, runtime.middlewares)
	}
	result, err := execute(ctx, execution)

	if runtime.emit != nil {
		runtime.emit(Event{
			Type:      EventModelExecEnd,
			Execution: executionRef(execution.Execution),
			Message:   result.Message,
			Err:       err,
		})
	}
	return result, err
}

func buildModelMiddlewareChain(exec ModelExecuteFunc, middlewares []ModelMiddleware) ModelExecuteFunc {
	for i := len(middlewares) - 1; i >= 0; i-- {
		middleware := middlewares[i]
		next := exec
		exec = func(ctx context.Context, call ModelExecution) (ModelResult, error) {
			return middleware(ctx, call, next)
		}
	}
	return exec
}
