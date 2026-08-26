package agentgo

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
)

func TestAgentStateMarshalRoundTripDurableProjection(t *testing.T) {
	original := AgentState{
		SystemPrompt:     "runtime config",
		Messages:         []AgentMessage{UserMsg("hello"), assistantMsg("world", StopReasonStop)},
		Tools:            []Tool{NewFuncTool("noop", "noop", nil, nil)},
		IsRunning:        true,
		StreamMessage:    assistantMsg("partial", ""),
		PendingToolCalls: map[string]struct{}{"call-1": {}},
		TotalUsage:       Usage{Input: 10, Output: 4, TotalTokens: 14},
		Progress: RunProgress{
			Active:                true,
			NextTurn:              true,
			CompletedTurns:        3,
			LengthRecoveries:      1,
			ToolCalls:             2,
			ToolErrors:            1,
			ConsecutiveToolErrors: map[string]int{"read": 1},
			PendingMessages:       []AgentMessage{UserMsg("continue")},
		},
		Error: "runtime error",
	}

	data, err := original.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var restored AgentState
	if err := restored.Unmarshal(data); err != nil {
		t.Fatal(err)
	}

	if len(restored.Messages) != 2 || restored.Messages[0].TextContent() != "hello" || restored.Messages[1].TextContent() != "world" {
		t.Fatalf("restored messages = %#v", restored.Messages)
	}
	if !reflect.DeepEqual(restored.TotalUsage, original.TotalUsage) {
		t.Fatalf("restored usage = %#v, want %#v", restored.TotalUsage, original.TotalUsage)
	}
	if restored.Progress.CompletedTurns != 3 || !restored.Progress.Active || !restored.Progress.NextTurn {
		t.Fatalf("restored progress = %#v", restored.Progress)
	}
	if got := restored.Progress.PendingMessages[0].TextContent(); got != "continue" {
		t.Fatalf("pending message = %q", got)
	}
	if restored.SystemPrompt != "" || restored.Tools != nil || restored.IsRunning || restored.StreamMessage != nil || restored.PendingToolCalls != nil || restored.Error != "" {
		t.Fatalf("runtime fields leaked into durable state: %#v", restored)
	}
}

func TestAgentStateMarshalRejectsUnknownAgentMessage(t *testing.T) {
	state := AgentState{Messages: []AgentMessage{applicationMessage{text: "domain message", include: true}}}
	if _, err := state.Marshal(); err == nil {
		t.Fatal("expected unsupported AgentMessage error")
	}
}

func TestAgentLoopRunHooksExecuteOnce(t *testing.T) {
	var order []string
	var checkpoints []AgentState
	events := runTestLoop(t,
		[]AgentMessage{UserMsg("question")},
		AgentContext{},
		LoopConfig{
			Model: mockModel(
				assistantMsg("truncated", StopReasonLength),
				assistantMsg("done", StopReasonStop),
			),
			BeforeRun: func(_ context.Context, run BeforeRunContext) (AgentState, error) {
				order = append(order, "before_run")
				if !run.State.Progress.Active {
					t.Fatal("run must be active before BeforeRun")
				}
				return run.State, nil
			},
			AfterTurn: func(_ context.Context, turn AfterTurnContext) error {
				order = append(order, "after_turn")
				checkpoints = append(checkpoints, turn.State)
				if turn.State.Progress.CompletedTurns != turn.TurnIndex {
					t.Fatalf("checkpoint turn = %d, want %d", turn.State.Progress.CompletedTurns, turn.TurnIndex)
				}
				return nil
			},
			AfterRun: func(_ context.Context, run AfterRunContext) error {
				order = append(order, "after_run")
				if run.State.Progress.Active {
					t.Fatal("final state must be inactive")
				}
				return nil
			},
		},
	)

	if !slices.Equal(order, []string{"before_run", "after_turn", "after_turn", "after_run"}) {
		t.Fatalf("lifecycle order = %v", order)
	}
	if len(checkpoints) != 2 || !checkpoints[0].Progress.NextTurn || len(checkpoints[0].Progress.PendingMessages) != 1 {
		t.Fatalf("first checkpoint does not carry length continuation: %#v", checkpoints)
	}
	if checkpoints[1].Progress.NextTurn || len(checkpoints[1].Progress.PendingMessages) != 0 {
		t.Fatalf("final checkpoint unexpectedly continues: %#v", checkpoints[1].Progress)
	}
	end, ok := findEvent(events, EventAgentEnd)
	if !ok || end.State == nil || end.State.Progress.Active {
		t.Fatalf("terminal event state = %#v", end.State)
	}
}

func TestAgentLoopTerminalModelMessageProducesCheckpoint(t *testing.T) {
	tests := []struct {
		name       string
		stopReason StopReason
		endReason  EndReason
	}{
		{name: "error", stopReason: StopReasonError, endReason: EndReasonError},
		{name: "aborted", stopReason: StopReasonAborted, endReason: EndReasonAborted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var checkpoint AgentState
			var final AfterRunContext
			events := runTestLoop(t,
				[]AgentMessage{UserMsg("question")},
				AgentContext{},
				LoopConfig{
					Model: mockModel(assistantMsg("terminal", tt.stopReason)),
					AfterTurn: func(_ context.Context, turn AfterTurnContext) error {
						checkpoint = turn.State
						return nil
					},
					AfterRun: func(_ context.Context, run AfterRunContext) error {
						final = run
						return nil
					},
				},
			)

			if checkpoint.Progress.CompletedTurns != 1 || checkpoint.Progress.NextTurn {
				t.Fatalf("terminal checkpoint progress = %#v", checkpoint.Progress)
			}
			if final.State.Progress.CompletedTurns != 1 || final.Summary.TurnCount != 1 || final.Summary.EndReason != tt.endReason {
				t.Fatalf("terminal final state/summary = %#v / %#v", final.State.Progress, final.Summary)
			}
			turnEnd, ok := findEvent(events, EventTurnEnd)
			if !ok || turnEnd.State == nil || turnEnd.State.Progress.CompletedTurns != 1 {
				t.Fatalf("terminal turn_end = %#v", turnEnd)
			}
		})
	}
}

func TestAgentContinueRestoresStateInBeforeRun(t *testing.T) {
	restored := AgentState{
		Messages: []AgentMessage{UserMsg("restored request")},
		Progress: RunProgress{Active: true, NextTurn: true, CompletedTurns: 4},
	}
	var attempts []int
	var callIDs []string
	agent := NewAgent(
		WithModel(mockModel(assistantMsg("done", StopReasonStop))),
		WithBeforeRun(func(context.Context, BeforeRunContext) (AgentState, error) {
			return restored, nil
		}),
		WithModelMiddlewares(func(ctx context.Context, call ModelCall, next ModelExecuteFunc) (ModelResult, error) {
			attempts = append(attempts, call.TurnIndex)
			callIDs = append(callIDs, call.ID)
			return next(ctx, call)
		}),
	)

	if err := agent.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()
	if !slices.Equal(attempts, []int{5}) {
		t.Fatalf("restored turn indexes = %v, want [5]", attempts)
	}
	if !slices.Equal(callIDs, []string{"model-5"}) {
		t.Fatalf("restored model call IDs = %v, want [model-5]", callIDs)
	}
	if got := agent.Messages()[0].TextContent(); got != "restored request" {
		t.Fatalf("restored transcript head = %q", got)
	}
}

func TestAgentRunHooksRepeatPerRunNotPerTurn(t *testing.T) {
	beforeRuns := 0
	afterRuns := 0
	agent := NewAgent(
		WithModel(mockModel(
			assistantMsg("first", StopReasonStop),
			assistantMsg("second", StopReasonStop),
		)),
		WithBeforeRun(func(_ context.Context, run BeforeRunContext) (AgentState, error) {
			beforeRuns++
			return run.State, nil
		}),
		WithAfterRun(func(context.Context, AfterRunContext) error {
			afterRuns++
			return nil
		}),
	)

	if err := agent.Prompt(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()
	if err := agent.Prompt(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()

	if beforeRuns != 2 || afterRuns != 2 {
		t.Fatalf("run hooks = before:%d after:%d, want 2 each", beforeRuns, afterRuns)
	}
}

func TestAgentLoopAfterRunObservesBeforeRunFailure(t *testing.T) {
	afterCalls := 0
	loadErr := errors.New("load checkpoint")
	events := runTestLoop(t,
		nil,
		AgentContext{},
		LoopConfig{
			BeforeRun: func(context.Context, BeforeRunContext) (AgentState, error) {
				return AgentState{}, loadErr
			},
			AfterRun: func(_ context.Context, run AfterRunContext) error {
				afterCalls++
				if !errors.Is(run.Err, loadErr) {
					t.Fatalf("after run error = %v", run.Err)
				}
				return nil
			},
		},
	)

	if afterCalls != 1 {
		t.Fatalf("after run calls = %d, want 1", afterCalls)
	}
	if len(events) == 0 || events[len(events)-1].Type != EventAgentEnd || events[len(events)-1].Err == nil {
		t.Fatalf("terminal events = %#v", events)
	}
}

func TestAgentLoopModelMiddlewareCanReturnKnownOutcome(t *testing.T) {
	modelCalled := false
	events := runTestLoop(t,
		[]AgentMessage{UserMsg("question")},
		AgentContext{},
		LoopConfig{
			Model: funcModel(func(context.Context, *LLMRequest) (*LLMResponse, error) {
				modelCalled = true
				return nil, errors.New("must not execute")
			}),
			ModelMiddlewares: []ModelMiddleware{func(_ context.Context, call ModelCall, _ ModelExecuteFunc) (ModelResult, error) {
				if call.TurnIndex != 1 || call.Attempt != 1 {
					t.Fatalf("call coordinate = turn %d attempt %d", call.TurnIndex, call.Attempt)
				}
				return ModelResult{Message: assistantMsg("known", StopReasonStop)}, nil
			}},
		},
	)

	if modelCalled {
		t.Fatal("model executed despite known middleware outcome")
	}
	response, ok := findEvent(events, EventModelResponse)
	if !ok || response.Message.TextContent() != "known" {
		t.Fatalf("model response = %#v", response.Message)
	}
}

func TestAgentLoopToolMiddlewareCanReturnKnownOutcome(t *testing.T) {
	toolCalls := 0
	gateCalls := 0
	tool := NewFuncTool("write", "write", nil, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		toolCalls++
		return json.RawMessage(`{"real":true}`), nil
	})
	events := runTestLoop(t,
		[]AgentMessage{UserMsg("question")},
		AgentContext{Tools: []Tool{tool}},
		LoopConfig{
			Model: mockModel(
				toolCallMsg(ToolCall{ID: "write-1", Name: "write", Args: json.RawMessage(`{"path":"a"}`)}),
				assistantMsg("done", StopReasonStop),
			),
			ToolGate: func(context.Context, GateRequest) (*GateDecision, error) {
				gateCalls++
				return &GateDecision{Allowed: true}, nil
			},
			ToolMiddlewares: []ToolMiddleware{func(_ context.Context, call ToolCall, _ ToolExecuteFunc) (ToolResult, error) {
				return ToolResult{ToolCallID: call.ID, ToolName: call.Name, Content: json.RawMessage(`{"known":true}`)}, nil
			}},
		},
	)

	if toolCalls != 0 || gateCalls != 0 {
		t.Fatalf("known outcome reached real pipeline: tool=%d gate=%d", toolCalls, gateCalls)
	}
	toolEnd, ok := findEvent(events, EventToolExecEnd)
	if !ok || string(toolEnd.Result) != `{"known":true}` {
		t.Fatalf("tool end = %#v", toolEnd)
	}
}
