package context

import (
	"time"

	"github.com/compforge/agentgo"
)

// ContextSummary is a compacted context summary message.
// It implements AgentMessage but is NOT a Message, so DefaultConvertToLLM
// will filter it out. Use ContextConvertToLLM to handle it.
type ContextSummary struct {
	Summary       string
	TokensBefore  int
	ReadFiles     []string
	ModifiedFiles []string
	Timestamp     time.Time
}

func (c ContextSummary) GetRole() agentgo.Role { return agentgo.RoleUser }
func (c ContextSummary) GetTimestamp() time.Time { return c.Timestamp }
func (c ContextSummary) TextContent() string     { return c.Summary }
func (c ContextSummary) ThinkingContent() string { return "" }
func (c ContextSummary) HasToolCalls() bool      { return false }

// ContextConvertToLLM converts AgentMessages to LLM Messages,
// handling ContextSummary by wrapping it as a user message with XML tags.
// For all other message types, it delegates to DefaultConvertToLLM behavior.
func ContextConvertToLLM(msgs []agentgo.AgentMessage) []agentgo.Message {
	out := make([]agentgo.Message, 0, len(msgs))
	for _, m := range msgs {
		switch v := m.(type) {
		case ContextSummary:
			out = append(out, agentgo.Message{
				Role:    agentgo.RoleUser,
				Content: []agentgo.ContentBlock{agentgo.TextBlock("<context-summary>\n" + v.Summary + "\n</context-summary>")},
				Metadata: map[string]any{
					"type":           "context_summary",
					"tokens_before":  v.TokensBefore,
					"read_files":     v.ReadFiles,
					"modified_files": v.ModifiedFiles,
				},
				Timestamp: v.Timestamp,
			})
		case agentgo.Message:
			if v.StopReason == agentgo.StopReasonError || v.StopReason == agentgo.StopReasonAborted {
				continue
			}
			out = append(out, v)
		}
	}
	return out
}
