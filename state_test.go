package agentgo

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentcodec "github.com/compforge/agentgo/codec"
)

func TestAgentStateCodecRoundTripPortableProjection(t *testing.T) {
	userMessage := UserMsg("hello")
	assistantMessage := assistantMsg("world", StopReasonToolUse)
	assistantMessage.Content = append(assistantMessage.Content, ToolCallBlock(ToolCall{
		ID:   "read-1",
		Name: "read",
		Args: json.RawMessage(`{"path":"README.md"}`),
	}))
	original := AgentState{
		SystemPrompt:     "runtime config",
		Messages:         []AgentMessage{&userMessage, assistantMessage},
		SteeringQueue:    []AgentMessage{UserMsg("redirect")},
		FollowUpQueue:    []AgentMessage{UserMsg("one more thing")},
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

	c, err := NewCodec()
	if err != nil {
		t.Fatal(err)
	}
	data, err := c.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"$type":"agentgo.agent-state.v1"`) || !strings.Contains(string(data), `"$type":"agentgo.message.v1"`) {
		t.Fatalf("encoded state does not carry stable AgentGo type IDs: %s", data)
	}
	var restored AgentState
	if err := c.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	if len(restored.Messages) != 2 || restored.Messages[0].TextContent() != "hello" || restored.Messages[1].TextContent() != "world" {
		t.Fatalf("restored messages = %#v", restored.Messages)
	}
	restoredAssistant := restored.Messages[1].(Message)
	toolCalls := restoredAssistant.ToolCalls()
	if len(toolCalls) != 1 || toolCalls[0].ID != "read-1" || string(toolCalls[0].Args) != `{"path":"README.md"}` {
		t.Fatalf("restored tool calls = %#v", toolCalls)
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
	if got := restored.SteeringQueue[0].TextContent(); got != "redirect" {
		t.Fatalf("steering message = %q", got)
	}
	if got := restored.FollowUpQueue[0].TextContent(); got != "one more thing" {
		t.Fatalf("follow-up message = %q", got)
	}
	if restored.SystemPrompt != "" || restored.Tools != nil || restored.IsRunning || restored.StreamMessage != nil || restored.PendingToolCalls != nil || restored.Error != "" {
		t.Fatalf("runtime fields leaked into portable state: %#v", restored)
	}
}

func TestAgentStateCodecRejectsUnregisteredAgentMessage(t *testing.T) {
	c, err := NewCodec()
	if err != nil {
		t.Fatal(err)
	}
	state := AgentState{Messages: []AgentMessage{applicationMessage{text: "domain message", include: true}}}
	if _, err := c.Marshal(state); err == nil || !strings.Contains(err.Error(), "is not registered") {
		t.Fatalf("error = %v, want unregistered AgentMessage", err)
	}
}

type codecApplicationMessage struct {
	Text      string    `codec:"text"`
	Include   bool      `codec:"include"`
	Timestamp time.Time `codec:"timestamp"`
	cache     string
}

func (m *codecApplicationMessage) GetRole() Role           { return RoleUser }
func (m *codecApplicationMessage) GetTimestamp() time.Time { return m.Timestamp }
func (m *codecApplicationMessage) Raw() AgentMessage       { return m }
func (m *codecApplicationMessage) Priority() int           { return 0 }
func (m *codecApplicationMessage) Compact(float64) (AgentMessage, float64) {
	return m, 1
}
func (m *codecApplicationMessage) TextContent() string     { return m.Text }
func (m *codecApplicationMessage) ThinkingContent() string { return "" }
func (m *codecApplicationMessage) HasToolCalls() bool      { return false }
func (m *codecApplicationMessage) ToMessage() (Message, bool) {
	return UserMsg(m.Text), m.Include
}

func TestAgentStateCodecRoundTripsRegisteredApplicationMessage(t *testing.T) {
	c, err := NewCodec(
		agentcodec.Type[*codecApplicationMessage]("test.application-message.v1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := &codecApplicationMessage{
		Text:      "domain message",
		Include:   true,
		Timestamp: time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC),
		cache:     "runtime only",
	}

	data, err := c.Marshal(AgentState{Messages: []AgentMessage{want}})
	if err != nil {
		t.Fatal(err)
	}
	var restored AgentState
	if err := c.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored.Messages) != 1 {
		t.Fatalf("restored messages = %#v", restored.Messages)
	}
	got, ok := restored.Messages[0].(*codecApplicationMessage)
	if !ok || got.Text != want.Text || !got.Include || !got.Timestamp.Equal(want.Timestamp) || got.cache != "" {
		t.Fatalf("restored message = %#v", restored.Messages[0])
	}
}

func TestAgentLoopRunHooksExecuteOnce(t *testing.T) {
	var order []string
	var turnStates []AgentState
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
				turnStates = append(turnStates, turn.State)
				if turn.State.Progress.CompletedTurns != turn.TurnIndex {
					t.Fatalf("state turn = %d, want %d", turn.State.Progress.CompletedTurns, turn.TurnIndex)
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
	if len(turnStates) != 2 || !turnStates[0].Progress.NextTurn || len(turnStates[0].Progress.PendingMessages) != 1 {
		t.Fatalf("first turn state does not carry length continuation: %#v", turnStates)
	}
	if turnStates[1].Progress.NextTurn || len(turnStates[1].Progress.PendingMessages) != 0 {
		t.Fatalf("final turn state unexpectedly continues: %#v", turnStates[1].Progress)
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
			var turnState AgentState
			var final AfterRunContext
			events := runTestLoop(t,
				[]AgentMessage{UserMsg("question")},
				AgentContext{},
				LoopConfig{
					Model: mockModel(assistantMsg("terminal", tt.stopReason)),
					AfterTurn: func(_ context.Context, turn AfterTurnContext) error {
						turnState = turn.State
						return nil
					},
					AfterRun: func(_ context.Context, run AfterRunContext) error {
						final = run
						return nil
					},
				},
			)

			if turnState.Progress.CompletedTurns != 1 || turnState.Progress.NextTurn {
				t.Fatalf("terminal turn progress = %#v", turnState.Progress)
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
		WithModelMiddlewares(func(ctx context.Context, call ModelExecution, next ModelExecuteFunc) (ModelResult, error) {
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

func TestAgentContinueConsumesRestoredFollowUpQueue(t *testing.T) {
	restored := AgentState{
		Messages:      []AgentMessage{UserMsg("question"), assistantMsg("first answer", StopReasonStop)},
		FollowUpQueue: []AgentMessage{UserMsg("restored follow-up")},
		Progress:      RunProgress{CompletedTurns: 1},
	}
	model := sequentialModel(func(i int, req *LLMRequest) (*LLMResponse, error) {
		if i != 0 {
			t.Fatalf("model call = %d, want one resumed call", i+1)
		}
		if got := req.Messages[len(req.Messages)-1].TextContent(); got != "restored follow-up" {
			t.Fatalf("last model input = %q, want restored follow-up", got)
		}
		return &LLMResponse{Message: assistantMsg("follow-up answer", StopReasonStop)}, nil
	})
	agent := NewAgent(
		WithModel(model),
		WithBeforeRun(func(context.Context, BeforeRunContext) (AgentState, error) {
			return restored, nil
		}),
	)

	if err := agent.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()
	if agent.HasFollowUps() {
		t.Fatal("restored follow-up remained queued after Continue")
	}
	if got := agent.Messages()[len(agent.Messages())-1].TextContent(); got != "follow-up answer" {
		t.Fatalf("last message = %q, want follow-up answer", got)
	}
}

func TestAgentContinueCombinesRestoredStateWithNewAcceptedFollowUp(t *testing.T) {
	restored := AgentState{
		Messages: []AgentMessage{UserMsg("question"), assistantMsg("first answer", StopReasonStop)},
		Progress: RunProgress{CompletedTurns: 1},
	}
	model := sequentialModel(func(i int, req *LLMRequest) (*LLMResponse, error) {
		if i != 0 {
			t.Fatalf("model call = %d, want one resumed call", i+1)
		}
		if got := req.Messages[len(req.Messages)-1].TextContent(); got != "new follow-up" {
			t.Fatalf("last model input = %q, want new follow-up", got)
		}
		return &LLMResponse{Message: assistantMsg("follow-up answer", StopReasonStop)}, nil
	})
	agent := NewAgent(
		WithModel(model),
		WithBeforeRun(func(context.Context, BeforeRunContext) (AgentState, error) {
			return restored, nil
		}),
	)
	agent.FollowUp(UserMsg("new follow-up"))

	if err := agent.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()
	if agent.HasFollowUps() {
		t.Fatal("new follow-up remained queued after restored Continue")
	}
}

func TestAgentStateIncludesAcceptedQueues(t *testing.T) {
	agent := NewAgent()
	agent.Steer(UserMsg("redirect"))
	agent.FollowUp(UserMsg("one more thing"))

	state := agent.State()
	if len(state.SteeringQueue) != 1 || state.SteeringQueue[0].TextContent() != "redirect" {
		t.Fatalf("steering queue = %#v", state.SteeringQueue)
	}
	if len(state.FollowUpQueue) != 1 || state.FollowUpQueue[0].TextContent() != "one more thing" {
		t.Fatalf("follow-up queue = %#v", state.FollowUpQueue)
	}
}

func TestAgentFinalStateRetainsInputAcceptedDuringFailedRun(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var checkpoint AgentState
	agent := NewAgent(
		WithMaxRetries(0),
		WithModel(funcModel(func(context.Context, *LLMRequest) (*LLMResponse, error) {
			close(started)
			<-release
			return nil, errors.New("model unavailable")
		})),
		WithAfterRun(func(_ context.Context, run AfterRunContext) error {
			checkpoint = run.State
			return nil
		}),
	)

	if err := agent.Prompt(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	<-started
	agent.FollowUp(UserMsg("accepted while running"))
	close(release)
	agent.WaitForIdle()

	if len(checkpoint.FollowUpQueue) != 1 || checkpoint.FollowUpQueue[0].TextContent() != "accepted while running" {
		t.Fatalf("final checkpoint follow-up queue = %#v", checkpoint.FollowUpQueue)
	}
	state := agent.State()
	if len(state.FollowUpQueue) != 1 || state.FollowUpQueue[0].TextContent() != "accepted while running" {
		t.Fatalf("idle Agent follow-up queue = %#v", state.FollowUpQueue)
	}
}

func TestAgentCanClearFollowUpAfterCompletedBoundaryCheckpoint(t *testing.T) {
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	var calls atomic.Int32
	agent := NewAgent(WithModel(funcModel(func(context.Context, *LLMRequest) (*LLMResponse, error) {
		switch calls.Add(1) {
		case 1:
			close(firstStarted)
			<-releaseFirst
			return &LLMResponse{Message: assistantMsg("truncated", StopReasonLength)}, nil
		case 2:
			close(secondStarted)
			<-releaseSecond
			return &LLMResponse{Message: assistantMsg("recovered", StopReasonStop)}, nil
		default:
			return nil, errors.New("unexpected follow-up model call")
		}
	})))

	if err := agent.Prompt(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	<-firstStarted
	agent.FollowUp(UserMsg("cancel before natural stop"))
	close(releaseFirst)
	<-secondStarted
	if !agent.HasFollowUps() {
		t.Fatal("completed-boundary checkpoint removed the live follow-up queue")
	}
	agent.ClearFollowUpQueue()
	close(releaseSecond)
	agent.WaitForIdle()

	if got := calls.Load(); got != 2 {
		t.Fatalf("model calls = %d, want 2 after clearing follow-up", got)
	}
	if agent.HasFollowUps() {
		t.Fatal("cleared follow-up returned after run completion")
	}
}

func TestAgentLoopCheckpointMovesAcceptedInputThroughQueueAndPending(t *testing.T) {
	delivered := false
	var checkpoints []AgentState
	runTestLoop(t,
		[]AgentMessage{UserMsg("question")},
		AgentContext{},
		LoopConfig{
			Model: mockModel(
				assistantMsg("truncated", StopReasonLength),
				assistantMsg("recovered", StopReasonStop),
				assistantMsg("follow-up answer", StopReasonStop),
			),
			GetFollowUpMessages: func() []AgentMessage {
				if delivered {
					return nil
				}
				delivered = true
				return []AgentMessage{UserMsg("queued follow-up")}
			},
			AfterTurn: func(_ context.Context, turn AfterTurnContext) error {
				checkpoints = append(checkpoints, turn.State)
				return nil
			},
		},
	)

	if len(checkpoints) != 3 {
		t.Fatalf("turn checkpoints = %d, want 3", len(checkpoints))
	}
	first := checkpoints[0]
	if len(first.FollowUpQueue) != 1 || first.FollowUpQueue[0].TextContent() != "queued follow-up" {
		t.Fatalf("first checkpoint follow-up queue = %#v", first.FollowUpQueue)
	}
	if len(first.Progress.PendingMessages) != 1 || first.Progress.PendingMessages[0].TextContent() != defaultLengthRecoveryPrompt {
		t.Fatalf("first checkpoint loop pending = %#v", first.Progress.PendingMessages)
	}
	second := checkpoints[1]
	if len(second.FollowUpQueue) != 0 {
		t.Fatalf("second checkpoint follow-up queue = %#v, want transferred", second.FollowUpQueue)
	}
	if len(second.Progress.PendingMessages) != 1 || second.Progress.PendingMessages[0].TextContent() != "queued follow-up" {
		t.Fatalf("second checkpoint loop pending = %#v", second.Progress.PendingMessages)
	}
}

func TestAgentLoopCheckpointDoesNotReplayCommittedPendingInput(t *testing.T) {
	state := AgentState{
		Messages: []AgentMessage{UserMsg("base")},
		Progress: RunProgress{
			Active:          true,
			NextTurn:        true,
			CompletedTurns:  1,
			PendingMessages: []AgentMessage{UserMsg("resume input")},
		},
	}
	events := collectEvents(AgentLoopContinue(
		context.Background(),
		AgentContext{Messages: state.Messages},
		LoopConfig{
			InitialState: state,
			MaxRetries:   0,
			Model: funcModel(func(context.Context, *LLMRequest) (*LLMResponse, error) {
				return nil, errors.New("model unavailable")
			}),
		},
	))

	end, ok := findEvent(events, EventAgentEnd)
	if !ok || end.State == nil {
		t.Fatalf("terminal event = %#v", end)
	}
	if len(end.State.Progress.PendingMessages) != 0 {
		t.Fatalf("committed input remained pending: %#v", end.State.Progress.PendingMessages)
	}
	if len(end.State.Messages) != 2 || end.State.Messages[1].TextContent() != "resume input" {
		t.Fatalf("final messages = %#v", end.State.Messages)
	}
}

func TestAgentLoopStartsFreshProgressWhenAddingPromptToInactiveState(t *testing.T) {
	state := AgentState{
		Messages: []AgentMessage{UserMsg("old question"), assistantMsg("old answer", StopReasonStop)},
		Progress: RunProgress{
			CompletedTurns: 7,
		},
	}
	events := collectEvents(AgentLoop(
		context.Background(),
		[]AgentMessage{UserMsg("new question")},
		AgentContext{Messages: state.Messages},
		LoopConfig{
			InitialState: state,
			Model:        mockModel(assistantMsg("new answer", StopReasonStop)),
		},
	))

	end, ok := findEvent(events, EventAgentEnd)
	if !ok || end.State == nil {
		t.Fatalf("terminal event = %#v", end)
	}
	if end.State.Progress.CompletedTurns != 1 {
		t.Fatalf("completed turns = %d, want fresh run count 1", end.State.Progress.CompletedTurns)
	}
	if got := end.State.Messages[len(end.State.Messages)-1].TextContent(); got != "new answer" {
		t.Fatalf("last message = %q, want new answer", got)
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
	loadErr := errors.New("load state")
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
			ModelMiddlewares: []ModelMiddleware{func(_ context.Context, call ModelExecution, _ ModelExecuteFunc) (ModelResult, error) {
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
	var observed ToolExecution
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
			ToolMiddlewares: []ToolMiddleware{func(_ context.Context, execution ToolExecution, _ ToolExecuteFunc) (ToolResult, error) {
				observed = execution
				call := execution.Call
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
	toolStart, ok := findEvent(events, EventToolExecStart)
	if !ok || toolStart.Execution == nil || toolEnd.Execution == nil {
		t.Fatalf("tool execution events = start:%#v end:%#v", toolStart, toolEnd)
	}
	want := Execution{ID: "write-1", Kind: ExecutionKindTool, TurnIndex: 1, Attempt: 1}
	if observed.Execution != want || *toolStart.Execution != want || *toolEnd.Execution != want {
		t.Fatalf("tool execution coordinate = middleware:%#v start:%#v end:%#v, want %#v", observed.Execution, toolStart.Execution, toolEnd.Execution, want)
	}
}
