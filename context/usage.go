package context

import (
	"unicode/utf8"

	"github.com/compforge/agentgo"
)

// estimateTextTokens estimates token count for a text string with CJK awareness.
// For CJK-dominant text (bytes/runes > 2): uses runes * 1.5 (empirical fit for
// Chinese/Japanese/Korean tokenizers). For ASCII-dominant text: uses bytes / 4
// (standard BPE approximation). The dominant script is auto-detected so callers
// do not need to configure it.
func estimateTextTokens(text string) int {
	bytes := len(text)
	if bytes == 0 {
		return 0
	}
	runes := utf8.RuneCountInString(text)
	if runes == 0 {
		return 0
	}
	// Multi-byte dominant (CJK): avg bytes/rune > 2 indicates most chars are 3-byte UTF-8
	if bytes > runes*2 {
		return max(int(float64(runes)*1.5+0.5), 1)
	}
	// ASCII dominant: standard bytes/4 approximation
	return max((bytes+3)/4, 1)
}

// EstimateTokens estimates the token count for a single message.
// Text and thinking blocks use CJK-aware estimation; tool calls and images
// use byte-based heuristics (JSON args are ASCII-dominant).
func EstimateTokens(msg agentgo.AgentMessage) int {
	if msg == nil {
		return 0
	}
	modelMessage, include := msg.ToMessage()
	if !include {
		return 0
	}

	var tokens int
	for _, b := range modelMessage.Content {
		switch b.Type {
		case agentgo.ContentText:
			tokens += estimateTextTokens(b.Text)
		case agentgo.ContentThinking:
			tokens += estimateTextTokens(b.Thinking)
		case agentgo.ContentToolCall:
			if b.ToolCall != nil {
				// Tool call args are JSON (ASCII-dominant), use byte-based estimation
				tokens += max((len(b.ToolCall.Name)+len(b.ToolCall.Args)+3)/4, 1)
			}
		case agentgo.ContentImage:
			tokens += 1200
		}
	}

	return max(tokens, 1)
}

// EstimateTotal estimates the total token count for a message list.
func EstimateTotal(msgs []agentgo.AgentMessage) int {
	total := 0
	for _, m := range msgs {
		total += EstimateTokens(m)
	}
	return total
}

// ---------------------------------------------------------------------------
// Hybrid context token estimation
// ---------------------------------------------------------------------------

// ContextUsageEstimate holds the hybrid token estimation result.
// It combines LLM-reported Usage data with chars/4 estimation for trailing
// messages to approximate current context-window occupancy.
type ContextUsageEstimate struct {
	// Tokens is the total estimated context tokens (UsageTokens + TrailingTokens).
	Tokens int
	// UsageTokens is the token count derived from the last LLM-reported Usage.
	UsageTokens int
	// TrailingTokens estimates the assistant output carrying the last Usage and all later messages.
	TrailingTokens int
	// LastUsageIndex is the index of the last assistant message with Usage, or -1 if none.
	LastUsageIndex int
}

// calculateContextTokens computes total input tokens from LLM-reported Usage.
// Input already includes CacheRead (litellm convention), so the full prompt
// size is Input + CacheWrite (cache creation is reported separately and not
// rolled into Input). The output of this call is estimated from its replayable
// message, not Output usage, which can include non-replayed reasoning.
func calculateContextTokens(u *agentgo.Usage) int {
	total := u.Input + u.CacheWrite
	if total > 0 {
		return total
	}
	return max(0, u.TotalTokens-u.Output)
}

// EstimateContextTokens uses a hybrid approach: actual Usage from the last
// non-aborted assistant message, plus estimates for that output and later messages.
// This approximates the current context window occupancy.
func EstimateContextTokens(msgs []agentgo.AgentMessage) ContextUsageEstimate {
	// Walk backwards to find the last assistant projection with valid Usage.
	lastIdx := -1
	var lastUsage *agentgo.Usage
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, ok := msgs[i].ToMessage()
		if !ok || msg.Role != agentgo.RoleAssistant {
			continue
		}
		if msg.StopReason == agentgo.StopReasonAborted || msg.StopReason == agentgo.StopReasonError {
			continue
		}
		if msg.Usage != nil && calculateContextTokens(msg.Usage) > 0 {
			lastIdx = i
			lastUsage = msg.Usage
			break
		}
	}

	// No Usage found — fall back to pure chars/4 estimation
	if lastIdx < 0 {
		total := EstimateTotal(msgs)
		return ContextUsageEstimate{
			Tokens:         total,
			TrailingTokens: total,
			LastUsageIndex: -1,
		}
	}

	usageTokens := calculateContextTokens(lastUsage)

	// Usage describes the request before this assistant output existed. That
	// output (including tool arguments) is replayed on the next request.
	var trailing int
	for i := lastIdx; i < len(msgs); i++ {
		trailing += EstimateTokens(msgs[i])
	}

	return ContextUsageEstimate{
		Tokens:         usageTokens + trailing,
		UsageTokens:    usageTokens,
		TrailingTokens: trailing,
		LastUsageIndex: lastIdx,
	}
}

// ContextEstimateAdapter adapts EstimateContextTokens to the
// agentgo.ContextEstimateFn signature used by agent runtime options.
func ContextEstimateAdapter(msgs []agentgo.AgentMessage) (tokens, usageTokens, trailingTokens int) {
	e := EstimateContextTokens(msgs)
	return e.Tokens, e.UsageTokens, e.TrailingTokens
}

// InvalidateUsage removes context calibration from a rewritten model view.
// API usage measures the old prompt; retaining it after trimming/removing a
// prefix would undo the apparent savings or accept an ineffective compaction.
// Raw messages retain their usage for billing, persistence and later inspection.
// Applications that rewrite context outside ContextEngine must call this too.
func InvalidateUsage(messages []agentgo.AgentMessage) []agentgo.AgentMessage {
	out := make([]agentgo.AgentMessage, len(messages))
	for i, message := range messages {
		view, include := message.ToMessage()
		if !include || view.Usage == nil {
			out[i] = message
		} else {
			out[i] = uncalibratedMessage{message}
		}
	}
	return out
}

type uncalibratedMessage struct{ agentgo.AgentMessage }

func (m uncalibratedMessage) ToMessage() (agentgo.Message, bool) {
	view, include := m.AgentMessage.ToMessage()
	view.Usage = nil
	return view, include
}

func (m uncalibratedMessage) Compact(expect float64) (agentgo.AgentMessage, float64) {
	next, ratio := m.AgentMessage.Compact(expect)
	if next == nil {
		return nil, ratio
	}
	return uncalibratedMessage{next}, ratio
}

func (m uncalibratedMessage) ContextItems() []agentgo.ContextItem {
	if provider, ok := m.AgentMessage.(agentgo.ContextItemProvider); ok {
		return provider.ContextItems()
	}
	return nil
}

func (m uncalibratedMessage) ContextDemands() []agentgo.ContextDemand {
	if provider, ok := m.AgentMessage.(agentgo.ContextDemandProvider); ok {
		return provider.ContextDemands()
	}
	return nil
}
