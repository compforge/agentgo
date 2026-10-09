package agentgo

import (
	"context"
	"errors"
	"testing"
)

type steeringContext struct {
	projectionCommitManager
	project func([]AgentMessage) ContextCommitResult
	recover func([]AgentMessage) ContextRecoveryResult
	compact func() ContextCommitResult
}

func (m steeringContext) Transform(_ context.Context, input TransformContext) ([]AgentMessage, error) {
	msgs := input.Messages
	return m.project(msgs).Messages, nil
}
func (m steeringContext) Compact(_ context.Context, input TransformContext, _ CompactReason) (ContextCommitResult, error) {
	return m.compact(), nil
}
func (m steeringContext) RecoverOverflow(_ context.Context, input TransformContext, _ error) (ContextRecoveryResult, error) {
	msgs := input.Messages
	return m.recover(msgs), nil
}

func TestSteeringDuringCompactionReachesNextCall(t *testing.T) {
	for _, mode := range []string{"committed", "transient", "overflow", "commit_failure"} {
		t.Run(mode, func(t *testing.T) {
			var queued []AgentMessage
			manager := steeringContext{
				compact: func() ContextCommitResult {
					if mode == "overflow" {
						return ContextCommitResult{}
					}
					queued = []AgentMessage{UserMsg("focus on tests")}
					return ContextCommitResult{Messages: []AgentMessage{UserMsg("summary")}, Changed: mode != "transient"}
				},
				project: func(msgs []AgentMessage) ContextCommitResult {
					if mode == "transient" {
						return ContextCommitResult{Messages: append([]AgentMessage{UserMsg("summary")}, msgs[1:]...)}
					}
					return ContextCommitResult{Messages: msgs}
				},
				recover: func([]AgentMessage) ContextRecoveryResult {
					queued = []AgentMessage{UserMsg("focus on tests")}
					view := []AgentMessage{UserMsg("recovered summary")}
					return ContextRecoveryResult{View: view, CommitMessages: view, ShouldCommit: true}
				},
			}
			calls, commits, observed := 0, 0, 0
			failed := &ContextOverflowError{Cause: errors.New("storage unavailable")}
			events := runTestLoop(t, []AgentMessage{UserMsg("original task")}, AgentContext{}, LoopConfig{
				ContextManager:      manager,
				GetSteeringMessages: func() []AgentMessage { out := queued; queued = nil; return out },
				CommitMessage: func(m AgentMessage) error {
					if m.TextContent() == "focus on tests" {
						commits++
						if mode == "commit_failure" {
							return failed
						}
					}
					return nil
				},
				OnMessage: func(m AgentMessage) {
					if m.TextContent() == "focus on tests" {
						observed++
					}
				},
				Model: sequentialModel(func(_ int, req *LLMRequest) (*LLMResponse, error) {
					calls++
					if mode == "overflow" && calls == 1 {
						return nil, &ContextOverflowError{Cause: errors.New("full")}
					}
					n := 0
					for _, m := range req.Messages {
						if m.TextContent() == "focus on tests" {
							n++
						}
					}
					if n != 1 || req.Messages[len(req.Messages)-1].TextContent() != "focus on tests" {
						t.Errorf("steering missing or duplicated: %+v", req.Messages)
					}
					return &LLMResponse{Message: assistantMsg("done", StopReasonStop)}, nil
				}),
			})
			end, _ := findEvent(events, EventAgentEnd)
			if commits != 1 {
				t.Fatalf("steering commits=%d", commits)
			}
			if mode == "commit_failure" {
				if calls != 0 || observed != 0 || countEvent(events, EventRetry) != 0 || end.Summary.EndReason != EndReasonError {
					t.Fatalf("rejected steering: calls=%d observed=%d end=%+v", calls, observed, end.Summary)
				}
				ev, _ := findEvent(events, EventError)
				if !errors.Is(ev.Err, failed) {
					t.Fatalf("error=%v", ev.Err)
				}
			} else {
				wantCalls := 1
				if mode == "overflow" {
					wantCalls = 2
				}
				if calls != wantCalls || observed != 1 || end.Summary.EndReason != EndReasonStop {
					t.Fatalf("calls=%d observed=%d end=%+v", calls, observed, end.Summary)
				}
			}
			accepted := 0
			for _, m := range end.NewMessages {
				if m.TextContent() == "focus on tests" {
					accepted++
				}
			}
			wantAccepted := 1
			if mode == "commit_failure" {
				wantAccepted = 0
			}
			if accepted != wantAccepted {
				t.Fatalf("accepted steering=%d want=%d", accepted, wantAccepted)
			}
			if mode == "transient" && end.State.Messages[0].TextContent() != "original task" {
				t.Fatal("transient projection replaced baseline")
			}
		})
	}
}

func TestToolCallDeltaRetainsIdentityInLoopEvents(t *testing.T) {
	model := newScriptedStreamModel(func(ch chan<- StreamEvent) {
		for _, id := range []string{"a", "b", "a", "b"} {
			ch <- StreamEvent{Type: StreamEventToolCallDelta, ToolID: id, Delta: id, Message: Message{Role: RoleAssistant}}
		}
		ch <- StreamEvent{Type: StreamEventDone, Message: assistantMsg("done", StopReasonStop)}
	})
	events := runTestLoop(t, []AgentMessage{UserMsg("task")}, AgentContext{}, LoopConfig{Model: model})
	var ids []string
	for _, ev := range events {
		if ev.Type == EventMessageUpdate && ev.DeltaKind == DeltaToolCall {
			ids = append(ids, ev.ToolID)
		}
	}
	if len(ids) != 4 || ids[0] != "a" || ids[1] != "b" || ids[2] != "a" || ids[3] != "b" {
		t.Fatalf("delta identities=%v", ids)
	}
}

func TestOverflowRetryViewRemainsSeparateFromBaseline(t *testing.T) {
	for _, commit := range []bool{false, true} {
		var queued []AgentMessage
		compacts, transforms, models := 0, 0, 0
		manager := steeringContext{
			compact: func() ContextCommitResult { compacts++; return ContextCommitResult{} },
			project: func(messages []AgentMessage) ContextCommitResult {
				transforms++
				return ContextCommitResult{Messages: messages}
			},
			recover: func([]AgentMessage) ContextRecoveryResult {
				queued = []AgentMessage{UserMsg("late")}
				return ContextRecoveryResult{View: []AgentMessage{UserMsg("retry view")}, CommitMessages: []AgentMessage{UserMsg("accepted baseline")}, ShouldCommit: commit}
			},
		}
		events := runTestLoop(t, []AgentMessage{UserMsg("original baseline")}, AgentContext{}, LoopConfig{
			ContextManager:      manager,
			GetSteeringMessages: func() []AgentMessage { pending := queued; queued = nil; return pending },
			Model: sequentialModel(func(_ int, req *LLMRequest) (*LLMResponse, error) {
				models++
				if models == 1 {
					return nil, ErrContextOverflow
				}
				if len(req.Messages) != 2 || req.Messages[0].TextContent() != "retry view" || req.Messages[1].TextContent() != "late" {
					t.Errorf("retry request=%+v", req.Messages)
				}
				return &LLMResponse{Message: assistantMsg("done", StopReasonStop)}, nil
			}),
		})
		end, _ := findEvent(events, EventAgentEnd)
		want := "original baseline"
		if commit {
			want = "accepted baseline"
		}
		if compacts != 1 || transforms != 2 || models != 2 || end.State == nil || len(end.State.Messages) != 3 || end.State.Messages[0].TextContent() != want || end.State.Messages[1].TextContent() != "late" {
			t.Fatalf("commit=%v compacts=%d transforms=%d models=%d state=%+v", commit, compacts, transforms, models, end.State)
		}
	}
}
