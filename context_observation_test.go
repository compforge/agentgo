package agentgo

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

type observedApplicationMessage struct {
	applicationMessage
	items   []ContextItem
	demands []ContextDemand
}

func (m observedApplicationMessage) ContextItems() []ContextItem {
	return append([]ContextItem(nil), m.items...)
}

func (m observedApplicationMessage) ContextDemands() []ContextDemand {
	return append([]ContextDemand(nil), m.demands...)
}

func TestContextItemAndDemandShareFlatKeyProtocol(t *testing.T) {
	key := ContextKey{Kind: "file", Identity: "internal/loop.go"}
	item := ContextItem{
		ContextKey: key, Representation: "outline", Reason: "caller", Ref: "Run",
	}
	demand := ContextDemand{ContextKey: key, Signal: "source_request"}

	itemJSON, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(itemJSON), `{"kind":"file","identity":"internal/loop.go","representation":"outline","reason":"caller","ref":"Run"}`; got != want {
		t.Fatalf("ContextItem JSON = %s, want %s", got, want)
	}
	demandJSON, err := json.Marshal(demand)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(demandJSON), `{"kind":"file","identity":"internal/loop.go","signal":"source_request"}`; got != want {
		t.Fatalf("ContextDemand JSON = %s, want %s", got, want)
	}
}

func TestCollectContextObservationsPreservesApplicationOrder(t *testing.T) {
	first := observedApplicationMessage{
		applicationMessage: applicationMessage{text: "first", include: true},
		items: []ContextItem{{
			ContextKey:     ContextKey{Kind: "file", Identity: "a.go"},
			Representation: "source",
		}},
	}
	second := observedApplicationMessage{
		applicationMessage: applicationMessage{text: "second", include: true},
		items: []ContextItem{{
			ContextKey:     ContextKey{Kind: "spec", Identity: "checkout"},
			Representation: "full",
		}},
		demands: []ContextDemand{{
			ContextKey: ContextKey{Kind: "file", Identity: "b.go"},
			Signal:     "search_hit",
		}},
	}
	messages := []AgentMessage{first, UserMsg("plain"), second}

	if got, want := CollectContextItems(messages), append(first.items, second.items...); !reflect.DeepEqual(got, want) {
		t.Fatalf("CollectContextItems() = %#v, want %#v", got, want)
	}
	if got, want := CollectContextDemands(messages), second.demands; !reflect.DeepEqual(got, want) {
		t.Fatalf("CollectContextDemands() = %#v, want %#v", got, want)
	}
}

func TestAgentLoopEmitsProjectedContextInventory(t *testing.T) {
	message := observedApplicationMessage{
		applicationMessage: applicationMessage{text: "review", include: true},
		items: []ContextItem{{
			ContextKey:     ContextKey{Kind: "file", Identity: "a.go"},
			Representation: "source",
		}},
	}
	events := runTestLoop(t,
		[]AgentMessage{message},
		AgentContext{},
		LoopConfig{Model: mockModel(assistantMsg("done", StopReasonStop))},
	)

	projected, ok := findEvent(events, EventContextProjected)
	if !ok {
		t.Fatal("missing context_projected event")
	}
	if !reflect.DeepEqual(projected.ContextItems, message.items) {
		t.Fatalf("projected items = %#v, want %#v", projected.ContextItems, message.items)
	}
}

func TestContextProjectedEventUsesActualCompactedRepresentation(t *testing.T) {
	key := ContextKey{Kind: "file", Identity: "a.go"}
	raw := observedApplicationMessage{
		applicationMessage: applicationMessage{text: "full source", include: true},
		items:              []ContextItem{{ContextKey: key, Representation: "source"}},
	}
	outline := observedApplicationMessage{
		applicationMessage: applicationMessage{text: "outline", include: true},
		items:              []ContextItem{{ContextKey: key, Representation: "outline"}},
	}
	agentCtx := &AgentContext{Messages: []AgentMessage{raw}}
	events := make(chan Event, 8)
	config := LoopConfig{
		ContextManager: projectionCommitManager{
			projection: ContextProjection{Messages: []AgentMessage{outline}},
		},
		Model: mockModel(assistantMsg("done", StopReasonStop)),
	}
	if _, _, err := callLLM(context.Background(), agentCtx, config, 1, 1, eventSink{ctx: context.Background(), ch: events}); err != nil {
		t.Fatal(err)
	}
	close(events)

	projected, ok := findEvent(collectEvents(events), EventContextProjected)
	if !ok {
		t.Fatal("missing context_projected event")
	}
	want := []ContextItem{{ContextKey: key, Representation: "outline"}}
	if !reflect.DeepEqual(projected.ContextItems, want) {
		t.Fatalf("projected items = %#v, want actual compacted inventory %#v", projected.ContextItems, want)
	}
}
