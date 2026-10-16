package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/compforge/agentgo"
	"github.com/compforge/agentgo/codec"
	agentcontext "github.com/compforge/agentgo/context"
)

type scriptModel func(context.Context, []agentgo.Message) (agentgo.Message, error)

func (m scriptModel) SupportsTools() bool { return true }

func (m scriptModel) Generate(ctx context.Context, messages []agentgo.Message, _ []agentgo.ToolSpec, _ ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
	message, err := m(ctx, messages)
	return &agentgo.LLMResponse{Message: message}, err
}
func (m scriptModel) GenerateStream(ctx context.Context, messages []agentgo.Message, _ []agentgo.ToolSpec, _ ...agentgo.CallOption) (<-chan agentgo.StreamEvent, error) {
	message, err := m(ctx, messages)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentgo.StreamEvent, 2)
	ch <- agentgo.StreamEvent{Type: agentgo.StreamEventTextDelta, Delta: "partial", Message: message}
	ch <- agentgo.StreamEvent{Type: agentgo.StreamEventDone, Message: message}
	close(ch)
	return ch, nil
}
func answer(text string) agentgo.Message {
	return agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock(text)}, StopReason: agentgo.StopReasonStop}
}

type richTool struct{ *agentgo.FuncTool }

func (t richTool) ExecuteContent(ctx context.Context, args json.RawMessage) ([]agentgo.ContentBlock, error) {
	if string(args) != `{"approved":true}` {
		return nil, errors.New("gate rewrite lost")
	}
	_, err := agentgo.ExecuteModel(ctx, agentgo.ModelExecution{
		Execution: agentgo.Execution{ID: "inspect-1/nested", Kind: agentgo.ExecutionKindModel, Attempt: 1, TurnIndex: 1},
		Request:   agentgo.LLMRequest{Messages: []agentgo.Message{agentgo.UserMsg("nested analysis")}},
	}, func(context.Context, agentgo.ModelExecution) (agentgo.ModelResult, error) {
		return agentgo.ModelResult{Message: answer("nested response")}, nil
	})
	return []agentgo.ContentBlock{agentgo.TextBlock("tool output"), agentgo.ImageURLBlock("https://example.test/image.png")}, err
}

func decodeEvent(t *testing.T, r Record) EventData {
	t.Helper()
	var data EventData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

// +case:id=session_loop_transcript,desc=`Model middleware, tool authorization rewrite and nested model call`,expect=`Actual requests, rich outputs and stable references survive JSONL round trip`
func TestLoopRecordsActualRequestsAndResults(t *testing.T) {
	s, path := newTestSession(t)
	run := mustRun(t, s)
	model := scriptModel(func(ctx context.Context, messages []agentgo.Message) (agentgo.Message, error) {
		execution, _ := agentgo.ExecutionFromContext(ctx)
		if messages[len(messages)-1].TextContent() != "middleware input" {
			return agentgo.Message{}, errors.New("middleware request missing")
		}
		if execution.TurnIndex == 1 {
			return agentgo.Message{Role: agentgo.RoleAssistant, StopReason: agentgo.StopReasonToolUse,
				Content: []agentgo.ContentBlock{agentgo.ToolCallBlock(agentgo.ToolCall{ID: "inspect-1", Name: "inspect", Args: json.RawMessage(`{}`)})}}, nil
		}
		return answer("complete"), nil
	})
	tool := richTool{agentgo.NewFuncTool("inspect", "inspect", nil, nil)}
	var refs []Ref
	for event := range agentgo.AgentLoop(context.Background(), []agentgo.AgentMessage{agentgo.UserMsg("inspect")}, agentgo.AgentContext{Tools: []agentgo.Tool{tool}}, agentgo.LoopConfig{
		Model: model,
		ModelMiddlewares: []agentgo.ModelMiddleware{func(ctx context.Context, call agentgo.ModelExecution, next agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
			call.Request.Messages = append(call.Request.Messages, agentgo.UserMsg("middleware input"))
			return next(ctx, call)
		}},
		ToolGate: func(context.Context, agentgo.GateRequest) (*agentgo.GateDecision, error) {
			return &agentgo.GateDecision{Allowed: true, UpdatedArgs: json.RawMessage(`{"approved":true}`)}, nil
		},
	}) {
		if err := run.Record(event); err != nil {
			t.Fatal(err)
		}
		if event.Type == agentgo.EventModelRequest {
			refs = append(refs, run.Ref(*event.Execution))
		}
		if event.Type == agentgo.EventAgentEnd && (event.Err != nil || event.Summary.EndReason != agentgo.EndReasonStop) {
			t.Fatalf("loop failed: %+v", event)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	records := recordsAt(t, path)
	requests, responses, toolResults := 0, 0, 0
	requested, invoked, nested := false, false, false
	for _, record := range records {
		switch agentgo.EventType(record.Type) {
		case agentgo.EventModelRequest:
			data := decodeEvent(t, record)
			if data.ModelRequest == nil || data.ModelRequest.Messages[len(data.ModelRequest.Messages)-1].TextContent() != "middleware input" {
				t.Fatalf("recorded stale request: %s", record.Data)
			}
			ref := Ref{SessionID: record.SessionID, RunID: record.RunID, ExecutionID: record.Execution.ID, Attempt: record.Execution.Attempt}
			if ref != refs[requests] {
				t.Fatalf("reference mismatch: %+v", ref)
			}
			if record.Execution.ID == "inspect-1/nested" {
				nested = record.Execution.ParentID == "inspect-1"
			}
			requests++
		case agentgo.EventModelExecEnd:
			data := decodeEvent(t, record)
			var message agentgo.AgentMessage
			if err := s.codec.Unmarshal(data.Message, &message); err != nil {
				t.Fatal(err)
			}
			if message.GetRole() != agentgo.RoleAssistant {
				t.Fatal("response message lost")
			}
			responses++
		case agentgo.EventToolExecStart:
			requested = string(decodeEvent(t, record).Args) == `{}`
		case agentgo.EventToolInvokeStart:
			invoked = string(decodeEvent(t, record).Args) == `{"approved":true}`
		case agentgo.EventToolExecEnd:
			data := decodeEvent(t, record)
			if data.Disposition != agentgo.ToolInvoked || data.ToolResult == nil || len(data.ToolResult.ContentBlocks) != 2 || data.ToolResult.ContentBlocks[1].Image.URL != "https://example.test/image.png" {
				t.Fatalf("rich tool result lost: %s", record.Data)
			}
			toolResults++
		case agentgo.EventMessageUpdate:
			t.Fatal("stream token persisted")
		}
	}
	if requests != 3 || responses != 3 || toolResults != 1 || !requested || !invoked || !nested {
		t.Fatalf("incomplete transcript: requests=%d responses=%d tools=%d requested=%v invoked=%v nested=%v", requests, responses, toolResults, requested, invoked, nested)
	}
}

func TestRetryAndFailedCallRecords(t *testing.T) {
	s, path := newTestSession(t)
	run := mustRun(t, s)
	attempts := 0
	model := scriptModel(func(context.Context, []agentgo.Message) (agentgo.Message, error) {
		attempts++
		if attempts == 1 {
			return agentgo.Message{}, &agentgo.PartialStreamError{Partial: answer("unfinished")}
		}
		return agentgo.Message{}, errors.New("provider unavailable")
	})
	for event := range agentgo.AgentLoop(context.Background(), []agentgo.AgentMessage{agentgo.UserMsg("question")}, agentgo.AgentContext{}, agentgo.LoopConfig{Model: model, MaxRetries: 1}) {
		if err := run.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	var calls []agentgo.Execution
	failed, ended := 0, false
	for _, r := range recordsAt(t, path) {
		switch agentgo.EventType(r.Type) {
		case agentgo.EventModelRequest:
			calls = append(calls, *r.Execution)
		case agentgo.EventModelExecEnd:
			data := decodeEvent(t, r)
			if data.Error != "" {
				failed++
			}
			if r.Execution.Attempt == 1 {
				var partial agentgo.Message
				if err := s.codec.Unmarshal(data.PartialMessage, &partial); err != nil || partial.TextContent() != "unfinished" {
					t.Fatalf("partial response lost: %s %v", data.PartialMessage, err)
				}
			}
		case agentgo.EventAgentEnd:
			d := decodeEvent(t, r)
			ended = d.Summary.EndReason == agentgo.EndReasonError && strings.Contains(d.Error, "provider unavailable")
		}
	}
	if len(calls) != 2 || calls[0].ID != calls[1].ID || calls[0].Attempt != 1 || calls[1].Attempt != 2 || failed != 2 || !ended {
		t.Fatalf("retry facts lost: %+v failed=%d ended=%v", calls, failed, ended)
	}
	if _, ok, _ := s.LoadCheckpoint(); ok {
		t.Fatal("failed call advanced recovery state")
	}
}

func TestShortCircuitHasResponseWithoutProviderRequest(t *testing.T) {
	s, path := newTestSession(t)
	run := mustRun(t, s)
	for event := range agentgo.AgentLoop(context.Background(), []agentgo.AgentMessage{agentgo.UserMsg("cached")}, agentgo.AgentContext{}, agentgo.LoopConfig{
		ModelMiddlewares: []agentgo.ModelMiddleware{func(context.Context, agentgo.ModelExecution, agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
			return agentgo.ModelResult{Message: answer("cached response")}, nil
		}},
	}) {
		if err := run.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	responses := 0
	for _, r := range recordsAt(t, path) {
		if r.Type == string(agentgo.EventModelRequest) {
			t.Fatal("invented provider request")
		}
		if r.Type == string(agentgo.EventModelExecEnd) {
			responses++
		}
	}
	if responses != 1 {
		t.Fatal("missing short-circuit response")
	}
}

func TestStatefulAgentRecordsAfterRunAndContextMessages(t *testing.T) {
	c, err := agentgo.NewCodec(append(agentcontext.CodecOptions(), codec.Type[testArtifact]("test.artifact"))...)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Create(t.TempDir()+"/stateful.jsonl", c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run := mustRun(t, s)
	a := agentgo.NewAgent(agentgo.WithModel(scriptModel(func(context.Context, []agentgo.Message) (agentgo.Message, error) {
		return answer("done"), nil
	})), agentgo.WithAfterRun(func(_ context.Context, run agentgo.AfterRunContext) error {
		return run.Artifacts.AddArtifact(testArtifact{Key: "final", Text: "after run"}, false)
	}))
	var recordErr error
	a.Subscribe(func(event agentgo.Event) {
		if err := run.Record(event); err != nil {
			recordErr = err
		}
		if event.Type == agentgo.EventAgentEnd {
			if err := run.SaveCheckpoint(a.Snapshot()); err != nil {
				recordErr = err
			}
		}
	})
	if err := a.PromptMessages(context.Background(), agentcontext.ContextSummary{Summary: "compact question", RawMessages: []agentgo.AgentMessage{agentgo.UserMsg("original question")}}); err != nil {
		t.Fatal(err)
	}
	a.WaitForIdle()
	if recordErr != nil {
		t.Fatal(recordErr)
	}
	snapshot, ok, err := s.LoadCheckpoint()
	if err != nil || !ok || len(snapshot.State.Artifacts) != 1 || snapshot.State.Artifacts[0].(testArtifact).Text != "after run" {
		t.Fatalf("final snapshot missing hook writes: %+v %v", snapshot, err)
	}
	if summary, ok := snapshot.State.Messages[0].(agentcontext.ContextSummary); !ok || summary.RawMessages[0].TextContent() != "original question" {
		t.Fatal("context summary or raw message lost")
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}
