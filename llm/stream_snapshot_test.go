package llm

import (
	"encoding/json"
	"github.com/compforge/agentgo"
	"github.com/voocel/litellm"
	"testing"
)

func TestStreamEventsRetainPublishedContent(t *testing.T) {
	p := &captureStreamProvider{events: []litellm.Event{
		litellm.ContentDelta{Text: "A"}, litellm.ContentDelta{Text: "B"},
		litellm.ReasoningDelta{Text: "first"}, litellm.ReasoningDelta{Text: " second"},
		litellm.RefusalDelta{Text: "no"}, litellm.RefusalDelta{Text: " more"},
		litellm.DoneEvent{FinishReason: litellm.FinishReasonStop},
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
		case "no":
			if got := ev.Message.Metadata["refusal"]; got != "no" {
				t.Errorf("first refusal = %v", got)
			}
		}
	}
}

func TestStreamEventsConcurrentRead(t *testing.T) {
	p := &captureStreamProvider{}
	for range 1000 {
		p.events = append(p.events, litellm.ContentDelta{Text: "x"}, litellm.ReasoningDelta{Text: "y"}, litellm.RefusalDelta{Text: "z"})
	}
	p.events = append(p.events, litellm.DoneEvent{FinishReason: litellm.FinishReasonStop})
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
