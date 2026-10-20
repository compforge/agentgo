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

// ReportModelAttempt publishes a routed attempt to the active model call.
// With no observer it is a no-op; routers may additionally offer a direct observer.
func ReportModelAttempt(ctx context.Context, attempt ModelAttempt, err error) {
	if report, ok := ctx.Value(modelAttemptReporterKey{}).(func(ModelAttempt, error)); ok {
		report(attempt, err)
	}
}
