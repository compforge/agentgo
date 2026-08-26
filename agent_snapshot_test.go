package agentgo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestAgentSnapshotCodecRoundTrip(t *testing.T) {
	original := AgentSnapshot{
		State: AgentState{
			Messages:   []AgentMessage{UserMsg("working")},
			TotalUsage: Usage{Input: 3, Output: 2, TotalTokens: 5},
			Progress: RunProgress{
				Active:          true,
				NextTurn:        true,
				CompletedTurns:  2,
				PendingMessages: []AgentMessage{UserMsg("continue")},
			},
		},
		SteeringQueue: []AgentMessage{UserMsg("change direction")},
		FollowUpQueue: []AgentMessage{UserMsg("then summarize")},
	}
	c, err := NewCodec()
	if err != nil {
		t.Fatal(err)
	}

	data, err := c.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(data)
	for _, typeID := range []string{
		"agentgo.agent-snapshot.v1",
		"agentgo.agent-state.v1",
		"agentgo.message.v1",
	} {
		if !strings.Contains(encoded, `"$type":"`+typeID+`"`) {
			t.Fatalf("encoded snapshot does not carry type ID %q: %s", typeID, data)
		}
	}

	var restored AgentSnapshot
	if err := c.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if got := restored.State.Messages[0].TextContent(); got != "working" {
		t.Fatalf("state message = %q", got)
	}
	if got := restored.State.Progress.PendingMessages[0].TextContent(); got != "continue" {
		t.Fatalf("pending message = %q", got)
	}
	if got := restored.SteeringQueue[0].TextContent(); got != "change direction" {
		t.Fatalf("steering message = %q", got)
	}
	if got := restored.FollowUpQueue[0].TextContent(); got != "then summarize" {
		t.Fatalf("follow-up message = %q", got)
	}
}

func TestAgentSnapshotIncludesIndependentQueueSlices(t *testing.T) {
	agent := NewAgent()
	if err := agent.SetMessages([]AgentMessage{UserMsg("history")}); err != nil {
		t.Fatal(err)
	}
	agent.Steer(UserMsg("steer"))
	agent.FollowUp(UserMsg("follow up"))

	snapshot := agent.Snapshot()
	snapshot.State.Messages[0] = UserMsg("changed history")
	snapshot.SteeringQueue[0] = UserMsg("changed steer")
	snapshot.FollowUpQueue[0] = UserMsg("changed follow up")

	again := agent.Snapshot()
	if got := again.State.Messages[0].TextContent(); got != "history" {
		t.Fatalf("agent history changed through snapshot: %q", got)
	}
	if got := again.SteeringQueue[0].TextContent(); got != "steer" {
		t.Fatalf("steering queue changed through snapshot: %q", got)
	}
	if got := again.FollowUpQueue[0].TextContent(); got != "follow up" {
		t.Fatalf("follow-up queue changed through snapshot: %q", got)
	}
}

func TestAgentTurnEndListenerCanCaptureSnapshotBoundary(t *testing.T) {
	agent := NewAgent(WithModel(mockModel(
		assistantMsg("partial", StopReasonLength),
		assistantMsg("done", StopReasonStop),
		assistantMsg("followed up", StopReasonStop),
	)))
	var boundary AgentSnapshot
	agent.Subscribe(func(event Event) {
		if event.Type != EventTurnEnd || event.State == nil || event.State.Progress.CompletedTurns != 1 {
			return
		}
		agent.FollowUp(UserMsg("accepted at boundary"))
		boundary = agent.Snapshot()
	})

	if err := agent.Prompt(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()

	if boundary.State.Progress.CompletedTurns != 1 || !boundary.State.Progress.NextTurn {
		t.Fatalf("boundary progress = %#v", boundary.State.Progress)
	}
	if len(boundary.State.Progress.PendingMessages) != 1 {
		t.Fatalf("boundary pending messages = %#v", boundary.State.Progress.PendingMessages)
	}
	if got := boundary.State.Progress.PendingMessages[0].TextContent(); got != defaultLengthRecoveryPrompt {
		t.Fatalf("boundary pending message = %q", got)
	}
	if len(boundary.FollowUpQueue) != 1 {
		t.Fatalf("boundary follow-up queue = %#v", boundary.FollowUpQueue)
	}
	if got := boundary.FollowUpQueue[0].TextContent(); got != "accepted at boundary" {
		t.Fatalf("boundary follow-up = %q", got)
	}
}

func TestAgentSetSnapshotRestoresFollowUpQueue(t *testing.T) {
	var modelMessages []string
	agent := NewAgent(WithModel(funcModel(func(_ context.Context, req *LLMRequest) (*LLMResponse, error) {
		for _, message := range req.Messages {
			modelMessages = append(modelMessages, message.TextContent())
		}
		return &LLMResponse{Message: assistantMsg("continued", StopReasonStop)}, nil
	})))
	snapshot := AgentSnapshot{
		State: AgentState{Messages: []AgentMessage{
			UserMsg("request"),
			assistantMsg("initial answer", StopReasonStop),
		}},
		FollowUpQueue: []AgentMessage{UserMsg("add details")},
	}

	if err := agent.SetSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := agent.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()

	if !slices.Contains(modelMessages, "add details") {
		t.Fatalf("model messages = %v, want restored follow-up", modelMessages)
	}
	if agent.HasQueuedMessages() {
		t.Fatal("restored follow-up was not consumed")
	}
}

func TestAgentContinueRestoresSnapshotBeforeRun(t *testing.T) {
	beforeRunCalls := 0
	var (
		beforeRunKind     RunKind
		beforeRunMessages int
		turnIndexes       []int
	)
	agent := NewAgent(
		WithBeforeRun(func(_ context.Context, run BeforeRunContext) (AgentSnapshot, error) {
			beforeRunCalls++
			beforeRunKind = run.Kind
			beforeRunMessages = len(run.Snapshot.State.Messages)
			return AgentSnapshot{State: AgentState{
				Messages: []AgentMessage{
					UserMsg("request"),
					assistantMsg("partial", StopReasonLength),
				},
				Progress: RunProgress{
					NextTurn:        true,
					CompletedTurns:  4,
					PendingMessages: []AgentMessage{UserMsg("resume without recap")},
				},
			}}, nil
		}),
		WithModel(mockModel(assistantMsg("finished", StopReasonStop))),
		WithModelMiddlewares(func(ctx context.Context, execution ModelExecution, next ModelExecuteFunc) (ModelResult, error) {
			turnIndexes = append(turnIndexes, execution.TurnIndex)
			return next(ctx, execution)
		}),
	)

	if err := agent.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()

	if beforeRunCalls != 1 {
		t.Fatalf("BeforeRun calls = %d, want 1", beforeRunCalls)
	}
	if beforeRunKind != RunKindContinue || beforeRunMessages != 0 {
		t.Fatalf("BeforeRun input = kind %q messages %d", beforeRunKind, beforeRunMessages)
	}
	if !slices.Equal(turnIndexes, []int{5}) {
		t.Fatalf("turn indexes = %v, want [5]", turnIndexes)
	}
}

func TestAgentBeforeRunFailureCanRetry(t *testing.T) {
	loadErr := errors.New("snapshot store unavailable")
	beforeRunCalls := 0
	afterRunCalls := 0
	agent := NewAgent(
		WithBeforeRun(func(context.Context, BeforeRunContext) (AgentSnapshot, error) {
			beforeRunCalls++
			if beforeRunCalls == 1 {
				return AgentSnapshot{}, loadErr
			}
			return AgentSnapshot{State: AgentState{
				Messages: []AgentMessage{UserMsg("restored request")},
			}}, nil
		}),
		WithAfterRun(func(context.Context, AfterRunContext) error {
			afterRunCalls++
			return nil
		}),
		WithModel(mockModel(assistantMsg("done", StopReasonStop))),
	)

	if err := agent.Continue(context.Background()); !errors.Is(err, loadErr) {
		t.Fatalf("first Continue error = %v, want loader error", err)
	}
	if agent.State().IsRunning {
		t.Fatal("agent started after snapshot load failure")
	}
	if err := agent.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()

	if beforeRunCalls != 2 {
		t.Fatalf("BeforeRun calls = %d, want 2", beforeRunCalls)
	}
	if afterRunCalls != 1 {
		t.Fatalf("AfterRun calls = %d, want only the accepted run", afterRunCalls)
	}
}

func TestAgentSetSnapshotResumesPendingTurn(t *testing.T) {
	var (
		modelMessages []string
		turnIndexes   []int
	)
	agent := NewAgent(
		WithModel(funcModel(func(_ context.Context, req *LLMRequest) (*LLMResponse, error) {
			for _, message := range req.Messages {
				modelMessages = append(modelMessages, message.TextContent())
			}
			return &LLMResponse{Message: assistantMsg("finished", StopReasonStop)}, nil
		})),
		WithModelMiddlewares(func(ctx context.Context, execution ModelExecution, next ModelExecuteFunc) (ModelResult, error) {
			turnIndexes = append(turnIndexes, execution.TurnIndex)
			return next(ctx, execution)
		}),
	)
	snapshot := AgentSnapshot{State: AgentState{
		Messages: []AgentMessage{
			UserMsg("request"),
			assistantMsg("partial", StopReasonLength),
		},
		Progress: RunProgress{
			NextTurn:        true,
			CompletedTurns:  4,
			PendingMessages: []AgentMessage{UserMsg("resume without recap")},
		},
	}}

	if err := agent.SetSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := agent.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()

	if !slices.Equal(turnIndexes, []int{5}) {
		t.Fatalf("turn indexes = %v, want [5]", turnIndexes)
	}
	if !slices.Contains(modelMessages, "resume without recap") {
		t.Fatalf("model messages = %v, want restored pending message", modelMessages)
	}
	if got := agent.State().Progress.CompletedTurns; got != 5 {
		t.Fatalf("completed turns = %d, want 5", got)
	}
}

func TestAgentSetSnapshotRefusesRunningAgent(t *testing.T) {
	release := make(chan struct{})
	agent := NewAgent(WithModel(funcModel(func(context.Context, *LLMRequest) (*LLMResponse, error) {
		<-release
		return &LLMResponse{Message: assistantMsg("done", StopReasonStop)}, nil
	})))
	if err := agent.Prompt(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}

	err := agent.SetSnapshot(AgentSnapshot{})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("SetSnapshot error = %v, want ErrAlreadyRunning", err)
	}
	close(release)
	agent.WaitForIdle()
}

func TestAgentLoopNewPromptResetsInactiveContinuation(t *testing.T) {
	var (
		modelMessages []string
		turnIndexes   []int
	)
	events := runTestLoop(t,
		[]AgentMessage{UserMsg("new request")},
		AgentContext{},
		LoopConfig{
			Model: funcModel(func(_ context.Context, req *LLMRequest) (*LLMResponse, error) {
				for _, message := range req.Messages {
					modelMessages = append(modelMessages, message.TextContent())
				}
				return &LLMResponse{Message: assistantMsg("done", StopReasonStop)}, nil
			}),
			InitialState: AgentState{Progress: RunProgress{
				NextTurn:        true,
				CompletedTurns:  4,
				PendingMessages: []AgentMessage{UserMsg("stale continuation")},
			}},
			ModelMiddlewares: []ModelMiddleware{func(ctx context.Context, execution ModelExecution, next ModelExecuteFunc) (ModelResult, error) {
				turnIndexes = append(turnIndexes, execution.TurnIndex)
				return next(ctx, execution)
			}},
		},
	)

	if !slices.Equal(turnIndexes, []int{1}) {
		t.Fatalf("turn indexes = %v, want [1]", turnIndexes)
	}
	if slices.Contains(modelMessages, "stale continuation") {
		t.Fatalf("new run reused stale continuation: %v", modelMessages)
	}
	end, ok := findEvent(events, EventAgentEnd)
	if !ok || end.State == nil || end.State.Progress.CompletedTurns != 1 {
		t.Fatalf("final state = %#v", end.State)
	}
}
