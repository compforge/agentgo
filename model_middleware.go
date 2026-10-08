package agentgo

import (
	"context"
	"fmt"
)

// ModelExecution describes one physical model attempt. Execution.ID
// identifies the logical model call and remains stable across retries and
// recovery of the same turn. Request and Options may be adjusted by
// middleware; the embedded Execution coordinate must be preserved.
type ModelExecution struct {
	Artifacts ArtifactManager // Runtime-owned material; do not retain beyond the callback.
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
// The first middleware is outermost. Call next at most once: the loop owns
// retries and their Attempt coordinates. A derived context passed to next
// reaches the provider. Returning an error fails this attempt; diagnostics
// adapters must handle their own recording errors without returning them.
// Concurrent tools can invoke this middleware concurrently; keep per-call state local.
type ModelMiddleware func(context.Context, ModelExecution, ModelExecuteFunc) (ModelResult, error)

type modelExecutionRuntime struct {
	middlewares []ModelMiddleware
	artifacts   ArtifactManager
	emit        func(Event)
}

type modelExecutionRuntimeKey struct{}

func withModelExecutionRuntime(ctx context.Context, middlewares []ModelMiddleware, emit func(Event), artifacts ArtifactManager) context.Context {
	runtime := modelExecutionRuntime{
		artifacts:   artifacts,
		middlewares: append([]ModelMiddleware(nil), middlewares...),
		emit:        emit,
	}
	return context.WithValue(ctx, modelExecutionRuntimeKey{}, runtime)
}

// ExecuteModel runs one model attempt through the execution runtime installed
// by AgentLoop. Internal model consumers such as context summarization use the
// same entry so model middleware and execution events cover them as well. When
// called outside AgentLoop it invokes next without middleware or events. The
// current execution is available through ExecutionFromContext; an omitted
// ParentID inherits the enclosing execution. Panics become execution errors.
// Nested calls must finish before their owning tool or context operation returns;
// the runtime and event sink have the lifetime of the enclosing AgentLoop.
func ExecuteModel(ctx context.Context, execution ModelExecution, next ModelExecuteFunc) (result ModelResult, err error) {
	runtime, _ := ctx.Value(modelExecutionRuntimeKey{}).(modelExecutionRuntime)
	if runtime.artifacts != nil {
		execution.Artifacts = runtime.artifacts
	}
	if parent, ok := ExecutionFromContext(ctx); ok && execution.ParentID == "" && parent.ID != execution.ID {
		execution.ParentID = parent.ID
	}
	ctx = ContextWithExecution(ctx, execution.Execution)
	if runtime.emit != nil {
		runtime.emit(Event{Type: EventModelExecStart, Execution: executionRef(execution.Execution)})
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			result, err = ModelResult{}, fmt.Errorf("model execution %s panicked: %v", execution.ID, recovered)
		}
		if runtime.emit != nil {
			runtime.emit(Event{Type: EventModelExecEnd, Execution: executionRef(execution.Execution), Message: result.Message, Err: err})
		}
	}()
	execute := next
	if len(runtime.middlewares) > 0 {
		execute = buildModelMiddlewareChain(execute, runtime.middlewares)
	}
	return execute(ctx, execution)
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
