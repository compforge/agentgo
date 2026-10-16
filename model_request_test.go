package agentgo

import (
	"context"
	"encoding/json"
	"testing"
)

// +case:id=model_request_after_middleware,desc=`Middleware rewrites a request and executor mutates its containers`,expect=`Published request retains the actual pre-executor input`
func TestModelRequestAfterMiddleware(t *testing.T) {
	var events []Event
	ctx := withModelExecutionRuntime(context.Background(), []ModelMiddleware{
		func(ctx context.Context, call ModelExecution, next ModelExecuteFunc) (ModelResult, error) {
			call.Request.Messages = []Message{{Role: RoleUser,
				Content:  []ContentBlock{TextBlock("effective"), ToolCallBlock(ToolCall{ID: "c", Args: json.RawMessage(`{"a":1}`)})},
				Metadata: map[string]any{"source": "middleware"},
			}}
			return next(ctx, call)
		},
	}, func(e Event) { events = append(events, e) }, nil)
	_, err := ExecuteModel(ctx, ModelExecution{Execution: Execution{ID: "m", Kind: ExecutionKindModel, Attempt: 1},
		Request: LLMRequest{Messages: []Message{UserMsg("before")}},
	}, func(_ context.Context, call ModelExecution) (ModelResult, error) {
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
