package agentgo

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	agentcodec "github.com/compforge/agentgo/codec"
)

func TestAgentStateCodecRoundTripPortableProjection(t *testing.T) {
	userMessage := UserMsg("hello")
	userMessage.Metadata = map[string]any{
		"$type": "agentgo.message.v1",
		"value": "application metadata",
	}
	assistantMessage := assistantMsg("world", StopReasonToolUse)
	assistantMessage.Content = append(assistantMessage.Content, ToolCallBlock(ToolCall{
		ID:   "read-1",
		Name: "read",
		Args: json.RawMessage(`{"path":"README.md"}`),
	}))
	original := AgentState{
		SystemPrompt:     "runtime config",
		Messages:         []AgentMessage{&userMessage, assistantMessage},
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
	restoredUser := restored.Messages[0].(Message)
	if !reflect.DeepEqual(restoredUser.Metadata, userMessage.Metadata) {
		t.Fatalf("restored metadata = %#v, want %#v", restoredUser.Metadata, userMessage.Metadata)
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

func TestAgentLoopTurnCheckpoints(t *testing.T) {
	var turnStates []AgentState
	events := runTestLoop(t,
		[]AgentMessage{UserMsg("question")},
		AgentContext{},
		LoopConfig{
			Model: mockModel(
				assistantMsg("truncated", StopReasonLength),
				assistantMsg("done", StopReasonStop),
			),
			AfterTurn: func(_ context.Context, turn AfterTurnContext) error {
				turnStates = append(turnStates, turn.State)
				if turn.State.Progress.CompletedTurns != turn.TurnIndex {
					t.Fatalf("state turn = %d, want %d", turn.State.Progress.CompletedTurns, turn.TurnIndex)
				}
				return nil
			},
		},
	)

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
			events := runTestLoop(t,
				[]AgentMessage{UserMsg("question")},
				AgentContext{},
				LoopConfig{
					Model: mockModel(assistantMsg("terminal", tt.stopReason)),
					AfterTurn: func(_ context.Context, turn AfterTurnContext) error {
						turnState = turn.State
						return nil
					},
				},
			)

			if turnState.Progress.CompletedTurns != 1 || turnState.Progress.NextTurn {
				t.Fatalf("terminal turn progress = %#v", turnState.Progress)
			}
			end, ok := findEvent(events, EventAgentEnd)
			if !ok || end.State == nil || end.State.Progress.CompletedTurns != 1 || end.Summary == nil || end.Summary.TurnCount != 1 || end.Summary.EndReason != tt.endReason {
				t.Fatalf("terminal event = %#v", end)
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
		WithBeforeRun(func(context.Context, BeforeRunContext) (AgentSnapshot, error) {
			return AgentSnapshot{State: restored}, nil
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

func TestAgentRunHooksRepeatPerRunNotPerTurn(t *testing.T) {
	beforeRuns := 0
	afterRuns := 0
	agent := NewAgent(
		WithModel(mockModel(
			assistantMsg("first", StopReasonStop),
			assistantMsg("second", StopReasonStop),
		)),
		WithBeforeRun(func(_ context.Context, run BeforeRunContext) (AgentSnapshot, error) {
			beforeRuns++
			return run.Snapshot, nil
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

func TestAgentRunHooksWrapLoopAndTerminalListeners(t *testing.T) {
	var order []string
	agent := NewAgent(
		WithModel(mockModel(assistantMsg("done", StopReasonStop))),
		WithBeforeRun(func(_ context.Context, run BeforeRunContext) (AgentSnapshot, error) {
			order = append(order, "before_run")
			if run.Kind != RunKindPrompt || len(run.Input) != 1 || run.Input[0].TextContent() != "question" {
				t.Fatalf("BeforeRun context = %#v", run)
			}
			return run.Snapshot, nil
		}),
		WithAfterRun(func(_ context.Context, run AfterRunContext) error {
			order = append(order, "after_run")
			if run.Kind != RunKindPrompt || run.Snapshot.State.IsRunning || run.Snapshot.State.Progress.Active {
				t.Fatalf("AfterRun context = %#v", run)
			}
			return nil
		}),
	)
	agent.Subscribe(func(event Event) {
		switch event.Type {
		case EventAgentStart:
			order = append(order, "agent_start")
		case EventAgentEnd:
			order = append(order, "agent_end")
		}
	})

	if err := agent.Prompt(context.Background(), "question"); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()

	if !slices.Equal(order, []string{"before_run", "agent_start", "after_run", "agent_end"}) {
		t.Fatalf("lifecycle order = %v", order)
	}
}

func TestAgentAfterRunFailureChangesTerminalResult(t *testing.T) {
	hookErr := errors.New("save snapshot")
	var events []Event
	agent := NewAgent(
		WithModel(mockModel(assistantMsg("done", StopReasonStop))),
		WithAfterRun(func(context.Context, AfterRunContext) error {
			return hookErr
		}),
	)
	agent.Subscribe(func(event Event) {
		if event.Type == EventError || event.Type == EventAgentEnd {
			events = append(events, event)
		}
	})

	if err := agent.Prompt(context.Background(), "question"); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()

	if len(events) != 2 || events[0].Type != EventError || events[1].Type != EventAgentEnd {
		t.Fatalf("terminal events = %#v", events)
	}
	if !errors.Is(events[1].Err, hookErr) || events[1].Summary == nil || events[1].Summary.EndReason != EndReasonError {
		t.Fatalf("terminal result = %#v", events[1])
	}
	if events[1].State == nil || events[1].State.Error != agent.State().Error || !strings.Contains(events[1].State.Error, hookErr.Error()) {
		t.Fatalf("terminal state = %#v, agent state = %#v", events[1].State, agent.State())
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
