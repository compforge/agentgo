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
	Timestamp     time.Time
}

func (c ContextSummary) GetRole() agentgo.Role   { return agentgo.RoleUser }
func (c ContextSummary) GetTimestamp() time.Time { return c.Timestamp }
func (c ContextSummary) Priority() int           { return 0 }
func (c ContextSummary) TextContent() string     { return c.Summary }
func (c ContextSummary) ThinkingContent() string { return "" }
func (c ContextSummary) HasToolCalls() bool      { return false }

func (c ContextSummary) ToMessage() (agentgo.Message, bool) {
	return agentgo.Message{
		Role:    agentgo.RoleUser,
		Content: []agentgo.ContentBlock{agentgo.TextBlock("<context-summary>\n" + c.Summary + "\n</context-summary>")},
		Metadata: map[string]any{
			"type":           "context_summary",
			"tokens_before":  c.TokensBefore,
			"read_files":     c.ReadFiles,
			"modified_files": c.ModifiedFiles,
		},
		Timestamp: c.Timestamp,
	}, true
}
