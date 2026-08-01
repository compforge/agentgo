package context

import (
	"strings"
	"testing"
	"time"

	"github.com/compforge/agentgo"
)

type stagedMessage struct {
	contents []string
	stage    int
}

func (m stagedMessage) GetRole() agentgo.Role   { return agentgo.RoleUser }
func (m stagedMessage) GetTimestamp() time.Time { return time.Time{} }
func (m stagedMessage) TextContent() string     { return m.contents[m.stage] }
func (m stagedMessage) ThinkingContent() string { return "" }
func (m stagedMessage) HasToolCalls() bool      { return false }
func (m stagedMessage) ToMessage() (agentgo.Message, bool) {
	return agentgo.Message{
		Role:    agentgo.RoleUser,
		Content: []agentgo.ContentBlock{agentgo.TextBlock(m.TextContent())},
	}, true
}
func (m stagedMessage) Compact() (agentgo.AgentMessage, bool) {
	if m.stage+1 >= len(m.contents) {
		return nil, false
	}
	next := m
	next.stage++
	return next, true
}

func TestMessageCompactionUsesMessageOwnedStages(t *testing.T) {
	full := strings.Repeat("x", 8000)
	outline := strings.Repeat("x", 2000)
	reference := "artifact:path"
	original := stagedMessage{contents: []string{full, outline, reference}}
	strategy := NewMessageCompaction()

	view, result, err := strategy.Apply(t.Context(), nil, []agentgo.AgentMessage{original}, Budget{
		Tokens:    EstimateTokens(original),
		Threshold: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.TokensSaved <= 0 {
		t.Fatalf("result = %+v, want applied compaction", result)
	}
	got := view[0].(stagedMessage)
	if got.stage != 2 || got.TextContent() != reference {
		t.Fatalf("compacted message = %#v, want terminal reference stage", got)
	}
	if original.stage != 0 || original.TextContent() != full {
		t.Fatal("strategy mutated the original message")
	}
}

func TestMessageCompactionIgnoresNonShrinkingStep(t *testing.T) {
	message := stagedMessage{contents: []string{"short", "a longer replacement"}}
	strategy := NewMessageCompaction()

	view, result, err := strategy.Apply(t.Context(), nil, []agentgo.AgentMessage{message}, Budget{
		Tokens:    EstimateTokens(message),
		Threshold: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied {
		t.Fatalf("result = %+v, non-shrinking compaction must be ignored", result)
	}
	if got := view[0].(stagedMessage).stage; got != 0 {
		t.Fatalf("stage = %d, want original stage", got)
	}
}
