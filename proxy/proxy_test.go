package proxy

import (
	"context"
	"github.com/compforge/agentgo"
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
