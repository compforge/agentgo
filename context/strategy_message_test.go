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
	priority int
}

func (m stagedMessage) GetRole() agentgo.Role   { return agentgo.RoleUser }
func (m stagedMessage) GetTimestamp() time.Time { return time.Time{} }
func (m stagedMessage) Raw() agentgo.AgentMessage {
	raw := m
	raw.stage = 0
	return raw
}
func (m stagedMessage) TextContent() string     { return m.contents[m.stage] }
func (m stagedMessage) ThinkingContent() string { return "" }
func (m stagedMessage) HasToolCalls() bool      { return false }
func (m stagedMessage) Priority() int           { return m.priority }
func (m stagedMessage) ToMessage() (agentgo.Message, bool) {
	return agentgo.Message{
		Role:    agentgo.RoleUser,
		Content: []agentgo.ContentBlock{agentgo.TextBlock(m.TextContent())},
	}, true
}
func (m stagedMessage) Compact(expect float64) (agentgo.AgentMessage, float64) {
	rawLen := len(m.contents[0])
	current := float64(len(m.contents[m.stage])) / float64(rawLen)
	if expect >= current {
		return m, current
	}
	stage := len(m.contents) - 1
	for i := m.stage + 1; i < len(m.contents); i++ {
		ratio := float64(len(m.contents[i])) / float64(rawLen)
		if ratio <= expect {
			stage = i
			break
		}
	}
	next := m
	next.stage = stage
	return next, float64(len(next.contents[stage])) / float64(rawLen)
}

func TestMessageCompactionUsesMessageOwnedStages(t *testing.T) {
	full := strings.Repeat("x", 8000)
	outline := strings.Repeat("x", 2000)
	reference := "artifact:path"
	original := stagedMessage{contents: []string{full, outline, reference}}
	compactor := NewMessageCompactor()

	view, err := compactor.Compact(t.Context(), []agentgo.AgentMessage{original}, 0.05)
	if err != nil {
		t.Fatal(err)
	}
	got := view[0].(stagedMessage)
	if got.stage != 2 || got.TextContent() != reference {
		t.Fatalf("compacted message = %#v, want terminal reference stage", got)
	}
	if original.stage != 0 || original.TextContent() != full {
		t.Fatal("compactor mutated the original message")
	}
}

func TestAgentMessageCompactReportsRatioAgainstRaw(t *testing.T) {
	message := stagedMessage{contents: []string{strings.Repeat("x", 100), strings.Repeat("x", 40), "ref"}, stage: 1}

	next, actual := message.Compact(0)
	if got := next.(stagedMessage).stage; got != 2 {
		t.Fatalf("stage = %d, want terminal stage", got)
	}
	if actual != 0.03 {
		t.Fatalf("actual = %f, want 0.03 relative to raw", actual)
	}
	if got := next.Raw().(stagedMessage).stage; got != 0 {
		t.Fatalf("raw stage = %d, want 0", got)
	}
}

func TestMessageCompactionIgnoresNonShrinkingStep(t *testing.T) {
	message := stagedMessage{contents: []string{"short", "a longer replacement"}}
	compactor := NewMessageCompactor()

	view, err := compactor.Compact(t.Context(), []agentgo.AgentMessage{message}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := view[0].(stagedMessage).stage; got != 0 {
		t.Fatalf("stage = %d, want original stage", got)
	}
}

func TestMessageCompactorUsesNewestFirstWithinPriority(t *testing.T) {
	full := strings.Repeat("x", 8000)
	outline := strings.Repeat("x", 2000)
	messages := []agentgo.AgentMessage{
		stagedMessage{contents: []string{full, outline}},
		stagedMessage{contents: []string{full, outline}},
	}
	compactor := NewMessageCompactor()

	view, err := compactor.Compact(t.Context(), messages, 0.75)
	if err != nil {
		t.Fatal(err)
	}
	if got := view[0].(stagedMessage).stage; got != 0 {
		t.Fatalf("oldest stage = %d, want 0", got)
	}
	if got := view[1].(stagedMessage).stage; got != 1 {
		t.Fatalf("newest stage = %d, want 1", got)
	}
}

func TestMessageCompactorExhaustsLowerPriorityFirst(t *testing.T) {
	full := strings.Repeat("x", 8000)
	outline := strings.Repeat("x", 2000)
	messages := []agentgo.AgentMessage{
		stagedMessage{contents: []string{full, outline, "reference"}, priority: 0},
		stagedMessage{contents: []string{full, outline}, priority: 10},
	}
	compactor := NewMessageCompactor()

	view, err := compactor.Compact(t.Context(), messages, 0.6)
	if err != nil {
		t.Fatal(err)
	}
	if got := view[0].(stagedMessage).stage; got != 2 {
		t.Fatalf("low-priority stage = %d, want 2", got)
	}
	if got := view[1].(stagedMessage).stage; got != 0 {
		t.Fatalf("high-priority stage = %d, want 0", got)
	}
}

func TestMessageCompactorPriorityOverridesRecency(t *testing.T) {
	full := strings.Repeat("x", 8000)
	outline := strings.Repeat("x", 2000)
	messages := []agentgo.AgentMessage{
		stagedMessage{contents: []string{full, outline}, priority: 0},
		stagedMessage{contents: []string{full, outline}, priority: 10},
	}
	compactor := NewMessageCompactor()

	view, err := compactor.Compact(t.Context(), messages, 0.75)
	if err != nil {
		t.Fatal(err)
	}
	if got := view[0].(stagedMessage).stage; got != 1 {
		t.Fatalf("low-priority stage = %d, want 1", got)
	}
	if got := view[1].(stagedMessage).stage; got != 0 {
		t.Fatalf("high-priority stage = %d, want 0", got)
	}
}
