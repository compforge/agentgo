package agentgo

import "context"

// ModelCall describes one physical model attempt. ID identifies the logical
// model call and remains stable across retries and recovery of the same turn.
// It is scoped to an agent run, is opaque to callers, and is not a provider
// request ID. TurnIndex and Attempt are one-based.
type ModelCall struct {
	ID        string
	TurnIndex int
	Attempt   int
	Request   LLMRequest // read-only snapshot of the projected provider request
	Options   []CallOption
}

// ModelResult is the complete outcome consumed by the loop. Middleware may
// return a known result without invoking next.
type ModelResult struct {
	Message               Message
	HasCompletedToolCalls bool
}

type ModelExecuteFunc func(context.Context, ModelCall) (ModelResult, error)

// ModelMiddleware wraps one physical provider attempt. It may adjust call
// options, observe next's result, or return a known result without calling next.
type ModelMiddleware func(context.Context, ModelCall, ModelExecuteFunc) (ModelResult, error)

func buildModelMiddlewareChain(exec ModelExecuteFunc, middlewares []ModelMiddleware) ModelExecuteFunc {
	for i := len(middlewares) - 1; i >= 0; i-- {
		middleware := middlewares[i]
		next := exec
		exec = func(ctx context.Context, call ModelCall) (ModelResult, error) {
			return middleware(ctx, call, next)
		}
	}
	return exec
}
