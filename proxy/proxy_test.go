package proxy

import (
	"context"
	"encoding/json"
	"github.com/compforge/agentgo"
	"strings"
	"testing"
)

func TestToolCallFramesKeepInterleavedArgumentsAndIdentity(t *testing.T) {
	model := New(func(context.Context, *agentgo.LLMRequest) (<-chan Frame, error) {
		frames := []Frame{
			{Type: FrameToolCallStart, ToolCallID: "a", ToolName: "read"},
			{Type: FrameToolCallDelta, Delta: `{"a":`},
			{Type: FrameToolCallStart, ToolCallID: "b", ToolName: "read"},
			{Type: FrameToolCallDelta, Delta: `{"b":`},
			{Type: FrameToolCallDelta, ToolCallID: "a", Delta: `1}`},
			{Type: FrameToolCallDelta, ToolCallID: "b", Delta: `2}`},
			{Type: FrameDone, StopReason: agentgo.StopReasonToolUse},
		}
		ch := make(chan Frame, len(frames))
		for _, f := range frames {
			ch <- f
		}
		close(ch)
		return ch, nil
	})
	ch, err := model.GenerateStream(t.Context(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	var final agentgo.Message
	for ev := range ch {
		if ev.Type == agentgo.StreamEventToolCallDelta {
			ids = append(ids, ev.ToolID)
		}
		if ev.Type == agentgo.StreamEventDone {
			final = ev.Message
		}
	}
	if len(ids) != 4 || ids[0] != "a" || ids[1] != "b" || ids[2] != "a" || ids[3] != "b" {
		t.Fatalf("ids=%v", ids)
	}
	calls := final.ToolCalls()
	if len(calls) != 2 || string(calls[0].Args) != `{"a":1}` || string(calls[1].Args) != `{"b":2}` {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestProxyForwardsCallConfiguration(t *testing.T) {
	var captured []*agentgo.LLMRequest
	model := New(func(_ context.Context, req *agentgo.LLMRequest) (<-chan Frame, error) {
		captured = append(captured, req)
		ch := make(chan Frame, 1)
		ch <- Frame{Type: FrameDone, StopReason: agentgo.StopReasonLength, Usage: &agentgo.Usage{Output: 32, Reasoning: 32, TotalTokens: 42}}
		close(ch)
		return ch, nil
	})
	resp, err := model.Generate(context.Background(), []agentgo.Message{agentgo.UserMsg("plan")}, nil, agentgo.WithThinking(agentgo.ThinkingLow), agentgo.WithThinkingBudget(24), agentgo.WithMaxTokens(32), agentgo.WithToolChoice("none"), agentgo.WithAPIKey("local-secret"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := captured[0].Config
	if cfg == nil || cfg.ThinkingLevel != agentgo.ThinkingLow || cfg.ThinkingBudget != 24 || cfg.MaxTokens != 32 || cfg.ToolChoice != "none" {
		t.Fatalf("config=%+v", cfg)
	}
	b, err := json.Marshal(captured[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "local-secret") {
		t.Fatal("request serialized local credential")
	}
	var wire agentgo.LLMRequest
	if err = json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Config.ThinkingLevel != agentgo.ThinkingLow || wire.Config.APIKey != "" {
		t.Fatalf("wire config=%+v", wire.Config)
	}
	if resp.Message.StopReason != agentgo.StopReasonLength || resp.Message.Usage.Reasoning != 32 {
		t.Fatalf("response=%+v", resp)
	}
	_, err = model.Generate(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if captured[1].Config.ThinkingLevel != "" || captured[1].Config.MaxTokens != 0 {
		t.Fatalf("options leaked into next call: %+v", captured[1].Config)
	}
}
