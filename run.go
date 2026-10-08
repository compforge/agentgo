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
	Artifacts ArtifactManager // Runtime-owned material; do not retain beyond the callback.
	Kind      RunKind
	Snapshot  AgentSnapshot
	Input     []AgentMessage
}

// AfterRunContext describes a stateful Agent after its Loop state has been
// projected and before terminal listeners may start another run.
type AfterRunContext struct {
	Artifacts ArtifactManager // Runtime-owned material; do not retain beyond the callback.
	Kind      RunKind
	Snapshot  AgentSnapshot
	Summary   RunSummary
	Err       error
}

// BeforeRunHook runs synchronously before a stateful Agent accepts a run. The
// returned snapshot becomes the run baseline. Returning an error rejects the
// run, so Prompt or Continue returns that error directly. Agent lifecycle and
// queue mutations are serialized while the hook runs; use the supplied
// snapshot instead of re-entering those mutating methods on the same Agent.
// The returned snapshot is the restore baseline; successful AddArtifact calls
// and DeleteArtifact calls made through Artifacts take precedence over that
// baseline. Rejected admission rolls back membership changes, not mutations of
// application-owned payloads. Manager reads during the hook reflect the current
// working collection, not a snapshot that the hook has yet to return.
type BeforeRunHook func(context.Context, BeforeRunContext) (AgentSnapshot, error)

// AfterRunHook runs once for every accepted stateful Agent run, including
// failed, cancelled and zero-turn runs. Returning an error changes the
// terminal result to error. The same non-reentrancy rule as BeforeRunHook
// applies to lifecycle and queue mutations.
type AfterRunHook func(context.Context, AfterRunContext) error
