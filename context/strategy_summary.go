package context

import (
	"context"
	"time"

	"github.com/compforge/agentgo"
)

// FullSummaryConfig controls ContextSummary checkpoint generation.
// KeepRecentTokens reserves a recent suffix of messages to keep verbatim.
// PostSummaryHooks may inject lightweight reminder messages after the summary.
type FullSummaryConfig struct {
	// Model performs the actual summary generation.
	Model agentgo.ChatModel
	// ContextWindow and ReserveTokens bound the summarization request. The
	// default engine keeps them aligned with its active model window.
	ContextWindow int
	ReserveTokens int
	// StripImages controls whether images are removed before summarization.
	// Nil defaults to true.
	StripImages *bool
	// KeepRecentTokens reserves a recent suffix to keep verbatim. Zero scales it
	// from the budget instead.
	KeepRecentTokens int
	// PostSummaryHooks inject lightweight reminder messages after the summary.
	PostSummaryHooks []PostSummaryHook

	// Custom summary prompts. Empty strings fall back to the built-in defaults
	// (code-assistant oriented). Set these to override with domain-specific
	// prompts — e.g., novel-writing prompts that preserve narrative continuity.
	SystemPrompt        string
	SummaryPrompt       string
	UpdateSummaryPrompt string
	TurnPrefixPrompt    string
}

// SummaryCompactor rewrites older context into a ContextSummary checkpoint
// while keeping a recent suffix of messages verbatim.
type SummaryCompactor struct {
	cfg FullSummaryConfig
}

const minSummaryReserveTokens = 1024

// NewSummaryCompactor constructs the terminal summary compactor used when lighter
// rewrites are insufficient. Model is required for actual summarization.
func NewSummaryCompactor(cfg FullSummaryConfig) *SummaryCompactor {
	return &SummaryCompactor{cfg: cfg}
}

func (s *SummaryCompactor) setContextWindow(window, reserve int) {
	s.cfg.ContextWindow = window
	s.cfg.ReserveTokens = reserve
}

// keepRecentTokens scales the verbatim tail with the requested output size.
func (s *SummaryCompactor) keepRecentTokens(target int) int {
	if s.cfg.KeepRecentTokens > 0 {
		return s.cfg.KeepRecentTokens
	}
	return min(maxKeepRecentTokens, max(minKeepRecentTokens, target/4))
}

func (s *SummaryCompactor) Compact(ctx context.Context, messages []agentgo.AgentMessage, expect float64) ([]agentgo.AgentMessage, error) {
	if expect >= 1 {
		return messages, nil
	}
	return s.apply(ctx, messages, clampRatio(expect))
}

// SetPostSummaryHooks replaces the hook list used to inject lightweight
// reminder messages after a summary checkpoint is produced.
func (s *SummaryCompactor) SetPostSummaryHooks(hooks ...PostSummaryHook) {
	s.cfg.PostSummaryHooks = hooks
}

func (s *SummaryCompactor) apply(ctx context.Context, messages []agentgo.AgentMessage, expect float64) ([]agentgo.AgentMessage, error) {
	if len(messages) == 0 || s.cfg.Model == nil {
		return messages, nil
	}

	tokens := EstimateTotal(messages)
	target := int(float64(tokens) * expect)
	ctxWindow := s.cfg.ContextWindow
	if ctxWindow <= 0 {
		ctxWindow = max(tokens, 2)
	}
	reserve := s.cfg.ReserveTokens
	if reserve <= 0 {
		reserve = max(1, ctxWindow-target)
	}
	if expect == 0 {
		// A forced compact still needs enough output space for a useful
		// checkpoint. The resulting ratio may therefore be greater than zero.
		reserve = minSummaryReserveTokens
	}

	cfg := summaryRunConfig{
		Model:               s.cfg.Model,
		ContextWindow:       ctxWindow,
		ReserveTokens:       reserve,
		KeepRecentTokens:    s.keepRecentTokens(target),
		SystemPrompt:        s.cfg.SystemPrompt,
		SummaryPrompt:       s.cfg.SummaryPrompt,
		UpdateSummaryPrompt: s.cfg.UpdateSummaryPrompt,
		TurnPrefixPrompt:    s.cfg.TurnPrefixPrompt,
	}
	stripImages := true
	if s.cfg.StripImages != nil {
		stripImages = *s.cfg.StripImages
	}

	// Summarize original domain evidence, but keep the already-compacted recent
	// suffix. Otherwise the terminal stage would undo earlier message/tool
	// compaction exactly when composing compactors should be most useful.
	next, info, err := runSummaryCompaction(ctx, cfg, messages, rawSummaryView(messages), stripImages)
	if err != nil {
		return nil, err
	}
	if info == nil || !containsContextSummary(next) {
		return messages, nil
	}

	next, err = s.applyHooks(ctx, next, *info)
	if err != nil {
		return nil, err
	}

	info.TokensAfter = EstimateTotal(next)
	if info.Duration == 0 {
		info.Duration = time.Millisecond
	}

	return next, nil
}

func (s *SummaryCompactor) applyHooks(ctx context.Context, msgs []agentgo.AgentMessage, info SummaryInfo) ([]agentgo.AgentMessage, error) {
	if len(s.cfg.PostSummaryHooks) == 0 || len(msgs) == 0 {
		return msgs, nil
	}
	kept := append([]agentgo.AgentMessage(nil), msgs[1:]...)
	var injected []agentgo.AgentMessage
	var err error
	for _, hook := range s.cfg.PostSummaryHooks {
		var extra []agentgo.AgentMessage
		extra, err = hook(ctx, info, kept)
		if err != nil {
			return nil, err
		}
		injected = append(injected, extra...)
	}
	if len(injected) == 0 {
		return msgs, nil
	}
	out := make([]agentgo.AgentMessage, 0, len(msgs)+len(injected))
	out = append(out, msgs[0])
	out = append(out, injected...)
	out = append(out, msgs[1:]...)
	return out, nil
}

func containsContextSummary(msgs []agentgo.AgentMessage) bool {
	for _, msg := range msgs {
		if _, ok := msg.(ContextSummary); ok {
			return true
		}
	}
	return false
}
