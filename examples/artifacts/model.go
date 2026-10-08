package main

import (
	"context"
	"encoding/json"

	"github.com/compforge/agentgo"
)

// demoModel makes the example reproducible without a provider or credentials.
// The first turn calls the tool; the second consumes its registered artifact.
type demoModel struct{ requests [][]agentgo.Message }

func (m *demoModel) SupportsTools() bool { return true }
func (m *demoModel) Generate(_ context.Context, messages []agentgo.Message, _ []agentgo.ToolSpec, _ ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
	m.requests = append(m.requests, append([]agentgo.Message(nil), messages...))
	reply := agentgo.Message{Role: agentgo.RoleAssistant, StopReason: agentgo.StopReasonStop,
		Content: []agentgo.ContentBlock{agentgo.TextBlock("Done.")}}
	if len(m.requests) == 1 {
		reply.StopReason = agentgo.StopReasonToolUse
		reply.Content = []agentgo.ContentBlock{{Type: agentgo.ContentToolCall, ToolCall: &agentgo.ToolCall{
			ID: "report-1", Name: "load_report", Args: json.RawMessage(`{}`),
		}}}
	}
	return &agentgo.LLMResponse{Message: reply}, nil
}
func (m *demoModel) GenerateStream(ctx context.Context, messages []agentgo.Message, tools []agentgo.ToolSpec, options ...agentgo.CallOption) (<-chan agentgo.StreamEvent, error) {
	response, err := m.Generate(ctx, messages, tools, options...)
	if err != nil {
		return nil, err
	}
	events := make(chan agentgo.StreamEvent, 1)
	events <- agentgo.StreamEvent{Type: agentgo.StreamEventDone, Message: response.Message, StopReason: response.Message.StopReason}
	close(events)
	return events, nil
}
