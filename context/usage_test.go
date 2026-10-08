package context

import (
	"context"
	"encoding/json"
	"github.com/compforge/agentgo"
	"strings"
	"testing"
)

func TestContextUsageIncludesLatestToolArguments(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"body": strings.Repeat("x", 40000)})
	assistant := agentgo.Message{Role: agentgo.RoleAssistant,
		Content: []agentgo.ContentBlock{agentgo.ToolCallBlock(agentgo.ToolCall{Name: "submit", Args: args})},
		Usage:   &agentgo.Usage{Input: 100, Output: 20000, TotalTokens: 20100},
	}
	messages := []agentgo.AgentMessage{agentgo.UserMsg("prompt"), assistant, agentgo.UserMsg("tool result")}
	got := EstimateContextTokens(messages)
	want := 100 + EstimateTokens(assistant) + EstimateTokens(messages[2])
	if got.Tokens != want || got.TrailingTokens < 10000 {
		t.Fatalf("estimate = %+v, want %d", got, want)
	}
	// TotalTokens-only providers must not count their output both as usage and replay.
	assistant.Usage = &agentgo.Usage{TotalTokens: 20100, Output: 20000}
	messages[1] = assistant
	if got := EstimateContextTokens(messages); got.Tokens != want {
		t.Fatalf("total-only estimate = %+v", got)
	}
}

func TestContextUsageIgnoresEmptyOrFailedCalibration(t *testing.T) {
	for _, stop := range []agentgo.StopReason{agentgo.StopReasonError, agentgo.StopReasonAborted} {
		m := agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("output")},
			Usage: &agentgo.Usage{Input: 90000}, StopReason: stop}
		got := EstimateContextTokens([]agentgo.AgentMessage{m})
		if got.UsageTokens != 0 || got.Tokens != EstimateTokens(m) {
			t.Fatalf("failed calibration: %+v", got)
		}
	}
	m := agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("output")}, Usage: &agentgo.Usage{}}
	if got := EstimateContextTokens([]agentgo.AgentMessage{m}); got.LastUsageIndex != -1 {
		t.Fatalf("empty usage accepted: %+v", got)
	}
}

type removePrefix struct{}

func (removePrefix) Compact(_ context.Context, messages []agentgo.AgentMessage, _ float64) ([]agentgo.AgentMessage, error) {
	return messages[1:], nil
}

func TestCompactionInvalidatesOldPromptUsageAndPreservesRaw(t *testing.T) {
	assistant := agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("kept")}, Usage: &agentgo.Usage{Input: 90000}}
	messages := []agentgo.AgentMessage{agentgo.UserMsg(strings.Repeat("x", 4000)), assistant}
	engine := NewEngine(EngineConfig{ContextWindow: 1000, ReserveTokens: 100, Compactor: removePrefix{}})
	projection, err := engine.Compact(context.Background(), agentgo.TransformContext{Messages: messages}, agentgo.CompactReasonThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if !projection.Changed || len(projection.Messages) != 1 {
		t.Fatalf("projection = %+v", projection)
	}
	if projection.Usage.Tokens != EstimateTokens(assistant) {
		t.Fatalf("stale calibration: %+v", projection.Usage)
	}
	view, _ := projection.Messages[0].ToMessage()
	raw, _ := projection.Messages[0].Raw().ToMessage()
	if view.Usage != nil || raw.Usage.Input != 90000 || assistant.Usage.Input != 90000 {
		t.Fatal("usage invalidation mutated raw evidence")
	}
	// The next real response calibrates the new prompt normally.
	fresh := assistant
	fresh.Usage = &agentgo.Usage{Input: 20}
	got := EstimateContextTokens(append(projection.Messages, fresh))
	if got.UsageTokens != 20 {
		t.Fatalf("fresh usage not accepted: %+v", got)
	}
}

func TestNoOpCompactionKeepsCalibration(t *testing.T) {
	messages := []agentgo.AgentMessage{agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("kept")}, Usage: &agentgo.Usage{Input: 90000}}}
	engine := NewEngine(EngineConfig{ContextWindow: 1000, ReserveTokens: 100, Compactor: Chain()})
	projection, err := engine.Compact(context.Background(), agentgo.TransformContext{Messages: messages}, agentgo.CompactReasonThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Changed || projection.Usage.UsageTokens != 90000 {
		t.Fatalf("no-op discarded calibration: %+v", projection)
	}
}

func TestLatestToolArgumentsTriggerCompaction(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"body": strings.Repeat("x", 40000)})
	assistant := agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.ToolCallBlock(agentgo.ToolCall{Name: "submit", Args: args})}, Usage: &agentgo.Usage{Input: 100}}
	compactor := &replacingCompactor{text: "summary"}
	engine := NewEngine(EngineConfig{ContextWindow: 5000, ReserveTokens: 100, Compactor: compactor})
	result, err := engine.Compact(context.Background(), agentgo.TransformContext{Messages: []agentgo.AgentMessage{assistant}}, agentgo.CompactReasonThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if compactor.calls != 1 || result.Compaction == nil {
		t.Fatalf("large tool call bypassed compaction: calls=%d result=%+v", compactor.calls, result)
	}
}
