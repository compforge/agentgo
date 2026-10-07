package agentgo

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

type lifecycleContextKey struct{}

func TestLifecycleNestedModelAndDerivedContexts(t *testing.T) {
	var calls []Execution
	tool := NewFuncTool("inspect", "inspect", nil, func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		parent, ok := ExecutionFromContext(ctx)
		if !ok || parent.ID != "inspect-1" || ctx.Value(lifecycleContextKey{}) != "tool" {
			t.Errorf("tool context lost: %+v", parent)
		}
		_, err := ExecuteModel(ctx, ModelExecution{Execution: Execution{ID: "inspect-1/model", Kind: ExecutionKindModel, TurnIndex: 1, Attempt: 1}},
			func(ctx context.Context, execution ModelExecution) (ModelResult, error) {
				current, _ := ExecutionFromContext(ctx)
				if current != execution.Execution || execution.ParentID != parent.ID || ctx.Value(lifecycleContextKey{}) != "model" {
					t.Errorf("nested model context = %+v, execution = %+v", current, execution.Execution)
				}
				return ModelResult{Message: assistantMsg("detail", StopReasonStop)}, nil
			})
		return json.RawMessage(`"detail"`), err
	})
	model := callOptionFuncModel(func(ctx context.Context, _ *LLMRequest, _ CallConfig) (*LLMResponse, error) {
		execution, ok := ExecutionFromContext(ctx)
		if !ok || execution.Kind != ExecutionKindModel || ctx.Value(lifecycleContextKey{}) != "model" {
			t.Errorf("provider context lost: %+v", execution)
		}
		if execution.TurnIndex == 1 {
			return &LLMResponse{Message: toolCallMsg(ToolCall{ID: "inspect-1", Name: "inspect", Args: json.RawMessage(`{}`)})}, nil
		}
		return &LLMResponse{Message: assistantMsg("done", StopReasonStop)}, nil
	})
	events := runTestLoop(t, []AgentMessage{UserMsg("inspect")}, AgentContext{Tools: []Tool{tool}}, LoopConfig{
		Model: model,
		ToolMiddlewares: []ToolMiddleware{func(ctx context.Context, execution ToolExecution, next ToolExecuteFunc) (ToolResult, error) {
			current, _ := ExecutionFromContext(ctx)
			if current != execution.Execution {
				t.Errorf("middleware tool context = %+v", current)
			}
			return next(context.WithValue(ctx, lifecycleContextKey{}, "tool"), execution)
		}},
		ModelMiddlewares: []ModelMiddleware{func(ctx context.Context, execution ModelExecution, next ModelExecuteFunc) (ModelResult, error) {
			calls = append(calls, execution.Execution)
			return next(context.WithValue(ctx, lifecycleContextKey{}, "model"), execution)
		}},
	})
	if len(calls) != 3 || calls[1].ID != "inspect-1/model" || calls[1].ParentID != "inspect-1" {
		t.Fatalf("model middleware calls = %+v", calls)
	}
	if countEvent(events, EventModelExecStart) != 3 || countEvent(events, EventModelExecEnd) != 3 {
		t.Fatal("nested call missing model lifecycle")
	}
	for _, ev := range events {
		if ev.Timestamp.IsZero() {
			t.Errorf("missing timestamp on %s", ev.Type)
		}
	}
	end, _ := findEvent(events, EventAgentEnd)
	if end.Err != nil || end.Summary.TurnCount != 2 {
		t.Fatalf("end = %+v", end)
	}
}

func TestLifecycleToolOutcomes(t *testing.T) {
	for _, name := range []string{"invoked", "denied", "short_circuited", "middleware_error", "panic", "cancelled_gate", "skipped"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := make(chan Event, 32)
			sink := eventSink{ctx: ctx, ch: ch}
			called := 0
			tool := NewFuncTool("work", "work", nil, func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
				called++
				if ctx.Value(lifecycleContextKey{}) != "derived" {
					t.Error("derived context lost")
				}
				if string(args) != `{"approved":true}` {
					t.Errorf("args = %s", args)
				}
				if name == "panic" {
					panic("tool failed")
				}
				return json.RawMessage(`"ok"`), nil
			})
			cfg := LoopConfig{
				ToolMiddlewares: []ToolMiddleware{func(ctx context.Context, execution ToolExecution, next ToolExecuteFunc) (ToolResult, error) {
					if name == "short_circuited" {
						return ToolResult{Content: json.RawMessage(`"cached"`)}, nil
					}
					if name == "middleware_error" {
						return ToolResult{}, errors.New("middleware failed")
					}
					return next(context.WithValue(ctx, lifecycleContextKey{}, "derived"), execution)
				}},
				ToolGate: func(ctx context.Context, _ GateRequest) (*GateDecision, error) {
					execution, _ := ExecutionFromContext(ctx)
					if execution.ID != "work-1" || ctx.Value(lifecycleContextKey{}) != "derived" {
						t.Error("gate context lost")
					}
					if name == "cancelled_gate" {
						cancel()
					}
					return &GateDecision{Allowed: name != "denied", UpdatedArgs: json.RawMessage(`{"approved":true}`)}, nil
				},
			}
			if name == "skipped" {
				cancel()
			}
			results, _ := executeToolCalls(ctx, 1, []Tool{tool}, []ToolCall{{ID: "work-1", Name: "work", Args: json.RawMessage(`{}`)}}, cfg, map[string]int{}, sink)
			close(ch)
			events := collectEvents(ch)
			want := ToolInvoked
			switch name {
			case "denied":
				want = ToolRejected
			case "short_circuited", "middleware_error":
				want = ToolShortCircuited
			case "cancelled_gate", "skipped":
				want = ToolSkipped
			}
			end, ok := findEvent(events, EventToolExecEnd)
			if !ok || end.Disposition != want || len(results) != 1 || results[0].ToolCallID != "work-1" {
				t.Fatalf("end = %+v, results = %+v", end, results)
			}
			if countEvent(events, EventToolQueued) != 1 || countEvent(events, EventToolExecStart) != 1 || countEvent(events, EventToolExecEnd) != 1 {
				t.Fatal("tool lifecycle not paired")
			}
			wantCalls := 0
			if want == ToolInvoked {
				wantCalls = 1
			}
			if called != wantCalls || countEvent(events, EventToolInvokeStart) != wantCalls || countEvent(events, EventToolInvokeEnd) != wantCalls {
				t.Fatalf("invocation facts disagree: calls=%d events=%+v", called, events)
			}
			if wantCalls > 0 {
				start, _ := findEvent(events, EventToolInvokeStart)
				if string(start.Args) != `{"approved":true}` {
					t.Errorf("invoke args = %s", start.Args)
				}
			}
			if name == "panic" {
				invoked, _ := findEvent(events, EventToolInvokeEnd)
				if !results[0].IsError || end.Err == nil || invoked.Err == nil {
					t.Fatal("panic missing failure outcome")
				}
			}
		})
	}
}

type lifecycleManager struct {
	projectionCommitManager
	failure string
}

func (m lifecycleManager) Transform(ctx context.Context, messages []AgentMessage) ([]AgentMessage, error) {
	if m.failure == "panic" {
		panic("projection failed")
	}
	if m.failure == "error" {
		return nil, errors.New("projection failed")
	}
	return messages, nil
}
func (m lifecycleManager) RecoverOverflow(ctx context.Context, messages []AgentMessage, cause error) (ContextRecoveryResult, error) {
	if m.failure == "overflow" {
		return ContextRecoveryResult{}, errors.New("recovery failed")
	}
	return ContextRecoveryResult{View: messages}, nil
}

func TestLifecycleFailedTurnDoesNotCommitProgress(t *testing.T) {
	for _, name := range []string{"before_turn", "model_error", "model_panic", "context_error", "context_panic", "overflow", "commit"} {
		t.Run(name, func(t *testing.T) {
			cfg := LoopConfig{Model: mockModel(assistantMsg("done", StopReasonStop))}
			afterTurns := 0
			cfg.AfterTurn = func(context.Context, AfterTurnContext) error { afterTurns++; return nil }
			switch name {
			case "before_turn":
				cfg.BeforeTurn = func(context.Context, BeforeTurnContext) ([]AgentMessage, error) {
					return nil, errors.New("before failed")
				}
			case "model_error", "model_panic", "overflow":
				cfg.Model = sequentialModel(func(int, *LLMRequest) (*LLMResponse, error) {
					if name == "model_panic" {
						panic("provider failed")
					}
					if name == "overflow" {
						return nil, ErrContextOverflow
					}
					return nil, errors.New("provider failed")
				})
				if name == "overflow" {
					cfg.ContextManager = lifecycleManager{failure: "overflow"}
				}
			case "context_error":
				cfg.ContextManager = lifecycleManager{failure: "error"}
			case "context_panic":
				cfg.ContextManager = lifecycleManager{failure: "panic"}
			case "commit":
				cfg.CommitMessage = func(message AgentMessage) error {
					if modelMessage, include := message.ToMessage(); include && modelMessage.Role == RoleAssistant {
						return errors.New("commit failed")
					}
					return nil
				}
			}
			events := runTestLoop(t, []AgentMessage{UserMsg("hi")}, AgentContext{}, cfg)
			turnEnd, ok := findEvent(events, EventTurnEnd)
			if !ok || turnEnd.Err == nil || turnEnd.State != nil || turnEnd.TurnIndex != 1 || afterTurns != 0 {
				t.Fatalf("turn end = %+v; afterTurns=%d", turnEnd, afterTurns)
			}
			end, _ := findEvent(events, EventAgentEnd)
			if end.Err == nil || end.State.Progress.CompletedTurns != 0 || end.Timestamp.Before(turnEnd.Timestamp) {
				t.Fatalf("agent end = %+v", end)
			}
			if countEvent(events, EventTurnStart) != 1 || countEvent(events, EventTurnEnd) != 1 {
				t.Fatal("unpaired turn")
			}
			if countEvent(events, EventModelExecStart) != countEvent(events, EventModelExecEnd) {
				t.Fatal("unpaired model attempt")
			}
			if countEvent(events, EventContextPrepareStart) != countEvent(events, EventContextPrepareEnd) {
				t.Fatal("unpaired context preparation")
			}
			if name == "context_error" || name == "context_panic" || name == "overflow" {
				var last Event
				for _, ev := range events {
					if ev.Type == EventContextPrepareEnd {
						last = ev
					}
				}
				if last.Err == nil {
					t.Fatal("missing preparation failure")
				}
				if name == "overflow" && last.ContextOperation != ContextRecoverOverflow {
					t.Errorf("operation = %s", last.ContextOperation)
				}
			}
		})
	}
}

type lifecycleRetryError struct{ delay time.Duration }

func (e lifecycleRetryError) Error() string             { return "retryable" }
func (e lifecycleRetryError) Retryable() bool           { return true }
func (e lifecycleRetryError) RetryAfter() time.Duration { return e.delay }

func TestLifecycleRetryWait(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "cancel_wait"}[abort], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			attempts := 0
			model := sequentialModel(func(i int, _ *LLMRequest) (*LLMResponse, error) {
				attempts++
				if i == 0 {
					delay := time.Millisecond
					if abort {
						delay = time.Minute
					}
					return nil, lifecycleRetryError{delay}
				}
				return &LLMResponse{Message: assistantMsg("done", StopReasonStop)}, nil
			})
			var events []Event
			for ev := range AgentLoop(ctx, []AgentMessage{UserMsg("hi")}, AgentContext{}, LoopConfig{Model: model, MaxRetries: 1}) {
				events = append(events, ev)
				if abort && ev.Type == EventRetryWaitStart {
					cancel()
				}
			}
			start, _ := findEvent(events, EventRetryWaitStart)
			end, ok := findEvent(events, EventRetryWaitEnd)
			if !ok || start.Execution == nil || end.Execution == nil || *start.Execution != *end.Execution || end.Timestamp.Before(start.Timestamp) {
				t.Fatalf("wait start/end = %+v / %+v", start, end)
			}
			wantAttempts := 2
			if abort {
				wantAttempts = 1
				if !errors.Is(end.Err, context.Canceled) {
					t.Fatalf("wait error = %v", end.Err)
				}
			}
			if attempts != wantAttempts {
				t.Fatalf("attempts=%d", attempts)
			}
			var got []int
			for _, ev := range events {
				if ev.Type == EventModelExecStart {
					if ev.Execution.ID != "model-1" {
						t.Error("retry changed logical ID")
					}
					got = append(got, ev.Execution.Attempt)
				}
			}
			if !abort && !slices.Equal(got, []int{1, 2}) {
				t.Errorf("attempt coordinates = %v", got)
			}
			if countEvent(events, EventTurnStart) != 1 || countEvent(events, EventTurnEnd) != 1 {
				t.Fatal("retry split logical turn")
			}
		})
	}
}

func TestLifecycleSourceTimestampAndCommittedTurn(t *testing.T) {
	// Completion precedes consumption: buffered events retain their source time.
	ch := make(chan Event, 1)
	sink := eventSink{ctx: context.Background(), ch: ch}
	sink.emit(Event{Type: EventModelExecEnd})
	afterEmission := time.Now()
	ev := <-ch
	if ev.Timestamp.IsZero() || ev.Timestamp.After(afterEmission) {
		t.Fatalf("timestamp = %v", ev.Timestamp)
	}

	events := runTestLoop(t, []AgentMessage{UserMsg("hi")}, AgentContext{}, LoopConfig{
		Model:     mockModel(assistantMsg("done", StopReasonStop)),
		AfterTurn: func(context.Context, AfterTurnContext) error { return errors.New("save failed") },
	})
	turn, _ := findEvent(events, EventTurnEnd)
	end, _ := findEvent(events, EventAgentEnd)
	if countEvent(events, EventTurnEnd) != 1 || turn.State == nil || turn.Err != nil || turn.State.Progress.CompletedTurns != 1 || end.Err == nil {
		t.Fatalf("turn=%+v end=%+v", turn, end)
	}
}

type lifecycleParallelTool struct{ Tool }

func (lifecycleParallelTool) ConcurrencySafe(json.RawMessage) bool { return true }

func TestLifecycleConcurrentCompletionPreservesTranscriptOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release := make(chan struct{})
	tool := lifecycleParallelTool{NewFuncTool("work", "work", nil, func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		execution, _ := ExecutionFromContext(ctx)
		if execution.ID == "slow" {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return json.RawMessage(`"ok"`), nil
	})}
	var completed []string
	var results []ToolResult
	for ev := range AgentLoop(ctx, []AgentMessage{UserMsg("hi")}, AgentContext{Tools: []Tool{tool}}, LoopConfig{
		MaxToolConcurrency: 2,
		Model:              mockModel(toolCallMsg(ToolCall{ID: "slow", Name: "work", Args: json.RawMessage(`{}`)}, ToolCall{ID: "fast", Name: "work", Args: json.RawMessage(`{}`)}), assistantMsg("done", StopReasonStop)),
	}) {
		if ev.Type == EventToolExecEnd {
			completed = append(completed, ev.ToolID)
			if ev.ToolID == "fast" {
				close(release)
			}
		}
		if ev.Type == EventTurnEnd && ev.TurnIndex == 1 {
			results = ev.ToolResults
		}
	}
	if ctx.Err() != nil {
		t.Fatal("parallel tool execution stalled")
	}
	if !slices.Equal(completed, []string{"fast", "slow"}) {
		t.Fatalf("completion order = %v", completed)
	}
	if len(results) != 2 || results[0].ToolCallID != "slow" || results[1].ToolCallID != "fast" {
		t.Fatalf("transcript result order = %+v", results)
	}
}

func TestLifecycleStatefulFailedTurnAndCompletedContinuation(t *testing.T) {
	agent := NewAgent(WithModel(funcModel(func(context.Context, *LLMRequest) (*LLMResponse, error) { return nil, errors.New("provider failed") })))
	var events []Event
	agent.Subscribe(func(ev Event) { events = append(events, ev) })
	if err := agent.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()
	turn, ok := findEvent(events, EventTurnEnd)
	if !ok || turn.State != nil || turn.Err == nil {
		t.Fatalf("stateful failed turn = %+v", turn)
	}
	state := agent.State()
	if state.Progress.CompletedTurns != 0 || state.IsRunning || len(state.Messages) != 1 {
		t.Fatalf("failed turn changed state: %+v", state)
	}

	events = collectEvents(AgentLoopContinue(context.Background(), AgentContext{Messages: []AgentMessage{UserMsg("hi")}}, LoopConfig{
		Model: mockModel(), InitialState: AgentState{Progress: RunProgress{Active: true, CompletedTurns: 1}},
	}))
	if countEvent(events, EventTurnStart) != 0 || countEvent(events, EventTurnEnd) != 0 || countEvent(events, EventModelExecStart) != 0 {
		t.Fatal("completed continuation started phantom work")
	}
}

func TestLifecycleAfterRunErrorTimestamp(t *testing.T) {
	agent := NewAgent(WithModel(mockModel(assistantMsg("done", StopReasonStop))), WithAfterRun(func(context.Context, AfterRunContext) error { return errors.New("save failed") }))
	var events []Event
	agent.Subscribe(func(ev Event) { events = append(events, ev) })
	if err := agent.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()
	ev, ok := findEvent(events, EventError)
	if !ok || ev.Timestamp.IsZero() {
		t.Fatalf("after-run error missing source time: %+v", ev)
	}
	end, _ := findEvent(events, EventAgentEnd)
	if end.Err == nil || end.Timestamp.After(ev.Timestamp) {
		t.Fatalf("loop completion should precede outer hook error: %+v", end)
	}
}
