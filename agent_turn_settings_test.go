package agentgo

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestAgentResetClearsArtifacts(t *testing.T) {
	a := NewAgent(WithModel(mockModel(assistantMsg("done", StopReasonStop))))
	old := AgentSnapshot{State: AgentState{Artifacts: []Artifact{textArtifact{"old-session", "note", "old"}}}}
	if err := a.SetSnapshot(old); err != nil {
		t.Fatal(err)
	}
	a.Reset()
	if got := len(a.Snapshot().State.Artifacts); got != 0 {
		t.Fatalf("Reset retained %d artifacts", got)
	}
	if err := a.Prompt(t.Context(), "new session"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()
	if got := len(a.State().Artifacts); got != 0 {
		t.Fatalf("new run inherited %d artifacts", got)
	}
	if len(old.State.Artifacts) != 1 {
		t.Fatal("Reset changed retained snapshot")
	}
}

func TestAgentTurnSettingsRefreshAfterBeforeTurn(t *testing.T) {
	for _, blocks := range []bool{false, true} {
		t.Run(map[bool]string{false: "prompt", true: "blocks"}[blocks], func(t *testing.T) {
			var calls []string
			oldTool := NewFuncTool("old", "old tool", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (json.RawMessage, error) {
				calls = append(calls, "old")
				return json.RawMessage(`"old"`), nil
			})
			newTool := NewFuncTool("new", "new tool", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (json.RawMessage, error) {
				calls = append(calls, "new")
				return json.RawMessage(`"new"`), nil
			})
			oldModel := mockModel(toolCallMsg(ToolCall{ID: "old-call", Name: "old", Args: json.RawMessage(`{}`)}))
			newCalls := 0
			newModel := callOptionFuncModel(func(_ context.Context, req *LLMRequest, cfg CallConfig) (*LLMResponse, error) {
				newCalls++
				if req.Messages[0].TextContent() != "new prompt" {
					t.Errorf("system = %q", req.Messages[0].TextContent())
				}
				if len(req.Tools) != 1 || req.Tools[0].Name != "new" {
					t.Errorf("tools = %+v", req.Tools)
				}
				if cfg.ThinkingLevel != ThinkingHigh || cfg.PromptCacheKey != "new-key" {
					t.Errorf("options = %+v", cfg)
				}
				if newCalls == 1 {
					return &LLMResponse{Message: toolCallMsg(ToolCall{ID: "new-call", Name: "new", Args: json.RawMessage(`{}`)})}, nil
				}
				return &LLMResponse{Message: assistantMsg("done", StopReasonStop)}, nil
			})
			var a *Agent
			a = NewAgent(WithModel(oldModel), WithTools(oldTool), WithSystemPrompt("old prompt"), WithBeforeTurn(func(_ context.Context, turn BeforeTurnContext) ([]AgentMessage, error) {
				if turn.TurnIndex == 2 {
					a.SetModel(newModel)
					a.SetTools(newTool)
					a.SetThinkingLevel(ThinkingHigh)
					a.SetPromptCacheKey("new-key")
					if blocks {
						a.SetSystemBlocks([]SystemBlock{{Text: "new prompt"}})
					} else {
						a.SetSystemPrompt("new prompt")
					}
				}
				return nil, nil
			}))
			if err := a.Prompt(t.Context(), "start"); err != nil {
				t.Fatal(err)
			}
			a.WaitForIdle()
			if a.State().Error != "" {
				t.Fatal(a.State().Error)
			}
			if !reflect.DeepEqual(calls, []string{"old", "new"}) || newCalls != 2 {
				t.Fatalf("tools=%v new model calls=%d", calls, newCalls)
			}
		})
	}
}

type turnSettingsRetryError struct{}

func (turnSettingsRetryError) Error() string             { return "retry this attempt" }
func (turnSettingsRetryError) Retryable() bool           { return true }
func (turnSettingsRetryError) RetryAfter() time.Duration { return time.Millisecond }

func TestAgentTurnSettingsStableAcrossRetryAndTools(t *testing.T) {
	var a *Agent
	var calls []string
	tool := func(label string) Tool {
		return NewFuncTool("work", label, map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (json.RawMessage, error) {
			calls = append(calls, label)
			return json.RawMessage(`"ok"`), nil
		})
	}
	next := callOptionFuncModel(func(_ context.Context, req *LLMRequest, cfg CallConfig) (*LLMResponse, error) {
		if req.Messages[0].TextContent() != "new" || cfg.ThinkingLevel != ThinkingHigh || cfg.PromptCacheKey != "new" || req.Tools[0].Description != "new" {
			t.Errorf("next turn did not refresh: request=%+v options=%+v", req, cfg)
		}
		return &LLMResponse{Message: assistantMsg("done", StopReasonStop)}, nil
	})
	attempts := 0
	first := callOptionFuncModel(func(_ context.Context, req *LLMRequest, cfg CallConfig) (*LLMResponse, error) {
		attempts++
		if req.Messages[0].TextContent() != "old" || cfg.ThinkingLevel != ThinkingLow || cfg.PromptCacheKey != "old" || req.Tools[0].Description != "old" {
			t.Errorf("active turn configuration changed: request=%+v options=%+v", req, cfg)
		}
		if attempts == 1 {
			a.SetModel(next)
			a.SetTools(tool("new"))
			a.SetSystemPrompt("new")
			a.SetThinkingLevel(ThinkingHigh)
			a.SetPromptCacheKey("new")
			return nil, turnSettingsRetryError{}
		}
		return &LLMResponse{Message: toolCallMsg(ToolCall{ID: "work-call", Name: "work", Args: json.RawMessage(`{}`)})}, nil
	})
	a = NewAgent(WithModel(first), WithTools(tool("old")), WithSystemPrompt("old"), WithThinkingLevel(ThinkingLow), WithPromptCacheKey("old"), WithMaxRetries(1))
	if err := a.Prompt(t.Context(), "start"); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()
	if a.State().Error != "" {
		t.Fatal(a.State().Error)
	}
	if attempts != 2 || !reflect.DeepEqual(calls, []string{"old"}) {
		t.Fatalf("attempts=%d tools=%v", attempts, calls)
	}
}
