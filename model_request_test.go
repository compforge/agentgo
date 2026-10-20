package agentgo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// +case:id=model_request_after_middleware,desc=`Middleware rewrites a request and executor mutates its containers`,expect=`Published request retains the actual pre-executor input`
func TestModelRequestAfterMiddleware(t *testing.T) {
	var events []Event
	executor := NewModelExecutor(ModelExecutorConfig{Middlewares: []ModelMiddleware{
		func(ctx context.Context, call ModelExecution, next ModelExecuteFunc) (ModelResult, error) {
			call.Request.Messages = []Message{{Role: RoleUser,
				Content:  []ContentBlock{TextBlock("effective"), ToolCallBlock(ToolCall{ID: "c", Args: json.RawMessage(`{"a":1}`)})},
				Metadata: map[string]any{"source": "middleware"},
			}}
			return next(ctx, call)
		},
	}, OnEvent: func(e Event) { events = append(events, e) }})
	_, err := executor.execute(t.Context(), ModelExecution{Execution: Execution{ID: "m", Kind: ExecutionKindModel, Attempt: 1},
		Request: LLMRequest{Messages: []Message{UserMsg("before")}},
	}, func(_ context.Context, call ModelExecution, _ func(Event)) (ModelResult, error) {
		call.Request.Messages[0].Content[0].Text = "mutated"
		call.Request.Messages[0].Content[1].ToolCall.Args[5] = '2'
		call.Request.Messages[0].Metadata["source"] = "executor"
		return ModelResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[1].Type != EventModelRequest {
		t.Fatalf("events=%+v", events)
	}
	request := events[1].ModelRequest
	if request.Messages[0].TextContent() != "effective" || request.Messages[0].Metadata["source"] != "middleware" || string(request.Messages[0].Content[1].ToolCall.Args) != `{"a":1}` {
		t.Fatalf("published request changed: %+v", request)
	}
}

// +case:id=model_request_config_snapshot,desc=`Middleware sets all call options and executor mutates them`,expect=`The observed request is complete, detached and credential-free`
func TestModelRequestConfigSnapshot(t *testing.T) {
	var request *LLMRequest
	temperature, strict := 0.2, true
	schema := map[string]any{"type": "object", "required": []string{"answer"}}
	choice := map[string]any{"type": "tool", "name": "inspect"}
	executor := NewModelExecutor(ModelExecutorConfig{Middlewares: []ModelMiddleware{
		func(ctx context.Context, call ModelExecution, next ModelExecuteFunc) (ModelResult, error) {
			call.Request.Config = &CallConfig{Temperature: &temperature, ThinkingLevel: ThinkingMedium, ThinkingBudget: 1024,
				MaxTokens: 321, APIKey: "secret", SessionID: "session", PromptCacheKey: "prefix", ToolChoice: choice,
				ResponseFormat: &ResponseFormat{Type: ResponseFormatJSONSchema, JSONSchema: &JSONSchema{Name: "answer", Schema: schema, Strict: &strict}},
			}
			return next(ctx, call)
		},
	}, OnEvent: func(event Event) {
		if event.Type == EventModelRequest {
			request = event.ModelRequest
		}
	}})
	model := callOptionFuncModel(func(_ context.Context, _ *LLMRequest, config CallConfig) (*LLMResponse, error) {
		if config.APIKey != "secret" || config.MaxTokens != 321 || config.PromptCacheKey != "prefix" || config.ThinkingLevel != ThinkingMedium {
			t.Fatalf("provider config=%+v", config)
		}
		*config.Temperature = 0.9
		*config.ResponseFormat.JSONSchema.Strict = false
		config.ResponseFormat.JSONSchema.Schema.(map[string]any)["type"] = "string"
		config.ToolChoice.(map[string]any)["name"] = "mutated"
		return &LLMResponse{Message: assistantMsg("done", StopReasonStop)}, nil
	})
	_, err := executor.Call(t.Context(), model, ModelExecution{Execution: Execution{ID: "direct", Kind: ExecutionKindModel, Attempt: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if request == nil || request.Config == nil {
		t.Fatal("missing resolved request")
	}
	config := request.Config
	if config.APIKey != "" || *config.Temperature != 0.2 || !*config.ResponseFormat.JSONSchema.Strict || config.SessionID != "session" || config.ThinkingBudget != 1024 {
		t.Fatalf("snapshot=%+v", config)
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"name":"inspect"`, `"type":"object"`, `"required":["answer"]`} {
		if !strings.Contains(string(data), expected) {
			t.Fatalf("lost %s: %s", expected, data)
		}
	}
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "mutated") {
		t.Fatalf("unsafe snapshot: %s", data)
	}
}
