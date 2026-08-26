package agentgo

import "context"

// RunKind identifies the stateful Agent entry path that is preparing a run.
type RunKind string

const (
	RunKindPrompt   RunKind = "prompt"
	RunKindContinue RunKind = "continue"
	RunKindInject   RunKind = "inject"
)

// BeforeRunContext describes a stateful Agent before it accepts a run. Input
// contains the new prompt or injected messages that have not entered Snapshot.
type BeforeRunContext struct {
	Kind     RunKind
	Snapshot AgentSnapshot
	Input    []AgentMessage
}

// AfterRunContext describes a stateful Agent after its Loop state has been
// projected and before terminal listeners may start another run.
type AfterRunContext struct {
	Kind     RunKind
	Snapshot AgentSnapshot
	Summary  RunSummary
	Err      error
}

// BeforeRunHook runs synchronously before a stateful Agent accepts a run. The
// returned snapshot becomes the run baseline. Returning an error rejects the
// run, so Prompt or Continue returns that error directly. Agent lifecycle and
// queue mutations are serialized while the hook runs; use the supplied
// snapshot instead of re-entering those mutating methods on the same Agent.
type BeforeRunHook func(context.Context, BeforeRunContext) (AgentSnapshot, error)

// AfterRunHook runs once for every accepted stateful Agent run, including
// failed, cancelled and zero-turn runs. Returning an error changes the
// terminal result to error. The same non-reentrancy rule as BeforeRunHook
// applies to lifecycle and queue mutations.
type AfterRunHook func(context.Context, AfterRunContext) error
