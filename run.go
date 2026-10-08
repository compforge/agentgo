package agentgo

import "context"

// RunKind identifies the stateful Agent entry path that is preparing a run.
type RunKind string

const (
	RunKindPrompt   RunKind = "prompt"
	RunKindContinue RunKind = "continue"
	RunKindInject   RunKind = "inject"
)

// SnapshotLoadContext describes the accepted baseline before run preparation.
// SnapshotLoader may return a recovered snapshot or retain this baseline.
type SnapshotLoadContext struct {
	Kind     RunKind
	Snapshot AgentSnapshot
}

// SnapshotLoader loads the baseline before BeforeRun initializes its materials.
// It runs once per run admission, outside the Loop. Errors reject admission;
// neither the recovered snapshot nor initialization is published until both
// stages succeed. Like BeforeRun, it must not re-enter lifecycle or queue
// mutation methods on the same Agent.
type SnapshotLoader func(context.Context, SnapshotLoadContext) (AgentSnapshot, error)

// BeforeRunContext describes a staged, recovered baseline before run admission.
// Snapshot is an observation of that baseline; use Artifacts to change materials.
// Input contains new prompt or injected messages not yet included in Snapshot.
type BeforeRunContext struct {
	Artifacts ArtifactManager // Staged material; do not retain beyond the callback.
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

// BeforeRunHook initializes material once after snapshot loading and before the
// Agent accepts a run. CRUD operates on the recovered baseline in an isolated
// collection. Success publishes the prepared messages and materials together;
// an error discards preparation and is returned by Prompt or Continue directly.
// Application-owned payload objects remain shared: publish replacements rather
// than mutate them in place when rejection must leave their contents unchanged.
// Lifecycle and queue mutations are serialized during preparation; observe the
// supplied Snapshot instead of re-entering mutating methods on the same Agent.
type BeforeRunHook func(context.Context, BeforeRunContext) error

// AfterRunHook runs once for every accepted stateful Agent run, including
// failed, cancelled and zero-turn runs. Returning an error changes the
// terminal result to error. The same non-reentrancy rule as BeforeRunHook
// applies to lifecycle and queue mutations.
type AfterRunHook func(context.Context, AfterRunContext) error
