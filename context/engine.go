// Package context provides message-native context compression for agentgo:
// prompt projection, summary checkpoints, overflow recovery, and usage estimation.
//
//	engine := context.NewDefaultEngine(model, 128000)
//	agent := agentgo.NewAgent(
//		agentgo.WithModel(model),
//		agentgo.WithContextManager(engine),
//	)
package context

import (
	"context"
	"sync"

	"github.com/compforge/agentgo"
)

const (
	// Default headroom scales with the window up to the previous 16K limit.
	defaultReserveRatio           = 0.13
	minEngineReserveTokens        = 4096
	maxEngineReserveTokens        = 16384
	defaultMaxConsecutiveFailures = 3
)

// EngineConfig configures ContextEngine. ContextWindow and Compactor are the
// only required capabilities for automatic projection.
type EngineConfig struct {
	// ContextWindow is the model's maximum supported context window.
	ContextWindow int
	// ReserveTokens is the minimum prompt headroom to preserve. When zero, a
	// conservative default is used.
	ReserveTokens int
	// Compactor owns the complete reduction policy. The engine only computes
	// the aggregate target ratio and validates the resulting size.
	Compactor Compactor
	// CommitOnProject makes threshold-triggered projections replace the runtime
	// baseline. Explicit Compact and overflow recovery always commit changes.
	CommitOnProject bool
	// OnProject is called when Project rewrites the prompt view.
	OnProject func(RewriteEvent)
	// OnRecover is called when RecoverOverflow rewrites the prompt view.
	OnRecover func(RewriteEvent)
	// MaxConsecutiveFailures is the circuit breaker threshold. After this many
	// consecutive Project failures, the engine skips compression and returns
	// the original messages to avoid wasting API calls. 0 = default (3).
	MaxConsecutiveFailures int
}

// RewriteEvent reports a projection or recovery rewrite that actually changed
// the active view. Info is populated when the rewrite produced a summary.
type RewriteEvent struct {
	Reason       string
	Changed      bool
	Committed    bool
	TokensBefore int
	TokensAfter  int
	Info         *SummaryInfo
	// View is what the rewrite produced. Carried on the event because the hook
	// fires before the loop installs a committed view: a host that reacts to
	// Committed by reading the agent's messages would still see the old
	// baseline.
	View []agentgo.AgentMessage
	// Failures is set when Reason == "circuit_breaker": the consecutive failure
	// count that triggered the bypass.
	Failures int
}

// ContextEngine implements agentgo.ContextManager with one replaceable
// compaction policy.
type ContextEngine struct {
	cfg EngineConfig

	mu         sync.Mutex
	transcript []agentgo.AgentMessage
	baseline   *agentgo.ContextUsage
	lastUsage  *agentgo.ContextUsage
	lastView   []agentgo.AgentMessage
	lastScope  string
	lastChange changeState

	// Circuit breaker: consecutive Project failures. Reset on success.
	consecutiveFailures int
	maxFailures         int
}

type changeState struct {
	changed bool
	info    *SummaryInfo
}

// NewEngine constructs a ContextEngine from an explicit compactor.
func NewEngine(cfg EngineConfig) *ContextEngine {
	maxFail := cfg.MaxConsecutiveFailures
	if maxFail <= 0 {
		maxFail = defaultMaxConsecutiveFailures
	}
	engine := &ContextEngine{cfg: cfg, maxFailures: maxFail}
	engine.syncCompactorWindow()
	return engine
}

// NewDefaultEngine creates a ContextEngine with the default compactor: domain
// message reduction, cheap generic projections, then full summary.
//
// This is the recommended entry point for applications that want context
// management without custom compactor wiring.
//
//	engine := context.NewDefaultEngine(model, 128000)
//	agentgo.WithContextManager(engine)
func NewDefaultEngine(model agentgo.ChatModel, contextWindow int) *ContextEngine {
	reserve := defaultEngineReserve(contextWindow)
	return NewEngine(EngineConfig{
		ContextWindow: contextWindow,
		Compactor: Chain(
			NewMessageCompactor(),
			NewToolResultCompactor(ToolResultMicrocompactConfig{}),
			NewLightTrimCompactor(LightTrimConfig{}),
			NewSummaryCompactor(FullSummaryConfig{
				Model:         model,
				ContextWindow: contextWindow,
				ReserveTokens: reserve,
			}),
		),
	})
}

// SetProjectHook installs the callback fired when Project rewrites the prompt
// view due to context pressure.
func (e *ContextEngine) SetProjectHook(fn func(RewriteEvent)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg.OnProject = fn
}

// SetRecoverHook installs the callback fired when RecoverOverflow rewrites the
// prompt view after a provider overflow error.
func (e *ContextEngine) SetRecoverHook(fn func(RewriteEvent)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg.OnRecover = fn
}

// Sync records msgs as the current runtime baseline and resets the active view
// remembered by the engine to that baseline.
func (e *ContextEngine) Sync(msgs []agentgo.AgentMessage) {
	usage := e.estimateUsage(msgs)

	e.mu.Lock()
	defer e.mu.Unlock()
	e.transcript = copyMessages(msgs)
	e.lastView = copyMessages(msgs)
	e.lastScope = "baseline"
	cp := usage
	e.baseline = &cp
	e.lastUsage = &cp
}

// Usage returns the latest effective usage remembered by the engine.
func (e *ContextEngine) Usage() *agentgo.ContextUsage {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastUsage == nil {
		return nil
	}
	cp := *e.lastUsage
	return &cp
}

// ConsecutiveFailures returns the current circuit breaker failure count.
func (e *ContextEngine) ConsecutiveFailures() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.consecutiveFailures
}

// Snapshot returns the latest active view snapshot remembered by the engine.
func (e *ContextEngine) Snapshot() *agentgo.ContextSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastUsage == nil && len(e.lastView) == 0 && len(e.transcript) == 0 {
		return nil
	}

	var baseline *agentgo.ContextUsage
	if e.baseline != nil {
		cp := *e.baseline
		baseline = &cp
	}
	var usage *agentgo.ContextUsage
	if e.lastUsage != nil {
		cp := *e.lastUsage
		usage = &cp
	}
	counts := summarizeContextView(e.lastView)
	return &agentgo.ContextSnapshot{
		Items:              agentgo.CollectContextItems(e.lastView),
		BaselineUsage:      baseline,
		Usage:              usage,
		Scope:              e.lastScope,
		TranscriptMessages: len(e.transcript),
		ActiveMessages:     len(e.lastView),
		SummaryMessages:    counts.summaryMessages,
		ToolMessages:       counts.toolMessages,
		ClearedToolResults: counts.clearedToolResults,
		TrimmedTextBlocks:  counts.trimmedTextBlocks,
		LastChanged:        e.lastChange.changed,
		LastCompactedCount: infoValueInt(e.lastChange.info, func(i *SummaryInfo) int { return i.CompactedCount }),
		LastKeptCount:      infoValueInt(e.lastChange.info, func(i *SummaryInfo) int { return i.KeptCount }),
		LastSplitTurn:      infoValueBool(e.lastChange.info, func(i *SummaryInfo) bool { return i.IsSplitTurn }),
	}
}

// Project builds the prompt view for one LLM call without committing a new
// runtime baseline. Includes a circuit breaker: after maxFailures consecutive
// compression errors, Project skips one cycle, reports the skipped state, then
// re-arms itself in a half-open state so later calls can retry compression.
func (e *ContextEngine) Project(ctx context.Context, msgs []agentgo.AgentMessage) (agentgo.ContextProjection, error) {
	e.Sync(msgs)

	// Circuit breaker: skip compression after too many consecutive failures.
	// Unlike a silent bypass, we still fire OnProject so the host can observe
	// and display the skipped state.
	e.mu.Lock()
	tripped := e.consecutiveFailures >= e.maxFailures
	failures := e.consecutiveFailures
	e.mu.Unlock()
	if tripped {
		usage := ptrUsage(e.estimateUsage(msgs))
		e.setLastState(msgs, usage, "skipped", false, nil)
		e.mu.Lock()
		if e.maxFailures > 1 {
			e.consecutiveFailures = e.maxFailures - 1
		} else {
			e.consecutiveFailures = 0
		}
		e.mu.Unlock()
		if e.cfg.OnProject != nil {
			e.cfg.OnProject(RewriteEvent{
				Reason:       "circuit_breaker",
				Changed:      false,
				Committed:    false,
				TokensBefore: EstimateTotal(msgs),
				TokensAfter:  EstimateTotal(msgs),
				Failures:     failures,
			})
		}
		return agentgo.ContextProjection{Messages: msgs, Usage: usage}, nil
	}

	r, err := e.apply(ctx, msgs, false)
	if err != nil {
		e.mu.Lock()
		e.consecutiveFailures++
		e.mu.Unlock()
		return agentgo.ContextProjection{}, err
	}

	// Reset on successful compression.
	if r.Changed {
		e.mu.Lock()
		e.consecutiveFailures = 0
		e.mu.Unlock()
	}
	compaction := newCompactionInfo(agentgo.CompactReasonThreshold, e.cfg.CommitOnProject, msgs, r)
	if r.Changed && e.cfg.OnProject != nil {
		e.cfg.OnProject(RewriteEvent{
			Reason:       string(compaction.Reason),
			Changed:      true,
			Committed:    compaction.Committed,
			TokensBefore: compaction.TokensBefore,
			TokensAfter:  compaction.TokensAfter,
			Info:         r.Info,
			View:         r.View,
		})
	}
	proj := agentgo.ContextProjection{
		Messages:   r.View,
		Usage:      r.Usage,
		Compaction: compaction,
	}
	if r.Changed && e.cfg.CommitOnProject {
		proj.CommitMessages = copyMessages(r.View)
		proj.ShouldCommit = true
	}
	return proj, nil
}

// Compact performs a forced rewrite suitable for explicit committed actions
// such as /compact. The caller should replace its runtime baseline with the
// returned Messages when Changed is true.
func (e *ContextEngine) Compact(ctx context.Context, msgs []agentgo.AgentMessage, reason agentgo.CompactReason) (agentgo.ContextCommitResult, error) {
	e.Sync(msgs)
	r, err := e.apply(ctx, msgs, true)
	if err != nil {
		return agentgo.ContextCommitResult{}, err
	}
	return agentgo.ContextCommitResult{
		Messages:       r.View,
		Usage:          r.Usage,
		Changed:        r.Changed,
		Compaction:     newCompactionInfo(reason, true, msgs, r),
		CompactedCount: infoValueInt(r.Info, func(i *SummaryInfo) int { return i.CompactedCount }),
		KeptCount:      infoValueInt(r.Info, func(i *SummaryInfo) int { return i.KeptCount }),
		SplitTurn:      infoValueBool(r.Info, func(i *SummaryInfo) bool { return i.IsSplitTurn }),
	}, nil
}

// RecoverOverflow performs a forced rewrite after a provider reports context
// overflow. When ShouldCommit is true, CommitMessages should become the new
// runtime baseline before retrying.
func (e *ContextEngine) RecoverOverflow(ctx context.Context, msgs []agentgo.AgentMessage, _ error) (agentgo.ContextRecoveryResult, error) {
	e.Sync(msgs)
	r, err := e.apply(ctx, msgs, true)
	if err != nil {
		return agentgo.ContextRecoveryResult{}, err
	}
	e.setLastState(r.View, r.Usage, "recovered", r.Changed, r.Info)
	// Successful recovery proves the LLM is reachable — reset circuit breaker.
	if r.Changed {
		e.mu.Lock()
		e.consecutiveFailures = 0
		e.mu.Unlock()
	}
	compaction := newCompactionInfo(agentgo.CompactReasonOverflow, true, msgs, r)
	if r.Changed && e.cfg.OnRecover != nil {
		e.cfg.OnRecover(RewriteEvent{
			Reason:       string(compaction.Reason),
			Changed:      true,
			Committed:    compaction.Committed,
			TokensBefore: compaction.TokensBefore,
			TokensAfter:  compaction.TokensAfter,
			Info:         r.Info,
			View:         r.View,
		})
	}
	return agentgo.ContextRecoveryResult{
		View:           r.View,
		CommitMessages: r.View,
		Usage:          r.Usage,
		Changed:        r.Changed,
		ShouldCommit:   r.Changed,
		Compaction:     compaction,
		CompactedCount: infoValueInt(r.Info, func(i *SummaryInfo) int { return i.CompactedCount }),
		KeptCount:      infoValueInt(r.Info, func(i *SummaryInfo) int { return i.KeptCount }),
		SplitTurn:      infoValueBool(r.Info, func(i *SummaryInfo) bool { return i.IsSplitTurn }),
	}, nil
}

type applyResult struct {
	View    []agentgo.AgentMessage
	Usage   *agentgo.ContextUsage
	Changed bool
	Info    *SummaryInfo
}

func newCompactionInfo(reason agentgo.CompactReason, committed bool, before []agentgo.AgentMessage, result applyResult) *agentgo.CompactionInfo {
	if !result.Changed {
		return nil
	}
	return &agentgo.CompactionInfo{
		Reason:         reason,
		Committed:      committed,
		TokensBefore:   EstimateTotal(before),
		TokensAfter:    EstimateTotal(result.View),
		MessagesBefore: len(before),
		MessagesAfter:  len(result.View),
		Summarized:     result.Info != nil,
	}
}

func (e *ContextEngine) apply(ctx context.Context, msgs []agentgo.AgentMessage, force bool) (applyResult, error) {
	view := copyMessages(msgs)
	before := EstimateContextTokens(view).Tokens
	if e.cfg.Compactor == nil {
		usage := ptrUsage(e.estimateUsage(view))
		e.setLastState(view, usage, scopeFor(force), false, nil)
		return applyResult{View: view, Usage: usage}, nil
	}

	expect := 0.0
	if !force {
		window := e.cfg.ContextWindow
		threshold := e.threshold(window)
		if window <= 0 || before <= threshold {
			usage := ptrUsage(e.estimateUsage(view))
			e.setLastState(view, usage, scopeFor(false), false, nil)
			return applyResult{View: view, Usage: usage}, nil
		}
		expect = float64(threshold) / float64(before)
	}

	next, err := e.cfg.Compactor.Compact(ctx, view, clampRatio(expect))
	if err != nil {
		return applyResult{}, err
	}
	after := EstimateContextTokens(next).Tokens
	changed := after < before
	if !changed {
		next = view
	}
	info := summaryInfoFromView(next)
	usage := ptrUsage(e.estimateUsage(next))
	e.setLastState(next, usage, scopeFor(force), changed, info)
	return applyResult{View: next, Usage: usage, Changed: changed, Info: info}, nil
}

func (e *ContextEngine) threshold(window int) int {
	return max(0, window-e.reserveTokens(window))
}

func (e *ContextEngine) reserveTokens(window int) int {
	if e.cfg.ReserveTokens > 0 {
		return e.cfg.ReserveTokens
	}
	return defaultEngineReserve(window)
}

func defaultEngineReserve(window int) int {
	return min(maxEngineReserveTokens, max(minEngineReserveTokens, int(float64(window)*defaultReserveRatio)))
}

func (e *ContextEngine) setLastState(view []agentgo.AgentMessage, usage *agentgo.ContextUsage, scope string, changed bool, info *SummaryInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastView = copyMessages(view)
	e.lastScope = scope
	if usage == nil {
		e.lastUsage = nil
	} else {
		cp := *usage
		e.lastUsage = &cp
	}
	if changed || info != nil {
		e.lastChange = changeState{
			changed: changed,
			info:    copySummaryInfo(info),
		}
	}
}

func (e *ContextEngine) estimateUsage(msgs []agentgo.AgentMessage) agentgo.ContextUsage {
	estimate := EstimateContextTokens(msgs)
	window := e.cfg.ContextWindow
	pct := 0.0
	if window > 0 {
		pct = float64(estimate.Tokens) / float64(window) * 100
	}
	return agentgo.ContextUsage{
		Tokens:         estimate.Tokens,
		ContextWindow:  window,
		Percent:        pct,
		UsageTokens:    estimate.UsageTokens,
		TrailingTokens: estimate.TrailingTokens,
	}
}

func ptrUsage(usage agentgo.ContextUsage) *agentgo.ContextUsage {
	return &usage
}

func scopeFor(force bool) string {
	if force {
		return "committed"
	}
	return "projected"
}

func infoValueInt(info *SummaryInfo, getter func(*SummaryInfo) int) int {
	if info == nil {
		return 0
	}
	return getter(info)
}

func infoValueBool(info *SummaryInfo, getter func(*SummaryInfo) bool) bool {
	if info == nil {
		return false
	}
	return getter(info)
}

type contextViewCounts struct {
	summaryMessages    int
	toolMessages       int
	clearedToolResults int
	trimmedTextBlocks  int
}

func summarizeContextView(msgs []agentgo.AgentMessage) contextViewCounts {
	var counts contextViewCounts
	for _, am := range msgs {
		if _, ok := am.(ContextSummary); ok {
			counts.summaryMessages++
		}
		msg, include := am.ToMessage()
		if !include {
			continue
		}
		if msg.Role == agentgo.RoleTool {
			counts.toolMessages++
		}
		if compacted, _ := msg.Metadata["compacted_tool_result"].(bool); compacted {
			counts.clearedToolResults++
		}
		switch v := msg.Metadata["trimmed_text_blocks"].(type) {
		case int:
			counts.trimmedTextBlocks += v
		case int32:
			counts.trimmedTextBlocks += int(v)
		case int64:
			counts.trimmedTextBlocks += int(v)
		case float64:
			counts.trimmedTextBlocks += int(v)
		}
	}
	return counts
}

func summaryInfoFromView(msgs []agentgo.AgentMessage) *SummaryInfo {
	for _, message := range msgs {
		summary, ok := message.(ContextSummary)
		if !ok {
			continue
		}
		return &SummaryInfo{
			TokensBefore:   summary.TokensBefore,
			TokensAfter:    EstimateTotal(msgs),
			CompactedCount: summary.Compacted,
			KeptCount:      summary.Kept,
			IsSplitTurn:    summary.SplitTurn,
			ReadFiles:      append([]string(nil), summary.ReadFiles...),
			ModifiedFiles:  append([]string(nil), summary.ModifiedFiles...),
		}
	}
	return nil
}

func copySummaryInfo(info *SummaryInfo) *SummaryInfo {
	if info == nil {
		return nil
	}
	cp := *info
	cp.ReadFiles = append([]string(nil), info.ReadFiles...)
	cp.ModifiedFiles = append([]string(nil), info.ModifiedFiles...)
	return &cp
}

func copyMessages(msgs []agentgo.AgentMessage) []agentgo.AgentMessage {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]agentgo.AgentMessage, len(msgs))
	copy(out, msgs)
	return out
}

func cloneMetadata(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// EstimateContext implements agentgo.ContextEstimator.
func (e *ContextEngine) EstimateContext(msgs []agentgo.AgentMessage) (tokens, usageTokens, trailingTokens int) {
	return ContextEstimateAdapter(msgs)
}

// SetContextWindow updates the context window size used for threshold calculations.
func (e *ContextEngine) SetContextWindow(n int) {
	e.mu.Lock()
	e.cfg.ContextWindow = n
	reserve := e.reserveTokens(n)
	compactor := e.cfg.Compactor
	e.mu.Unlock()
	if aware, ok := compactor.(windowAwareCompactor); ok {
		aware.setContextWindow(n, reserve)
	}
}

// SetReserveTokens updates the prompt headroom reserved when computing the
// compaction threshold. Pass 0 to restore the engine's built-in default on the
// next threshold calculation.
func (e *ContextEngine) SetReserveTokens(n int) {
	e.mu.Lock()
	e.cfg.ReserveTokens = n
	window := e.cfg.ContextWindow
	reserve := e.reserveTokens(window)
	compactor := e.cfg.Compactor
	e.mu.Unlock()
	if aware, ok := compactor.(windowAwareCompactor); ok {
		aware.setContextWindow(window, reserve)
	}
}

func (e *ContextEngine) syncCompactorWindow() {
	if aware, ok := e.cfg.Compactor.(windowAwareCompactor); ok {
		aware.setContextWindow(e.cfg.ContextWindow, e.reserveTokens(e.cfg.ContextWindow))
	}
}

// ContextWindow implements agentgo.ContextWindowProvider.
func (e *ContextEngine) ContextWindow() int {
	return e.cfg.ContextWindow
}
