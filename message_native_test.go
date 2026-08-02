package agentgo

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type applicationMessage struct {
	text    string
	include bool
}

type applicationToolResult struct {
	call   ToolCall
	result ToolResult
}

type applicationToolReference struct{ name string }

func (m applicationToolReference) GetRole() Role           { return RoleAssistant }
func (m applicationToolReference) GetTimestamp() time.Time { return time.Time{} }
func (m applicationToolReference) Raw() AgentMessage       { return m }
func (m applicationToolReference) Priority() int           { return 0 }
func (m applicationToolReference) Compact(float64) (AgentMessage, float64) {
	return m, 1
}
func (m applicationToolReference) TextContent() string     { return "" }
func (m applicationToolReference) ThinkingContent() string { return "" }
func (m applicationToolReference) HasToolCalls() bool      { return false }
func (m applicationToolReference) ToMessage() (Message, bool) {
	return Message{Role: RoleAssistant, Content: []ContentBlock{ToolRefBlock(m.name)}}, true
}

type recordingDeferTool struct{ activated []string }

func (t *recordingDeferTool) Name() string             { return "tool_search" }
func (t *recordingDeferTool) Description() string      { return "tool search" }
func (t *recordingDeferTool) Schema() map[string]any   { return map[string]any{} }
func (t *recordingDeferTool) IsDeferred(string) bool   { return false }
func (t *recordingDeferTool) WasDeferred(string) bool  { return true }
func (t *recordingDeferTool) Activate(names ...string) { t.activated = append(t.activated, names...) }
func (t *recordingDeferTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

func (m applicationToolResult) GetRole() Role           { return RoleTool }
func (m applicationToolResult) GetTimestamp() time.Time { return time.Time{} }
func (m applicationToolResult) Raw() AgentMessage       { return m }
func (m applicationToolResult) Priority() int           { return 0 }
func (m applicationToolResult) Compact(float64) (AgentMessage, float64) {
	return m, 1
}
func (m applicationToolResult) TextContent() string     { return string(m.result.Content) }
func (m applicationToolResult) ThinkingContent() string { return "" }
func (m applicationToolResult) HasToolCalls() bool      { return false }
func (m applicationToolResult) ToMessage() (Message, bool) {
	return ToolResultMsg(m.call.ID, m.result.Content, m.result.IsError), true
}

func (m applicationMessage) GetRole() Role           { return RoleUser }
func (m applicationMessage) GetTimestamp() time.Time { return time.Time{} }
func (m applicationMessage) Raw() AgentMessage       { return m }
func (m applicationMessage) Priority() int           { return 0 }
func (m applicationMessage) Compact(float64) (AgentMessage, float64) {
	return m, 1
}
func (m applicationMessage) TextContent() string     { return m.text }
func (m applicationMessage) ThinkingContent() string { return "" }
func (m applicationMessage) HasToolCalls() bool      { return false }
func (m applicationMessage) ToMessage() (Message, bool) {
	return Message{Role: RoleUser, Content: []ContentBlock{TextBlock(m.text)}}, m.include
}

func TestToMessagesUsesApplicationProjection(t *testing.T) {
	messages := []AgentMessage{
		applicationMessage{text: "domain context", include: true},
		applicationMessage{text: "ui only", include: false},
		AbortMsg("interrupted", "inference"),
	}

	got := ToMessages(messages)
	if len(got) != 1 || got[0].TextContent() != "domain context" {
		t.Fatalf("ToMessages() = %#v, want only projected domain message", got)
	}
}

func TestReactivateDeferredUsesApplicationProjection(t *testing.T) {
	tool := &recordingDeferTool{}
	ReactivateDeferred([]Tool{tool}, []AgentMessage{applicationToolReference{name: "read"}})

	if len(tool.activated) != 1 || tool.activated[0] != "read" {
		t.Fatalf("activated = %v, want [read]", tool.activated)
	}
}

func TestAgentLoopLowersApplicationMessageAtModelBoundary(t *testing.T) {
	model := funcModel(func(_ context.Context, request *LLMRequest) (*LLMResponse, error) {
		if len(request.Messages) != 1 || request.Messages[0].TextContent() != "domain prompt" {
			t.Fatalf("model messages = %#v", request.Messages)
		}
		return &LLMResponse{Message: assistantMsg("done", StopReasonStop)}, nil
	})

	events := runTestLoop(t,
		[]AgentMessage{applicationMessage{text: "domain prompt", include: true}},
		AgentContext{},
		LoopConfig{Model: model},
	)
	end, ok := findEvent(events, EventAgentEnd)
	if !ok || len(end.NewMessages) != 2 {
		t.Fatalf("agent end = %#v", end)
	}
	if _, ok := end.NewMessages[0].(applicationMessage); !ok {
		t.Fatalf("prompt was lowered before transcript commit: %T", end.NewMessages[0])
	}
}

func TestToolResultMessageFactoryKeepsApplicationMessage(t *testing.T) {
	want := applicationMessage{text: "typed tool result", include: true}
	got := toolResultMessage(LoopConfig{
		ToolResultMessageFactory: func(call ToolCall, result ToolResult) AgentMessage {
			if call.Name != "read" || string(call.Args) != `{"path":"a.go"}` || result.ToolCallID != call.ID {
				t.Fatalf("factory input: call=%#v result=%#v", call, result)
			}
			return want
		},
	}, ToolCall{ID: "call-1", Name: "read", Args: []byte(`{"path":"a.go"}`)}, ToolResult{ToolCallID: "call-1"})

	if got != want {
		t.Fatalf("tool result message = %#v, want %#v", got, want)
	}
}

func TestAgentLoopKeepsApplicationToolResultInTranscript(t *testing.T) {
	var calls []string
	call := ToolCall{ID: "call-1", Name: "echo", Args: []byte(`{"value":"ping"}`)}
	model := sequentialModel(func(i int, request *LLMRequest) (*LLMResponse, error) {
		switch i {
		case 0:
			return &LLMResponse{Message: toolCallMsg(call)}, nil
		case 1:
			if len(request.Messages) < 3 || request.Messages[len(request.Messages)-1].Role != RoleTool {
				t.Fatalf("second request lost projected tool result: %#v", request.Messages)
			}
			return &LLMResponse{Message: assistantMsg("done", StopReasonStop)}, nil
		default:
			t.Fatalf("unexpected model call %d", i)
			return nil, nil
		}
	})

	events := runTestLoop(t,
		[]AgentMessage{UserMsg("test")},
		AgentContext{Tools: []Tool{echoTool(&calls)}},
		LoopConfig{
			Model: model,
			ToolResultMessageFactory: func(call ToolCall, result ToolResult) AgentMessage {
				return applicationToolResult{call: call, result: result}
			},
		},
	)

	end, ok := findEvent(events, EventAgentEnd)
	if !ok {
		t.Fatal("missing agent_end")
	}
	found := false
	for _, message := range end.NewMessages {
		if _, ok := message.(applicationToolResult); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("custom tool result missing from transcript: %#v", end.NewMessages)
	}
}
