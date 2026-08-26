package agentgo

import "context"

// ExecutionKind identifies an AgentGo-owned execution boundary. Values are
// intentionally independent of persistence systems; adapters decide how an
// execution maps to their own action, attempt, or trace models.
type ExecutionKind string

const (
	ExecutionKindModel   ExecutionKind = "model"
	ExecutionKindTool    ExecutionKind = "tool"
	ExecutionKindCompact ExecutionKind = "compact"
)

// Execution is the stable coordinate shared by middleware and events for one
// logical execution. ID remains stable across physical retries while Attempt
// increases from one. IDs are scoped to one AgentGo run; hosts own any wider
// session or run identity.
type Execution struct {
	ID        string
	Kind      ExecutionKind
	ParentID  string
	TurnIndex int
	Attempt   int
}

// WithAttempt returns the same logical execution at a physical attempt.
func (e Execution) WithAttempt(attempt int) Execution {
	e.Attempt = attempt
	return e
}

type executionContextKey struct{}

// ContextWithExecution associates nested work with its parent execution. It
// only propagates identity; it does not change cancellation or execution
// behavior.
func ContextWithExecution(ctx context.Context, execution Execution) context.Context {
	return context.WithValue(ctx, executionContextKey{}, execution)
}

// ExecutionFromContext returns the nearest parent execution when one was
// installed by AgentGo or the caller.
func ExecutionFromContext(ctx context.Context) (Execution, bool) {
	execution, ok := ctx.Value(executionContextKey{}).(Execution)
	return execution, ok
}

func executionRef(execution Execution) *Execution {
	copy := execution
	return &copy
}
