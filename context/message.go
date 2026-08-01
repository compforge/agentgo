package context

import (
	"time"

	"github.com/compforge/agentgo"
)

// ContextSummary is a compacted context summary message. It remains a domain
// message in context and lowers itself only when a model request is built.
type ContextSummary struct {
	Summary       string
	TokensBefore  int
	ReadFiles     []string
	ModifiedFiles []string
	// RawMessages keeps the source transcript available to the application
	// while ToMessage exposes only Summary to the model.
	RawMessages []agentgo.AgentMessage
	Compacted   int
	Kept        int
	SplitTurn   bool
	Timestamp   time.Time
}

func (c ContextSummary) GetRole() agentgo.Role   { return agentgo.RoleUser }
func (c ContextSummary) GetTimestamp() time.Time { return c.Timestamp }
func (c ContextSummary) Raw() agentgo.AgentMessage {
	return c
}
func (c ContextSummary) Priority() int { return 0 }
func (c ContextSummary) Compact(float64) (agentgo.AgentMessage, float64) {
	return c, 1
}
func (c ContextSummary) TextContent() string     { return c.Summary }
func (c ContextSummary) ThinkingContent() string { return "" }
func (c ContextSummary) HasToolCalls() bool      { return false }

func (c ContextSummary) ToMessage() (agentgo.Message, bool) {
	return agentgo.Message{
		Role:    agentgo.RoleUser,
		Content: []agentgo.ContentBlock{agentgo.TextBlock("<context-summary>\n" + c.Summary + "\n</context-summary>")},
		Metadata: map[string]any{
			"type":            "context_summary",
			"tokens_before":   c.TokensBefore,
			"read_files":      c.ReadFiles,
			"modified_files":  c.ModifiedFiles,
			"compacted_count": c.Compacted,
			"kept_count":      c.Kept,
			"split_turn":      c.SplitTurn,
		},
		Timestamp: c.Timestamp,
	}, true
}

// projectedMessage keeps a generic model-level projection beside the original
// domain message. Context policies can therefore trim ordinary Message values
// without destroying data needed by later compaction or persistence.
type projectedMessage struct {
	raw     agentgo.AgentMessage
	current agentgo.AgentMessage
}

func newProjectedMessage(raw agentgo.AgentMessage, current agentgo.AgentMessage) agentgo.AgentMessage {
	return projectedMessage{raw: raw.Raw(), current: current}
}

func (m projectedMessage) GetRole() agentgo.Role              { return m.current.GetRole() }
func (m projectedMessage) GetTimestamp() time.Time            { return m.current.GetTimestamp() }
func (m projectedMessage) Raw() agentgo.AgentMessage          { return m.raw }
func (m projectedMessage) Priority() int                      { return m.raw.Priority() }
func (m projectedMessage) TextContent() string                { return m.current.TextContent() }
func (m projectedMessage) ThinkingContent() string            { return m.current.ThinkingContent() }
func (m projectedMessage) HasToolCalls() bool                 { return m.current.HasToolCalls() }
func (m projectedMessage) ToMessage() (agentgo.Message, bool) { return m.current.ToMessage() }
func (m projectedMessage) Compact(expect float64) (agentgo.AgentMessage, float64) {
	return m.raw.Compact(expect)
}

// rawSummaryView restores per-message domain evidence before a checkpoint is
// generated. Existing summaries stay summarized so incremental compaction does
// not replay an unbounded history into the model.
func rawSummaryView(messages []agentgo.AgentMessage) []agentgo.AgentMessage {
	out := make([]agentgo.AgentMessage, len(messages))
	for i, message := range messages {
		if _, ok := message.(ContextSummary); ok {
			out[i] = message
			continue
		}
		out[i] = message.Raw()
	}
	return out
}

func sourceMessages(messages []agentgo.AgentMessage) []agentgo.AgentMessage {
	var out []agentgo.AgentMessage
	for _, message := range messages {
		if summary, ok := message.(ContextSummary); ok && len(summary.RawMessages) > 0 {
			out = append(out, summary.RawMessages...)
			continue
		}
		out = append(out, message.Raw())
	}
	return out
}
