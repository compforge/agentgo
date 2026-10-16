package session_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/compforge/agentgo"
	"github.com/compforge/agentgo/session"
)

// A model stub keeps this example executable without network access.
type exampleModel struct{}

func (exampleModel) SupportsTools() bool { return false }
func (exampleModel) Generate(context.Context, []agentgo.Message, []agentgo.ToolSpec, ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
	return &agentgo.LLMResponse{Message: agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("done")}, StopReason: agentgo.StopReasonStop}}, nil
}
func (m exampleModel) GenerateStream(ctx context.Context, messages []agentgo.Message, tools []agentgo.ToolSpec, opts ...agentgo.CallOption) (<-chan agentgo.StreamEvent, error) {
	response, err := m.Generate(ctx, messages, tools, opts...)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentgo.StreamEvent, 1)
	ch <- agentgo.StreamEvent{Type: agentgo.StreamEventDone, Message: response.Message}
	close(ch)
	return ch, nil
}

func recordExample(path string) (ref session.Ref, err error) {
	s, err := session.Create(path, nil)
	if err != nil {
		return ref, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	run, err := s.BeginRun()
	if err != nil {
		return ref, err
	}
	var recordErr error
	for event := range agentgo.AgentLoop(context.Background(), []agentgo.AgentMessage{agentgo.UserMsg("hello")}, agentgo.AgentContext{}, agentgo.LoopConfig{Model: exampleModel{}}) {
		if recordErr == nil {
			recordErr = run.Record(event)
		}
		if event.Type == agentgo.EventModelRequest {
			ref = run.Ref(*event.Execution)
		}
	}
	return ref, errors.Join(recordErr, run.Close())
}

func Example() {
	dir, err := os.MkdirTemp("", "agentgo-session-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "loop.jsonl")
	ref, err := recordExample(path)
	if err != nil {
		panic(err)
	}
	file, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	requests := 0
	_, err = session.Read(file, func(record session.Record) error {
		if record.Type == string(agentgo.EventModelRequest) && record.SessionID == ref.SessionID && record.RunID == ref.RunID && record.Execution.ID == ref.ExecutionID && record.Execution.Attempt == ref.Attempt {
			requests++
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("linked model requests: %d\n", requests)
	// Output: linked model requests: 1
}
