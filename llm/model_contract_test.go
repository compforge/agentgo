package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/compforge/agentgo"
	"github.com/voocel/litellm"
	"github.com/voocel/litellm/provider/glm"
)

func TestThinkingOverrideReachesProviderWire(t *testing.T) {
	var bodies []map[string]any
	provider, err := glm.New(glm.Config{
		APIKey: "test-key", BaseURL: "https://provider.test/v1", AllowUnknownProviderOptions: true,
		HTTPClient: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, body)
			response := `{"model":"glm-5","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`
			if body["stream"] == true {
				response = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	model := NewLiteLLMAdapter("glm-5", mustClient(t, provider))
	model.extra = map[string]any{"thinking": map[string]any{"type": "enabled"}, "reasoning_effort": "high"}
	for _, stream := range []bool{false, true} {
		for _, level := range []agentgo.ThinkingLevel{agentgo.ThinkingOff, "none", agentgo.ThinkingLow} {
			var final agentgo.Message
			if stream {
				ch, err := model.GenerateStream(context.Background(), []agentgo.Message{agentgo.UserMsg("work")}, nil, agentgo.WithThinking(level))
				if err != nil {
					t.Fatal(err)
				}
				for ev := range ch {
					if ev.Err != nil {
						t.Fatal(ev.Err)
					}
					if ev.Type == agentgo.StreamEventDone {
						final = ev.Message
					}
				}
			} else {
				resp, err := model.Generate(context.Background(), []agentgo.Message{agentgo.UserMsg("work")}, nil, agentgo.WithThinking(level))
				if err != nil {
					t.Fatal(err)
				}
				final = resp.Message
			}
			body := bodies[len(bodies)-1]
			thinking := body["thinking"].(map[string]any)
			if level == agentgo.ThinkingLow {
				if thinking["type"] != "enabled" || body["reasoning_effort"] == nil {
					t.Fatalf("enabled wire=%v", body)
				}
			} else if thinking["type"] != "disabled" || body["reasoning_effort"] != nil {
				t.Fatalf("disabled wire=%v", body)
			}
			if final.Metadata["model_request_settings"] == nil {
				t.Fatal("resolved adapter settings missing")
			}
		}
	}
	// Per-call overrides must not rewrite shared provider defaults.
	if model.extra["reasoning_effort"] != "high" || model.extra["thinking"].(map[string]any)["type"] != "enabled" {
		t.Fatalf("shared defaults mutated: %v", model.extra)
	}
	_, err = model.Generate(context.Background(), []agentgo.Message{agentgo.UserMsg("work")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := bodies[len(bodies)-1]
	if body["thinking"].(map[string]any)["type"] != "enabled" || body["reasoning_effort"] != "high" {
		t.Fatalf("next call lost defaults: %v", body)
	}
	calls := len(bodies)
	_, err = model.Generate(context.Background(), []agentgo.Message{agentgo.UserMsg("work")}, nil, agentgo.WithThinkingBudget(256))
	if err == nil || len(bodies) != calls {
		t.Fatalf("unsupported budget was silently accepted: %v", err)
	}
	_, err = model.Generate(context.Background(), []agentgo.Message{agentgo.UserMsg("work")}, nil, agentgo.WithThinking("unsupported-effort"))
	if err == nil || len(bodies) != calls {
		t.Fatalf("unsupported effort was sent: calls=%d err=%v", len(bodies), err)
	}
}

func TestReasoningOnlyLengthResponsePreserved(t *testing.T) {
	usage := litellm.Usage{InputTokens: 10, OutputTokens: 32, ReasoningTokens: 32, TotalTokens: 42}
	provider := &captureProvider{chatFunc: func(context.Context, *litellm.Request) (*litellm.Response, error) {
		return &litellm.Response{Blocks: []litellm.Block{litellm.ReasoningBlock{Text: "partial reasoning"}}, FinishReason: litellm.FinishReasonLength, FinishReasonRaw: "length", Usage: usage}, nil
	}}
	model := NewLiteLLMAdapter("m", mustClient(t, provider))
	response, err := model.Generate(context.Background(), []agentgo.Message{agentgo.UserMsg("work")}, nil, agentgo.WithThinking(agentgo.ThinkingHigh), agentgo.WithMaxTokens(32))
	if err != nil {
		t.Fatal(err)
	}
	streamProvider := &captureStreamProvider{events: []litellm.Event{
		litellm.ReasoningDelta{Text: "partial reasoning"}, litellm.UsageEvent{Usage: usage}, litellm.DoneEvent{FinishReason: litellm.FinishReasonLength, FinishReasonRaw: "length", Provider: "capture", Model: "m"},
	}}
	streamModel := NewLiteLLMAdapter("m", mustClient(t, streamProvider))
	ch, err := streamModel.GenerateStream(context.Background(), []agentgo.Message{agentgo.UserMsg("work")}, nil, agentgo.WithThinking(agentgo.ThinkingHigh), agentgo.WithMaxTokens(32))
	if err != nil {
		t.Fatal(err)
	}
	messages := []agentgo.Message{response.Message}
	for ev := range ch {
		if ev.Err != nil {
			t.Fatal(ev.Err)
		}
		if ev.Type == agentgo.StreamEventDone {
			messages = append(messages, ev.Message)
		}
	}
	if len(messages) != 2 {
		t.Fatalf("final messages=%d", len(messages))
	}
	for _, msg := range messages {
		if msg.StopReason != agentgo.StopReasonLength || msg.TextContent() != "" || msg.ThinkingContent() == "" || msg.Usage.Reasoning != 32 || msg.Usage.Output != 32 || msg.Usage.TotalTokens != 42 {
			t.Fatalf("message=%+v usage=%+v", msg, msg.Usage)
		}
		if msg.Metadata["finish_reason_raw"] != "length" {
			t.Fatalf("metadata=%v", msg.Metadata)
		}
		cost := CalculateCost(&ModelPricing{InputPerToken: 1, OutputPerToken: 1}, msg.Usage)
		if cost.Total != 42 {
			t.Fatalf("reasoning double counted: %+v", cost)
		}
		codec, err := agentgo.NewCodec()
		if err != nil {
			t.Fatal(err)
		}
		data, err := codec.Marshal(agentgo.AgentState{Messages: []agentgo.AgentMessage{msg}})
		if err != nil {
			t.Fatal(err)
		}
		var restored agentgo.AgentState
		if err = codec.Unmarshal(data, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.Messages[0].(agentgo.Message).Usage.Reasoning != 32 {
			t.Fatal("reasoning usage lost in codec")
		}
	}
}

func TestThinkingBudgetIsNotSilentlyIgnored(t *testing.T) {
	provider := &captureProvider{}
	model := NewLiteLLMAdapter("m", mustClient(t, provider))
	_, err := model.Generate(context.Background(), []agentgo.Message{agentgo.UserMsg("work")}, nil, agentgo.WithThinkingBudget(256))
	if err != nil {
		t.Fatal(err)
	}
	if provider.lastReq.Thinking == nil || provider.lastReq.Thinking.BudgetTokens == nil || *provider.lastReq.Thinking.BudgetTokens != 256 {
		t.Fatalf("request=%+v", provider.lastReq)
	}
	for _, opts := range [][]agentgo.CallOption{{agentgo.WithThinkingBudget(-1)}, {agentgo.WithThinking(agentgo.ThinkingOff), agentgo.WithThinkingBudget(256)}} {
		provider.lastReq = nil
		_, err = model.Generate(context.Background(), []agentgo.Message{agentgo.UserMsg("work")}, nil, opts...)
		if err == nil || provider.lastReq != nil {
			t.Fatalf("invalid budget dispatched, error=%v", err)
		}
	}
}
