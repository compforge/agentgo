package agentgo

import "context"

// CompactReason identifies why context compaction was requested.
type CompactReason string

const (
	CompactReasonManual    CompactReason = "manual"
	CompactReasonOverflow  CompactReason = "overflow"
	CompactReasonThreshold CompactReason = "threshold"
)

// CompactionInfo describes one completed context compaction transaction.
// It reports the aggregate effect of the configured compactor; individual
// strategies remain an implementation detail. A nil Compaction means no
// compaction changed the context.
type CompactionInfo struct {
	Reason         CompactReason
	Committed      bool
	TokensBefore   int
	TokensAfter    int
	MessagesBefore int
	MessagesAfter  int
	Summarized     bool
}

// ContextProjection is the prompt view projected for a single LLM call.
// By default the projection does not modify the runtime message baseline.
// When ShouldCommit is true, CommitMessages should replace the runtime
// baseline before continuing the current call.
type ContextProjection struct {
	Messages       []AgentMessage
	Usage          *ContextUsage
	CommitMessages []AgentMessage
	ShouldCommit   bool
	Compaction     *CompactionInfo
}

// ContextSnapshot describes both the runtime baseline and the current active
// context view, including its identifiable Items, plus the most recent rewrite
// details remembered by the manager.
//
// Snapshot is meant for debugging, observability, and UI surfaces such as
// /context. BaselineUsage always reflects the caller's current runtime message
// baseline. Usage reports the active view currently remembered by the manager,
// which may be the baseline runtime messages, a projected prompt view, or a
// recovered/committed view depending on the most recent operation.
type ContextSnapshot struct {
	Items              []ContextItem
	BaselineUsage      *ContextUsage
	Usage              *ContextUsage
	Scope              string
	TranscriptMessages int
	ActiveMessages     int
	SummaryMessages    int
	ToolMessages       int
	ClearedToolResults int
	TrimmedTextBlocks  int
	LastChanged        bool
	LastCompactedCount int
	LastKeptCount      int
	LastSplitTurn      bool
}

// ContextCommitResult is the result of an explicit committed rewrite.
// The returned Messages should replace the runtime baseline when Changed is
// true, for example after a manual /compact command.
type ContextCommitResult struct {
	Messages       []AgentMessage
	Usage          *ContextUsage
	Changed        bool
	Compaction     *CompactionInfo
	CompactedCount int
	KeptCount      int
	SplitTurn      bool
}

// ContextRecoveryResult is the result of overflow recovery.
//
// View is always the retryable prompt view. CommitMessages is optional and,
// when ShouldCommit is true, should replace the runtime message baseline so
// future usage reporting and turns start from the recovered state.
type ContextRecoveryResult struct {
	View           []AgentMessage
	CommitMessages []AgentMessage
	Usage          *ContextUsage
	Changed        bool
	ShouldCommit   bool
	Compaction     *CompactionInfo
	CompactedCount int
	KeptCount      int
	SplitTurn      bool
}

// ContextManager owns prompt projection, committed rewrites, overflow
// recovery, and usage reporting for long-running agent sessions.
//
// The manager deliberately distinguishes between transient prompt projection
// and explicit baseline rewrites:
//   - Project builds a prompt view for one LLM call without committing it.
//   - Compact performs an explicit committed rewrite such as /compact.
//   - RecoverOverflow produces a retryable prompt view after context overflow
//     and may optionally return a new committed baseline.
//   - Sync updates the manager with the current runtime baseline after
//     external message replacement, session restore, or clear.
//   - Usage reports the latest effective usage remembered by the manager.
//   - Snapshot reports the current active view and recent rewrite details for
//     debugging and UI surfaces.
type ContextManager interface {
	// Project builds the prompt view for a single model call without mutating
	// the caller's runtime baseline.
	Project(ctx context.Context, msgs []AgentMessage) (ContextProjection, error)

	// Compact performs an explicit committed rewrite of msgs. The caller is
	// responsible for replacing its runtime baseline with the returned Messages
	// when Changed is true.
	Compact(ctx context.Context, msgs []AgentMessage, reason CompactReason) (ContextCommitResult, error)

	// RecoverOverflow produces a retryable view after a provider reports
	// context overflow. When ShouldCommit is true, CommitMessages should replace
	// the runtime baseline before continuing.
	RecoverOverflow(ctx context.Context, msgs []AgentMessage, cause error) (ContextRecoveryResult, error)

	// Sync tells the manager what the current runtime baseline is after restore,
	// clear, import, or any other external replacement of messages.
	Sync(msgs []AgentMessage)

	// Usage returns the latest effective context usage remembered by the
	// manager. It may reflect a projected or recovered view rather than the raw
	// runtime baseline.
	Usage() *ContextUsage

	// Snapshot returns the latest active view snapshot remembered by the
	// manager. It is intended for observability and may be nil before the
	// manager has seen any messages.
	Snapshot() *ContextSnapshot
}

// ContextEstimator is an optional interface a ContextManager can implement
// to provide token estimation. When implemented, NewAgent auto-wires it.
type ContextEstimator interface {
	EstimateContext([]AgentMessage) (tokens, usageTokens, trailingTokens int)
}

// ContextWindowProvider is an optional interface a ContextManager can implement
// to report its configured context window size.
type ContextWindowProvider interface {
	ContextWindow() int
}

// ContextWindowSetter is an optional interface a ContextManager can implement
// to receive window updates pushed through Agent.SetContextWindow. When
// implemented, the agent and its context manager can never disagree on the
// window size after a model hot-switch — without it, callers had to update
// both sides manually and a missed call silently skewed compaction thresholds.
type ContextWindowSetter interface {
	SetContextWindow(n int)
}
