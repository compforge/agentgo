package context

import (
	"github.com/compforge/agentgo"
	"reflect"
	"testing"
	"time"
)

func TestCodecRoundTripCompactedSnapshot(t *testing.T) {
	original := agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("original evidence")}, Usage: &agentgo.Usage{Input: 90000}, Timestamp: time.Now().UTC()}
	current := original
	current.Content = []agentgo.ContentBlock{agentgo.TextBlock("file reference")}
	projected := newProjectedMessage(original, current)
	summary := ContextSummary{Summary: "checkpoint", TokensBefore: 90000, ReadFiles: []string{"input.go"}, ModifiedFiles: []string{"output.go"}, RawMessages: []agentgo.AgentMessage{projected}, Compacted: 3, Kept: 1, SplitTurn: true, Timestamp: original.Timestamp}
	snapshot := agentgo.AgentSnapshot{State: agentgo.AgentState{Messages: InvalidateUsage([]agentgo.AgentMessage{summary, projected})}}
	c, err := agentgo.NewCodec(CodecOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	data, err := c.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var restored agentgo.AgentSnapshot
	if err := c.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot, restored) {
		t.Fatalf("snapshot changed on restore: %#v", restored)
	}
	view, _ := restored.State.Messages[1].ToMessage()
	raw, _ := restored.State.Messages[1].Raw().ToMessage()
	if view.TextContent() != "file reference" || view.Usage != nil || raw.TextContent() != "original evidence" || raw.Usage.Input != 90000 {
		t.Fatalf("lost raw/view/usage: view=%+v raw=%+v", view, raw)
	}
	engine := NewEngine(EngineConfig{ContextWindow: 1000})
	engine.Sync(restored.State.Messages)
	if engine.Usage().UsageTokens != 0 {
		t.Fatal("restore revived stale calibration")
	}
}
