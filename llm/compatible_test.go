package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/compforge/agentgo"
	"github.com/compforge/agentgo/llm"
)

func TestCompatibleEffortAndCacheWire(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies <- body
		fmt.Fprint(w, `{"model":"opaque-endpoint","choices":[{"message":{"role":"assistant","reasoning_content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":4,"completion_tokens":8,"total_tokens":12,"completion_tokens_details":{"reasoning_tokens":8}}}`)
	}))
	defer server.Close()
	defaults := map[string]any{"reasoning_effort": "high", "thinking": map[string]any{"type": "enabled"}, "custom": true}
	model, err := llm.NewModel("compat", "opaque-endpoint", llm.WithBaseURL(server.URL), llm.WithExtra(defaults))
	if err != nil {
		t.Fatal(err)
	}
	for _, effort := range []agentgo.ThinkingLevel{agentgo.ThinkingMinimal, agentgo.ThinkingLow, agentgo.ThinkingMedium, agentgo.ThinkingHigh, agentgo.ThinkingXHigh, agentgo.ThinkingMax, agentgo.ThinkingOff, "none"} {
		response, err := model.Generate(t.Context(), []agentgo.Message{agentgo.UserMsg("work")}, nil, agentgo.WithThinking(effort), agentgo.WithCallPromptCacheKey("session"), agentgo.WithMaxTokens(8))
		if err != nil {
			t.Fatal(err)
		}
		body := <-bodies
		want := string(effort)
		if effort == agentgo.ThinkingOff || effort == "none" {
			want = "none"
		}
		if body["reasoning_effort"] != want || body["prompt_cache_key"] != "session" || body["thinking"] != nil || body["custom"] != true {
			t.Fatalf("wire=%v", body)
		}
		if response.Message.StopReason != agentgo.StopReasonLength || response.Message.Usage.Reasoning != 8 || response.Message.Usage.TotalTokens != 12 {
			t.Fatalf("response=%+v", response)
		}
	}
	if defaults["thinking"].(map[string]any)["type"] != "enabled" {
		t.Fatal("shared defaults mutated")
	}
	if _, err := model.Generate(t.Context(), nil, nil, agentgo.WithThinkingBudget(100)); err == nil {
		t.Fatal("unsupported thinking budget silently accepted")
	}
}

func TestProviderPreservesCustomDeadlineCause(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	defer server.Close()
	model, err := llm.NewModel("compat", "endpoint", llm.WithBaseURL(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeoutCause(t.Context(), 10*time.Millisecond, errors.New("exploration deadline"))
	defer cancel()
	_, err = model.Generate(ctx, []agentgo.Message{agentgo.UserMsg("work")}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline identity lost: %v", err)
	}
}
