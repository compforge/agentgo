package proxy

import (
	"context"
	"encoding/json"
	"github.com/compforge/agentgo"
	"testing"
)

func TestStreamEventsRetainPublishedContent(t *testing.T) {
	frames := []Frame{
		{Type: FrameTextDelta, Delta: "A"}, {Type: FrameTextDelta, Delta: "B"},
		{Type: FrameThinkingDelta, Delta: "first"}, {Type: FrameThinkingDelta, Delta: " second"},
		{Type: FrameToolCallStart, ToolCallID: "call", ToolName: "read"},
		{Type: FrameToolCallDelta, ToolCallID: "call", Delta: `{"file":`},
		{Type: FrameToolCallDelta, ToolCallID: "call", Delta: `"test"}`},
		{Type: FrameDone, StopReason: agentgo.StopReasonToolUse},
	}
	m := New(func(context.Context, *agentgo.LLMRequest) (<-chan Frame, error) {
		ch := make(chan Frame, len(frames))
		for _, f := range frames {
			ch <- f
		}
		close(ch)
		return ch, nil
	})
	ch, err := m.GenerateStream(t.Context(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []agentgo.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	for _, ev := range events {
		switch ev.Delta {
		case "A":
			if got := ev.Message.TextContent(); got != "A" {
				t.Errorf("first text = %q", got)
			}
		case "first":
			if got := ev.Message.ThinkingContent(); got != "first" {
				t.Errorf("first thinking = %q", got)
			}
		case `{"file":`:
			if got := string(ev.Message.ToolCalls()[0].Args); got != `{"file":` {
				t.Errorf("partial args = %q", got)
			}
		}
	}
}

func TestStreamEventsConcurrentRead(t *testing.T) {
	m := New(func(ctx context.Context, _ *agentgo.LLMRequest) (<-chan Frame, error) {
		ch := make(chan Frame, 1001)
		for range 1000 {
			ch <- Frame{Type: FrameTextDelta, Delta: "x"}
		}
		ch <- Frame{Type: FrameDone, StopReason: agentgo.StopReasonStop}
		close(ch)
		return ch, nil
	})
	ch, err := m.GenerateStream(t.Context(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for ev := range ch {
		if _, err := json.Marshal(ev.Message); err != nil {
			t.Error(err)
		}
	}
}
