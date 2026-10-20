package agentgo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// +case:id=length_terminal,desc=`Truncated reasoning exhausts bounded recovery`,expect=`length terminal preserves usage and never reports normal stop`
func TestLengthRecoveryTerminal(t *testing.T) {
	zero, one := 0, 1
	for _, tc := range []struct {
		name                        string
		limit                       *int
		maxTurns, calls, recoveries int
		reason                      EndReason
	}{
		{"default", nil, 10, 4, 3, EndReasonLength},
		{"disabled", &zero, 10, 1, 0, EndReasonLength},
		{"one", &one, 10, 2, 1, EndReasonLength},
		{"turn_limit", nil, 2, 2, 2, EndReasonMaxTurns},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			model := sequentialModel(func(_ int, _ *LLMRequest) (*LLMResponse, error) {
				calls++
				return &LLMResponse{Message: Message{Role: RoleAssistant, Content: []ContentBlock{ThinkingBlock("partial reasoning")}, StopReason: StopReasonLength, Usage: &Usage{Input: 5, Output: 10, Reasoning: 10, TotalTokens: 15}}}, nil
			})
			events := runTestLoop(t, []AgentMessage{UserMsg("work")}, AgentContext{}, LoopConfig{Model: model, MaxTurns: tc.maxTurns, MaxLengthRecoveries: tc.limit})
			end, ok := findEvent(events, EventAgentEnd)
			if !ok || end.Summary.EndReason != tc.reason || calls != tc.calls || end.Summary.LengthRecoveries != tc.recoveries {
				t.Fatalf("calls=%d summary=%+v", calls, end.Summary)
			}
			if end.State.TotalUsage.Reasoning != 10*tc.calls || end.State.TotalUsage.TotalTokens != 15*tc.calls {
				t.Fatalf("usage=%+v", end.State.TotalUsage)
			}
			decisions := 0
			for _, ev := range events {
				if ev.Type == EventModelResponse {
					if ev.LengthRecovery == nil || ev.Message.(Message).StopReason != StopReasonLength {
						t.Fatalf("missing truncation fact: %+v", ev)
					}
					if ev.LengthRecovery.Count != decisions {
						t.Fatalf("decision count=%d, want %d", ev.LengthRecovery.Count, decisions)
					}
					decisions++
				}
			}
			if tc.reason == EndReasonLength {
				if !errors.Is(end.Err, ErrOutputTruncated) {
					t.Fatalf("terminal error=%v", end.Err)
				}
				for i := len(events) - 1; i >= 0; i-- {
					if events[i].Type == EventModelResponse {
						if events[i].LengthRecovery.WillRecover {
							t.Fatal("terminal response schedules recovery")
						}
						break
					}
				}
			}
		})
	}
}

func TestLengthRecoveryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	model := sequentialModel(func(i int, _ *LLMRequest) (*LLMResponse, error) {
		calls++
		if i == 1 {
			cancel()
			return nil, ctx.Err()
		}
		return &LLMResponse{Message: assistantMsg("partial", StopReasonLength)}, nil
	})
	var end Event
	for ev := range AgentLoop(ctx, []AgentMessage{UserMsg("work")}, AgentContext{}, LoopConfig{Model: model}) {
		if ev.Type == EventAgentEnd {
			end = ev
		}
	}
	if calls != 2 || end.Summary == nil || end.Summary.EndReason != EndReasonAborted {
		t.Fatalf("calls=%d end=%+v", calls, end.Summary)
	}
}

func TestLengthRecoveryAgentOption(t *testing.T) {
	a := NewAgent(WithMaxLengthRecoveries(0))
	cfg := a.buildConfig(false)
	if cfg.MaxLengthRecoveries == nil || *cfg.MaxLengthRecoveries != 0 {
		t.Fatalf("limit=%v", cfg.MaxLengthRecoveries)
	}
	if NewAgent().buildConfig(false).MaxLengthRecoveries != nil {
		t.Fatal("unset must retain default")
	}
}

func TestModelCallEventsRecordForwardedOptions(t *testing.T) {
	model := callOptionFuncModel(func(_ context.Context, _ *LLMRequest, cfg CallConfig) (*LLMResponse, error) {
		if cfg.ThinkingLevel != ThinkingOff || cfg.MaxTokens != 1024 {
			t.Fatalf("forwarded=%+v", cfg)
		}
		return &LLMResponse{Message: assistantMsg("done", StopReasonStop)}, nil
	})
	events := runTestLoop(t, []AgentMessage{UserMsg("work")}, AgentContext{}, LoopConfig{Model: model, ThinkingLevel: ThinkingHigh,
		ModelMiddlewares: []ModelMiddleware{func(ctx context.Context, call ModelExecution, next ModelExecuteFunc) (ModelResult, error) {
			for _, option := range []CallOption{WithThinking("none"), WithMaxTokens(1024), WithAPIKey("private-test-key")} {
				option(call.Request.Config)
			}
			return next(ctx, call)
		}},
	})
	start, _ := findEvent(events, EventModelExecStart)
	end, _ := findEvent(events, EventModelExecEnd)
	if start.ModelCall.ThinkingLevel != ThinkingHigh || end.ModelCall.ThinkingLevel != ThinkingOff || end.ModelCall.MaxTokens != 1024 {
		t.Fatalf("start=%+v end=%+v", start.ModelCall, end.ModelCall)
	}
	b, err := json.Marshal(end.ModelCall)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "private-test-key") {
		t.Fatal("credentials leaked")
	}
}

func TestReasoningUsageRoundTrip(t *testing.T) {
	usage := Usage{Input: 10, Output: 20, Reasoning: 15, TotalTokens: 30}
	usage.Add(&Usage{Input: 2, Output: 8, Reasoning: 6, TotalTokens: 10})
	if usage.Reasoning != 21 || usage.Output != 28 || usage.TotalTokens != 40 {
		t.Fatalf("usage=%+v", usage)
	}
	c, err := NewCodec()
	if err != nil {
		t.Fatal(err)
	}
	state := AgentState{TotalUsage: usage, Messages: []AgentMessage{Message{Role: RoleAssistant, StopReason: StopReasonLength, Usage: &usage}}}
	b, err := c.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var restored AgentState
	if err = c.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.TotalUsage.Reasoning != 21 || restored.Messages[0].(Message).Usage.Reasoning != 21 {
		t.Fatalf("restored=%+v", restored)
	}
	b, err = json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Usage
	if err = json.Unmarshal(b, &decoded); err != nil || decoded.Reasoning != 21 {
		t.Fatalf("json usage=%+v error=%v", decoded, err)
	}
}

func TestLengthTerminalSurvivesTurnCheckpoint(t *testing.T) {
	zero := 0
	var saved AgentState
	events := runTestLoop(t, []AgentMessage{UserMsg("work")}, AgentContext{}, LoopConfig{
		Model: mockModel(assistantMsg("partial", StopReasonLength)), MaxLengthRecoveries: &zero,
		AfterTurn: func(_ context.Context, turn AfterTurnContext) error { saved = turn.State; return nil },
	})
	end, _ := findEvent(events, EventAgentEnd)
	if saved.Progress.EndReason != EndReasonLength || end.State.Progress.EndReason != EndReasonLength {
		t.Fatal("length decision not persisted")
	}
	codec, err := NewCodec()
	if err != nil {
		t.Fatal(err)
	}
	b, err := codec.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	var restored AgentState
	if err = codec.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	model := sequentialModel(func(int, *LLMRequest) (*LLMResponse, error) {
		t.Fatal("terminal checkpoint called model again")
		return nil, nil
	})
	for ev := range AgentLoopContinue(context.Background(), AgentContext{Messages: restored.Messages}, LoopConfig{Model: model, InitialState: restored}) {
		if ev.Type == EventAgentEnd && (ev.Summary.EndReason != EndReasonLength || !errors.Is(ev.Err, ErrOutputTruncated)) {
			t.Fatalf("restored terminal=%+v", ev)
		}
	}
}

func TestLengthRecoveryBudgetSurvivesCheckpoint(t *testing.T) {
	one := 1
	saved := AgentState{Messages: []AgentMessage{UserMsg("work"), assistantMsg("partial", StopReasonLength)}, Progress: RunProgress{Active: true, NextTurn: true, CompletedTurns: 1, LengthRecoveries: 1, PendingMessages: []AgentMessage{UserMsg("continue")}}}
	calls := 0
	model := sequentialModel(func(int, *LLMRequest) (*LLMResponse, error) {
		calls++
		return &LLMResponse{Message: assistantMsg("partial", StopReasonLength)}, nil
	})
	var end Event
	for ev := range AgentLoopContinue(context.Background(), AgentContext{Messages: saved.Messages}, LoopConfig{Model: model, InitialState: saved, MaxLengthRecoveries: &one}) {
		if ev.Type == EventAgentEnd {
			end = ev
		}
	}
	if calls != 1 || end.Summary.EndReason != EndReasonLength || end.Summary.LengthRecoveries != 1 {
		t.Fatalf("calls=%d summary=%+v", calls, end.Summary)
	}
}

// +case:id=stream_error_is_not_commit,desc=`A stream emits an error event without a cause after partial text`,expect=`The call fails and retains diagnostic content without committing it`
func TestStreamErrorWithoutCauseRetainsEvidence(t *testing.T) {
	partial := assistantMsg("unfinished", "")
	model := newScriptedStreamModel(func(ch chan<- StreamEvent) {
		ch <- StreamEvent{Type: StreamEventTextDelta, Message: partial, Delta: "unfinished"}
		ch <- StreamEvent{Type: StreamEventError}
	})
	events := runTestLoop(t, []AgentMessage{UserMsg("test")}, AgentContext{}, LoopConfig{Model: model})
	end, ok := findEvent(events, EventModelExecEnd)
	if !ok || !errors.Is(end.Err, ErrStreamPartial) || end.Message.TextContent() != "unfinished" {
		t.Fatalf("end=%+v", end)
	}
	for _, event := range events {
		if event.Type == EventMessageEnd && event.Message.GetRole() == RoleAssistant {
			t.Fatal("failed stream committed")
		}
	}
}
