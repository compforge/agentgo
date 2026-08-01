package context

import (
	"context"
	"fmt"
	"time"

	"github.com/compforge/agentgo"
)

// DefaultClearedToolResult replaces a tool result that microcompact dropped.
// Exported so a host building on ClearedMessageFn can extend it rather than
// re-spell it and drift.
const DefaultClearedToolResult = "[Tool result cleared to save context.]"

type ToolResultMicrocompactConfig struct {
	Classifier     ToolClassifier
	KeepRecent     int
	ClearedMessage string
	// ClearedMessageFn overrides ClearedMessage per result, so a host can carry
	// forward whatever stays actionable after the content goes — a path to the
	// output it persisted on disk, say. "" falls back to ClearedMessage.
	//
	// Must be idempotent: every pass re-clears results a previous pass already
	// cleared, so feeding its own output back in has to yield the same text.
	// Anything else rewrites the prefix on each pass and burns the cache.
	ClearedMessageFn func(toolName string, original agentgo.Message) string
	IdleThreshold    time.Duration
}

type ToolResultCompactor struct {
	cfg ToolResultMicrocompactConfig
}

func NewToolResultCompactor(cfg ToolResultMicrocompactConfig) *ToolResultCompactor {
	if cfg.KeepRecent <= 0 {
		cfg.KeepRecent = 5
	}
	if cfg.ClearedMessage == "" {
		cfg.ClearedMessage = DefaultClearedToolResult
	}
	return &ToolResultCompactor{cfg: cfg}
}

func (s *ToolResultCompactor) Compact(_ context.Context, messages []agentgo.AgentMessage, expect float64) ([]agentgo.AgentMessage, error) {
	if len(messages) == 0 || expect >= 1 {
		return messages, nil
	}

	candidates := findCompactableToolResults(messages, s.cfg.Classifier)
	if len(candidates) == 0 {
		return messages, nil
	}

	keepRecent := s.cfg.KeepRecent
	if s.cfg.IdleThreshold > 0 {
		lastAssistant := latestAssistantTimestamp(messages)
		if !lastAssistant.IsZero() && time.Since(lastAssistant) > s.cfg.IdleThreshold && keepRecent > 1 {
			keepRecent = max(1, keepRecent/2)
		}
	}

	// Protect the most recent keepRecent results, deduplicated by (tool, args):
	// when the model re-issues the identical call, only the newest result is
	// worth protecting — older copies carry no extra information and would
	// crowd genuinely distinct results out of the protection window.
	protected := make(map[int]struct{}, keepRecent)
	seenKeys := make(map[string]struct{}, keepRecent)
	for i := len(candidates) - 1; i >= 0 && len(protected) < keepRecent; i-- {
		c := candidates[i]
		if _, dup := seenKeys[c.Key]; dup {
			continue
		}
		seenKeys[c.Key] = struct{}{}
		protected[c.Index] = struct{}{}
	}
	if len(protected) == len(candidates) {
		return messages, nil
	}

	out := copyMessages(messages)
	tokens := EstimateTotal(out)
	target := int(float64(tokens) * clampRatio(expect))
	for _, candidate := range candidates {
		if tokens <= target {
			break
		}
		if _, ok := protected[candidate.Index]; ok {
			continue
		}
		msg, ok := out[candidate.Index].ToMessage()
		if !ok {
			continue
		}
		before := EstimateTokens(out[candidate.Index])
		next := msg
		next.Content = []agentgo.ContentBlock{agentgo.TextBlock(s.clearedText(candidate.ToolName, msg))}
		next.Metadata = cloneMetadata(msg.Metadata)
		if next.Metadata == nil {
			next.Metadata = map[string]any{}
		}
		next.Metadata["compacted_tool_result"] = true
		next.Metadata["compacted_tool_name"] = candidate.ToolName
		out[candidate.Index] = newProjectedMessage(out[candidate.Index], next)
		tokens -= before - EstimateTokens(out[candidate.Index])
	}

	return out, nil
}

func (s *ToolResultCompactor) clearedText(toolName string, original agentgo.Message) string {
	if s.cfg.ClearedMessageFn != nil {
		if text := s.cfg.ClearedMessageFn(toolName, original); text != "" {
			return text
		}
	}
	return s.cfg.ClearedMessage
}

type compactableToolResult struct {
	Index    int
	ToolName string
	// Key identifies the originating call by tool name + raw args, so results
	// of identical repeated calls can be deduplicated in the protection window.
	Key string
}

type pendingToolCall struct {
	name string
	key  string
}

func findCompactableToolResults(msgs []agentgo.AgentMessage, classifier ToolClassifier) []compactableToolResult {
	pending := map[string]pendingToolCall{}
	var results []compactableToolResult

	for i, am := range msgs {
		msg, ok := am.ToMessage()
		if !ok {
			continue
		}

		if msg.Role == agentgo.RoleAssistant {
			for _, call := range msg.ToolCalls() {
				pending[call.ID] = pendingToolCall{
					name: call.Name,
					key:  call.Name + "\x00" + string(call.Args),
				}
			}
			continue
		}

		if msg.Role != agentgo.RoleTool {
			continue
		}

		callID, _ := msg.Metadata["tool_call_id"].(string)
		call := pending[callID]
		if call.name == "" {
			continue
		}
		if classifier != nil && !classifier(call.name) {
			continue
		}
		results = append(results, compactableToolResult{Index: i, ToolName: call.name, Key: call.key})
	}

	return results
}

func latestAssistantTimestamp(msgs []agentgo.AgentMessage) time.Time {
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, ok := msgs[i].ToMessage()
		if ok && msg.Role == agentgo.RoleAssistant {
			return msg.Timestamp
		}
	}
	return time.Time{}
}

func formatTrimmedPlaceholder(n int) string {
	return fmt.Sprintf("[%d characters trimmed]", n)
}
