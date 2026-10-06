package context

import (
	"context"
	"github.com/compforge/agentgo"
	"strings"
	"testing"
)

func TestExternalRetentionDisablesBuiltinSuffixes(t *testing.T) {
	source := strings.Repeat("evidence ", 1000)
	input := []agentgo.AgentMessage{
		agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.ToolCallBlock(agentgo.ToolCall{ID: "read-1", Name: "read_file", Args: []byte(`{"path":"source.go"}`)})}},
		agentgo.ToolResultMsg("read-1", []byte(source), false),
	}
	strategies := []Compactor{
		NewToolResultCompactor(ToolResultMicrocompactConfig{KeepRecent: -1, Classifier: func(string) bool { return true }}),
		NewLightTrimCompactor(LightTrimConfig{KeepRecent: -1}),
	}
	for _, strategy := range strategies {
		out, err := strategy.Compact(t.Context(), input, .5)
		if err != nil {
			t.Fatal(err)
		}
		if out[1].TextContent() == input[1].TextContent() {
			t.Fatalf("%T protected latest historical result", strategy)
		}
		if out[1].Raw().TextContent() != input[1].TextContent() {
			t.Fatal("raw lost")
		}
	}
}

func TestSummaryCanSummarizeEntireCallerSelectedHistory(t *testing.T) {
	for _, empty := range []bool{false, true} {
		calls := 0
		model := stubModel{generate: func(_ context.Context, _ []agentgo.Message, _ []agentgo.ToolSpec, _ ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
			calls++
			content := "retained conclusion"
			if empty {
				content = ""
			}
			return &agentgo.LLMResponse{Message: agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock(content)}}}, nil
		}}
		source := agentgo.UserMsg(strings.Repeat("source ", 1000))
		compactor := NewSummaryCompactor(FullSummaryConfig{Model: model, KeepRecentTokens: -1})
		out, err := compactor.Compact(t.Context(), []agentgo.AgentMessage{source}, .5)
		if empty {
			if err == nil {
				t.Fatal("empty summary accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if calls != 1 || len(out) != 1 {
			t.Fatalf("calls=%d messages=%d", calls, len(out))
		}
		summary, ok := out[0].(ContextSummary)
		if !ok || summary.Kept != 0 || len(summary.RawMessages) != 1 {
			t.Fatalf("summary=%+v", summary)
		}
	}
}
