package agentgo

import (
	"context"
	"time"
)

// ModelAttempt describes one routed provider call within a model Execution.
// Index is local to that execution attempt; it never replaces Execution.Attempt.
// Endpoint is an application-supplied safe label, not a URL or credential.
type ModelAttempt struct {
	Index        int           `json:"index"`
	Endpoint     string        `json:"endpoint"`
	Model        string        `json:"model,omitempty"`
	Duration     time.Duration `json:"duration_ns,omitempty"`
	TimeoutScope string        `json:"timeout_scope,omitempty"` // caller, routing, endpoint
	ErrorKind    string        `json:"error_kind,omitempty"`
	WillFailover bool          `json:"will_failover,omitempty"`
	Published    bool          `json:"published,omitempty"`
	Usage        *Usage        `json:"usage,omitempty"`
}

// ReportModelAttempt publishes a routed attempt to the enclosing AgentLoop.
// Outside a loop it is a no-op; routers may additionally offer a direct observer.
func ReportModelAttempt(ctx context.Context, attempt ModelAttempt, err error) {
	runtime, _ := ctx.Value(modelExecutionRuntimeKey{}).(modelExecutionRuntime)
	if runtime.emit == nil {
		return
	}
	execution, ok := ExecutionFromContext(ctx)
	var ref *Execution
	if ok {
		ref = executionRef(execution)
	}
	runtime.emit(Event{Type: EventModelAttempt, Execution: ref, ModelAttempt: &attempt, Err: err})
}
