package llm

import (
	"encoding/json"
	"github.com/compforge/agentgo"
	"github.com/voocel/litellm"
	"testing"
)

func TestStreamEventsRetainPublishedContent(t *testing.T) {
	p := &captureStreamProvider{events: []litellm.Event{
		litellm.BlockStart{Index: 0, Block: litellm.TextBlock{}}, litellm.BlockStart{Index: 1, Block: litellm.ReasoningBlock{}},
		litellm.TextDelta{Index: 0, Text: "A"}, litellm.TextDelta{Index: 0, Text: "B"}, litellm.ReasoningDelta{Index: 1, Text: "first"}, litellm.ReasoningDelta{Index: 1, Text: " second"},
		litellm.BlockEnd{Index: 0}, litellm.BlockEnd{Index: 1}, litellm.DoneEvent{FinishReason: litellm.FinishReasonStop},
	}}
	m := NewLiteLLMAdapter("m", mustClient(t, p))
	ch, err := m.GenerateStream(t.Context(), []agentgo.Message{agentgo.UserMsg("hi")}, nil)
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
		}
	}
}

func TestStreamEventsConcurrentRead(t *testing.T) {
	p := &captureStreamProvider{events: []litellm.Event{litellm.BlockStart{Index: 0, Block: litellm.TextBlock{}}, litellm.BlockStart{Index: 1, Block: litellm.ReasoningBlock{}}}}
	for range 1000 {
		p.events = append(p.events, litellm.TextDelta{Index: 0, Text: "x"}, litellm.ReasoningDelta{Index: 1, Text: "y"})
	}
	p.events = append(p.events, litellm.BlockEnd{Index: 0}, litellm.BlockEnd{Index: 1}, litellm.DoneEvent{FinishReason: litellm.FinishReasonStop})
	m := NewLiteLLMAdapter("m", mustClient(t, p))
	ch, err := m.GenerateStream(t.Context(), []agentgo.Message{agentgo.UserMsg("hi")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for ev := range ch {
		if _, err := json.Marshal(ev.Message); err != nil {
			t.Error(err)
		}
	}
}
