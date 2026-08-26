package agentgo

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	defaultMaxTurns            = 100
	defaultMaxRetries          = 3
	defaultMaxLengthRecoveries = 3
	defaultMaxRetryDelay       = 60 * time.Second
)

const defaultLengthRecoveryPrompt = "Output token limit hit. Resume directly - no apology, no recap of what you were doing. Pick up mid-thought if that is where the cut happened. Break remaining work into smaller pieces."

const (
	defaultAbortMarkerText     = "[Request interrupted by user]"
	defaultAbortMarkerToolText = "[Request interrupted by user for tool use]"
)

// AgentLoop starts an agent loop with new prompt messages.
// Prompts are added to context and events are emitted for them.
//
// The returned channel MUST be consumed until it closes: while the run is
// live the loop blocks on a full channel (backpressure, no event loss), so
// abandoning the channel without canceling ctx leaks the loop goroutine.
// To stop early, cancel ctx and keep draining — after cancellation delivery
// degrades to best-effort and the loop is guaranteed to exit and close the
// channel even if no one is reading.
func AgentLoop(ctx context.Context, prompts []AgentMessage, agentCtx AgentContext, config LoopConfig) <-chan Event {
	return startAgentLoop(ctx, prompts, agentCtx, config, false)
}

// AgentLoopContinue continues from existing context without adding new messages.
// The last message in context must convert to user or tool role via ToMessage.
//
// The returned channel follows the same consumption contract as AgentLoop:
// drain until close, or cancel ctx and keep draining to stop early.
func AgentLoopContinue(ctx context.Context, agentCtx AgentContext, config LoopConfig) <-chan Event {
	return startAgentLoop(ctx, nil, agentCtx, config, true)
}

func startAgentLoop(ctx context.Context, prompts []AgentMessage, agentCtx AgentContext, config LoopConfig, continuing bool) <-chan Event {
	ch := make(chan Event, 128)
	terminal := &terminalEvent{}
	sink := eventSink{ctx: ctx, ch: ch, terminal: terminal}

	go func() {
		var newMessages []AgentMessage
		currentCtx := AgentContext{
			SystemPrompt: agentCtx.SystemPrompt,
			SystemBlocks: append([]SystemBlock(nil), agentCtx.SystemBlocks...),
			Messages:     copyMessages(agentCtx.Messages),
			Tools:        append([]Tool(nil), agentCtx.Tools...),
		}
		state := config.InitialState
		if state.Messages == nil {
			state.Messages = copyMessages(agentCtx.Messages)
		}
		state = bindRunState(state, &currentCtx)
		state.Progress = activateRunProgress(state.Progress, continuing)

		defer func() {
			if recovered := recover(); recovered != nil {
				sink.emitError(fmt.Errorf("agent loop panicked: %v", recovered), &RunSummary{EndReason: EndReasonError})
			}
			finishAgentRun(&currentCtx, &state, sink)
			sink.flushTerminal()
			close(ch)
		}()

		if continuing && len(currentCtx.Messages) == 0 {
			sink.emitError(ErrNoMessages, &RunSummary{EndReason: EndReasonError})
			return
		}

		startState := snapshotRunState(state, &currentCtx)
		sink.emit(Event{Type: EventAgentStart, State: &startState})
		sink.emit(Event{Type: EventTurnStart})

		for _, prompt := range prompts {
			sink.emit(Event{Type: EventMessageStart, Message: prompt})
			if err := commitMessage(&currentCtx, &newMessages, config, prompt); err != nil {
				sink.emitError(fmt.Errorf("commit prompt message: %w", err), &RunSummary{EndReason: EndReasonError})
				return
			}
			sink.emit(Event{Type: EventMessageEnd, Message: prompt})
		}

		runLoop(ctx, &currentCtx, &newMessages, config, sink, &state)
	}()

	return ch
}

// activateRunProgress distinguishes a new run from continuation of a saved
// turn boundary. An inactive state carries resumable progress only when the
// Loop had already decided that another turn was required.
func activateRunProgress(progress RunProgress, continuing bool) RunProgress {
	if !continuing {
		return RunProgress{Active: true}
	}
	if progress.Active {
		return progress
	}
	if progress.NextTurn || len(progress.PendingMessages) > 0 {
		progress.Active = true
		return progress
	}
	return RunProgress{Active: true}
}

func bindRunState(state AgentState, current *AgentContext) AgentState {
	state.SystemPrompt = current.SystemPrompt
	state.Tools = append([]Tool(nil), current.Tools...)
	state.IsRunning = true
	state.StreamMessage = nil
	state.PendingToolCalls = nil
	state.Error = ""
	state.Progress = cloneRunProgress(state.Progress)
	return state
}

func snapshotRunState(state AgentState, current *AgentContext) AgentState {
	state.Messages = copyMessages(current.Messages)
	state.Progress = cloneRunProgress(state.Progress)
	state.Tools = append([]Tool(nil), current.Tools...)
	return state
}

func finishAgentRun(current *AgentContext, state *AgentState, sink eventSink) {
	if sink.terminal.event == nil {
		sink.emitError(errors.New("agent loop ended without terminal event"), &RunSummary{EndReason: EndReasonError})
	}
	state.Progress.Active = false
	state.IsRunning = false
	finalState := snapshotRunState(*state, current)
	sink.terminal.event.State = &finalState
}

// commitMessage is the single entry point for "message enters agent context".
// Durable persistence runs first; only a successful commit is appended to the
// runtime context and exposed to the observational OnMessage hook.
func commitMessage(currentCtx *AgentContext, newMessages *[]AgentMessage, config LoopConfig, msg AgentMessage) error {
	if config.CommitMessage != nil {
		if err := config.CommitMessage(msg); err != nil {
			return err
		}
	}
	currentCtx.Messages = append(currentCtx.Messages, msg)
	*newMessages = append(*newMessages, msg)
	if config.OnMessage != nil {
		config.OnMessage(msg)
	}
	return nil
}

// runLoop is the turn loop shared by AgentLoop and AgentLoopContinue.
//
// Core loop contracts:
//   - Streamed tool-call lifecycle signals are authoritative; stop reasons are
//     only hints and must not be used as the sole source of tool state.
//   - Tool execution starts only after the complete assistant message has been
//     committed. This lets CommitMessage establish the tool-call
//     record before a tool can produce side effects.
//   - Tool results are appended after the assistant message that requested them.
//   - Steering stops not-yet-started tools. Started tools follow their
//     InterruptBehavior and may continue or be cancelled.
func runLoop(ctx context.Context, currentCtx *AgentContext, newMessages *[]AgentMessage, config LoopConfig, sink eventSink, state *AgentState) {
	type runSummaryState struct {
		toolCalls  int
		toolErrors int
	}
	summaryState := runSummaryState{
		toolCalls:  state.Progress.ToolCalls,
		toolErrors: state.Progress.ToolErrors,
	}
	buildSummary := func(turnCount int, reason EndReason) *RunSummary {
		return &RunSummary{
			TurnCount:  turnCount,
			ToolCalls:  summaryState.toolCalls,
			ToolErrors: summaryState.toolErrors,
			EndReason:  reason,
		}
	}
	maxTurns := config.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}

	firstTurn := true
	turnCount := state.Progress.CompletedTurns
	lengthRecoveryCount := state.Progress.LengthRecoveries
	toolErrors := cloneStringIntMap(state.Progress.ConsecutiveToolErrors)
	if toolErrors == nil {
		toolErrors = make(map[string]int)
	}
	commit := func(msg AgentMessage) bool {
		if err := commitMessage(currentCtx, newMessages, config, msg); err != nil {
			sink.emitError(fmt.Errorf("commit message: %w", err), buildSummary(turnCount, EndReasonError))
			return false
		}
		state.Messages = copyMessages(currentCtx.Messages)
		if message, ok := msg.(Message); ok {
			state.TotalUsage.Add(message.Usage)
		}
		return true
	}
	runBeforeTurn := func(turnIndex int) bool {
		if config.BeforeTurn == nil {
			return true
		}
		messages, err := config.BeforeTurn(ctx, BeforeTurnContext{
			TurnIndex: turnIndex,
			Context:   snapshotAgentContext(currentCtx),
		})
		if err != nil {
			sink.emitError(fmt.Errorf("before turn %d: %w", turnIndex, err), buildSummary(turnCount, EndReasonError))
			return false
		}
		for _, message := range messages {
			sink.emit(Event{Type: EventMessageStart, Message: message})
			if !commit(message) {
				return false
			}
			sink.emit(Event{Type: EventMessageEnd, Message: message})
		}
		return true
	}
	runAfterTurn := func(turnIndex int, message AgentMessage, results []ToolResult) bool {
		state.Progress.CompletedTurns = turnIndex
		state.Progress.LengthRecoveries = lengthRecoveryCount
		state.Progress.ToolCalls = summaryState.toolCalls
		state.Progress.ToolErrors = summaryState.toolErrors
		state.Progress.ConsecutiveToolErrors = cloneStringIntMap(toolErrors)
		turnState := snapshotRunState(*state, currentCtx)
		sink.emit(Event{Type: EventTurnEnd, Message: message, ToolResults: append([]ToolResult(nil), results...), State: &turnState})
		if config.AfterTurn == nil {
			return true
		}
		if err := config.AfterTurn(ctx, AfterTurnContext{
			TurnIndex:   turnIndex,
			Message:     message,
			ToolResults: append([]ToolResult(nil), results...),
			Context:     snapshotAgentContext(currentCtx),
			State:       turnState,
		}); err != nil {
			sink.emitError(fmt.Errorf("after turn %d: %w", turnIndex, err), buildSummary(turnCount, EndReasonError))
			return false
		}
		return true
	}

	// Check for steering messages at start
	pendingMessages := copyMessages(state.Progress.PendingMessages)
	state.Progress.PendingMessages = nil
	if len(pendingMessages) == 0 && config.GetSteeringMessages != nil {
		pendingMessages = config.GetSteeringMessages()
	}

	// Track last assistant message so StopGuard can inspect what's stopping us.
	var lastAssistantMsg Message

	if state.Progress.CompletedTurns > 0 && !state.Progress.NextTurn {
		sink.emit(Event{Type: EventAgentEnd, NewMessages: *newMessages, Summary: buildSummary(turnCount, EndReasonStop)})
		return
	}

	afterToolExec := false
	for {
		var steeringAfterTools []AgentMessage
		hasMoreToolCalls := false
		// Check for context cancellation (Abort)
		if ctx.Err() != nil {
			if config.ShouldEmitAbortMarker != nil && config.ShouldEmitAbortMarker() {
				phase := "inference"
				text := config.AbortMarkerText
				if text == "" {
					text = defaultAbortMarkerText
				}
				if afterToolExec {
					phase = "tool_execution"
					text = config.AbortMarkerToolText
					if text == "" {
						text = defaultAbortMarkerToolText
					}
				}
				abortMsg := AbortMsg(text, phase)
				if !commit(abortMsg) {
					return
				}
				sink.emit(Event{Type: EventMessageEnd, Message: abortMsg})
			}
			sink.emit(Event{Type: EventError, Err: ctx.Err()})
			sink.emit(Event{Type: EventAgentEnd, NewMessages: *newMessages, Summary: buildSummary(turnCount, EndReasonAborted)})
			return
		}

		if turnCount >= maxTurns {
			sink.emit(Event{Type: EventError, Err: &MaxTurnsError{Limit: maxTurns}})
			sink.emit(Event{Type: EventAgentEnd, NewMessages: *newMessages, Summary: buildSummary(turnCount, EndReasonMaxTurns)})
			return
		}

		if !firstTurn {
			sink.emit(Event{Type: EventTurnStart})
		} else {
			firstTurn = false
		}

		// Process pending messages (inject before next LLM call)
		if len(pendingMessages) > 0 {
			for _, msg := range pendingMessages {
				sink.emit(Event{Type: EventMessageStart, Message: msg})
				if !commit(msg) {
					return
				}
				sink.emit(Event{Type: EventMessageEnd, Message: msg})
			}
			pendingMessages = nil
		}
		if !runBeforeTurn(turnCount + 1) {
			return
		}
		// Call LLM with retry (streaming: events emitted inside callLLM)
		assistantMsg, callInfo, err := callLLMWithRetry(ctx, currentCtx, config, turnCount+1, sink)
		if err != nil {
			if ctx.Err() != nil {
				if config.ShouldEmitAbortMarker != nil && config.ShouldEmitAbortMarker() {
					text := config.AbortMarkerText
					if text == "" {
						text = defaultAbortMarkerText
					}
					abortMsg := AbortMsg(text, "inference")
					if !commit(abortMsg) {
						return
					}
					sink.emit(Event{Type: EventMessageEnd, Message: abortMsg})
				}
				sink.emit(Event{Type: EventError, Err: ctx.Err()})
				sink.emit(Event{Type: EventAgentEnd, NewMessages: *newMessages, Summary: buildSummary(turnCount, EndReasonAborted)})
				return
			}
			sink.emitError(err, buildSummary(turnCount, EndReasonError))
			return
		}
		// Check stop reason — terminate early on error/aborted
		if assistantMsg.StopReason == StopReasonError || assistantMsg.StopReason == StopReasonAborted {
			if !commit(assistantMsg) {
				return
			}
			sink.emit(Event{Type: EventMessageEnd, Execution: executionRef(callInfo.Execution), Message: assistantMsg})
			sink.emit(Event{Type: EventModelResponse, Execution: executionRef(callInfo.Execution), Message: assistantMsg})
			turnCount++
			state.Progress.NextTurn = false
			state.Progress.PendingMessages = nil
			if !runAfterTurn(turnCount, assistantMsg, nil) {
				return
			}
			reason := EndReasonError
			if assistantMsg.StopReason == StopReasonAborted {
				reason = EndReasonAborted
			}
			sink.emit(Event{Type: EventAgentEnd, NewMessages: *newMessages, Summary: buildSummary(turnCount, reason)})
			return
		}

		// When output was truncated (max_tokens hit), tool calls are likely
		// incomplete with malformed JSON args. Strip them to avoid validation
		// errors and API rejections.
		if assistantMsg.StopReason == StopReasonLength && !callInfo.HasCompletedToolCalls {
			assistantMsg.Content = stripToolCallBlocks(assistantMsg.Content)
		}

		lastAssistantMsg = assistantMsg
		if !commit(assistantMsg) {
			return
		}
		sink.emit(Event{Type: EventMessageEnd, Execution: executionRef(callInfo.Execution), Message: assistantMsg})

		// Check for tool calls
		toolCalls := assistantMsg.ToolCalls()
		summaryState.toolCalls += len(toolCalls)
		hasMoreToolCalls = len(toolCalls) > 0
		// Recover when output was truncated and no tool calls completed.
		// This includes the case where tool call blocks existed but were
		// stripped due to incomplete JSON — the tool was never executed,
		// so recovery is safe. The recovery prompt tells the model to
		// "break remaining work into smaller pieces."
		shouldRecoverLength := assistantMsg.StopReason == StopReasonLength &&
			len(toolCalls) == 0 &&
			!callInfo.HasCompletedToolCalls &&
			lengthRecoveryCount < defaultMaxLengthRecoveries

		var turnToolResults []ToolResult
		if hasMoreToolCalls {
			var steering []AgentMessage
			turnToolResults, steering = executeToolCalls(ctx, turnCount+1, currentCtx.Tools, toolCalls, config, toolErrors, sink)
			afterToolExec = true

			for _, tr := range turnToolResults {
				call := findToolCall(toolCalls, tr.ToolCallID)
				execution := newToolExecution(turnCount+1, call)
				resultMsg := toolResultMessage(config, call, tr)
				sink.emit(Event{Type: EventMessageStart, Execution: executionRef(execution.Execution), Message: resultMsg})
				if !commit(resultMsg) {
					return
				}
				sink.emit(Event{Type: EventMessageEnd, Execution: executionRef(execution.Execution), Message: resultMsg})
			}

			steeringAfterTools = steering
		}
		for _, tr := range turnToolResults {
			if tr.IsError {
				summaryState.toolErrors++
			}
		}

		sink.emit(Event{Type: EventModelResponse, Execution: executionRef(callInfo.Execution), Message: assistantMsg, ToolResults: turnToolResults})
		turnCount++
		// Early exit: a terminal tool completed successfully. This is a
		// normal stop, so it passes through the same StopGuard gate as
		// end_turn — guards stay the single stop arbiter and can veto a
		// premature terminal-tool exit (Trigger distinguishes the paths).
		if stopAfterToolHit(config, turnToolResults) {
			inject, escalate := consultStopGuard(ctx, config, StopInfo{
				TurnIndex: turnCount,
				Message:   lastAssistantMsg,
				Trigger:   StopTriggerAfterTool,
			})
			if escalate {
				state.Progress.NextTurn = false
				state.Progress.PendingMessages = nil
				if !runAfterTurn(turnCount, assistantMsg, turnToolResults) {
					return
				}
				sink.emit(Event{Type: EventError, Err: ErrStopGuard})
				sink.emit(Event{Type: EventAgentEnd, NewMessages: *newMessages, Summary: buildSummary(turnCount, EndReasonError)})
				return
			}
			if inject == "" {
				state.Progress.NextTurn = false
				state.Progress.PendingMessages = nil
				if !runAfterTurn(turnCount, assistantMsg, turnToolResults) {
					return
				}
				sink.emit(Event{Type: EventAgentEnd, NewMessages: *newMessages, Summary: buildSummary(turnCount, EndReasonStop)})
				return
			}
			// Guard vetoed the early exit: keep the loop alive with the
			// injected message, carrying any steering captured during this
			// terminal-tool turn so a follow-up tool turn can't drop the
			// already-dequeued steering.
			pendingMessages = append([]AgentMessage{UserMsg(inject)}, steeringAfterTools...)
			steeringAfterTools = nil
			state.Progress.NextTurn = true
			state.Progress.PendingMessages = copyMessages(pendingMessages)
			if !runAfterTurn(turnCount, assistantMsg, turnToolResults) {
				return
			}
			continue
		}

		if shouldRecoverLength {
			lengthRecoveryCount++
			prompt := config.LengthRecoveryPrompt
			if prompt == "" {
				prompt = defaultLengthRecoveryPrompt
			}
			pendingMessages = []AgentMessage{UserMsg(prompt)}
			state.Progress.LengthRecoveries = lengthRecoveryCount
			state.Progress.NextTurn = true
			state.Progress.PendingMessages = copyMessages(pendingMessages)
			if !runAfterTurn(turnCount, assistantMsg, turnToolResults) {
				return
			}
			continue
		}

		// Get steering messages after turn completes
		if len(steeringAfterTools) > 0 {
			pendingMessages = steeringAfterTools
			steeringAfterTools = nil
		} else if config.GetSteeringMessages != nil {
			pendingMessages = config.GetSteeringMessages()
		}
		state.Progress.PendingMessages = copyMessages(pendingMessages)
		if hasMoreToolCalls || len(pendingMessages) > 0 {
			state.Progress.NextTurn = true
			if !runAfterTurn(turnCount, assistantMsg, turnToolResults) {
				return
			}
			continue
		}

		// Agent would stop here. Check for follow-up messages.
		if config.GetFollowUpMessages != nil {
			followUp := config.GetFollowUpMessages()
			if len(followUp) > 0 {
				afterToolExec = false
				pendingMessages = followUp
				state.Progress.NextTurn = true
				state.Progress.PendingMessages = copyMessages(pendingMessages)
				if !runAfterTurn(turnCount, assistantMsg, turnToolResults) {
					return
				}
				continue
			}
		}

		// StopGuard veto: give the application a chance to keep the loop alive.
		inject, escalate := consultStopGuard(ctx, config, StopInfo{
			TurnIndex: turnCount,
			Message:   lastAssistantMsg,
			Trigger:   StopTriggerEndTurn,
		})
		if escalate {
			state.Progress.NextTurn = false
			if !runAfterTurn(turnCount, assistantMsg, turnToolResults) {
				return
			}
			sink.emit(Event{Type: EventError, Err: ErrStopGuard})
			sink.emit(Event{Type: EventAgentEnd, NewMessages: *newMessages, Summary: buildSummary(turnCount, EndReasonError)})
			return
		}
		if inject != "" {
			afterToolExec = false
			pendingMessages = []AgentMessage{UserMsg(inject)}
			state.Progress.NextTurn = true
			state.Progress.PendingMessages = copyMessages(pendingMessages)
			if !runAfterTurn(turnCount, assistantMsg, turnToolResults) {
				return
			}
			continue
		}

		state.Progress.NextTurn = false
		state.Progress.PendingMessages = nil
		if !runAfterTurn(turnCount, assistantMsg, turnToolResults) {
			return
		}
		sink.emit(Event{Type: EventAgentEnd, NewMessages: *newMessages, Summary: buildSummary(turnCount, EndReasonStop)})
		return
	}
}

// stopAfterToolHit reports whether any successful tool result in this turn
// matches a StopAfterTool / StopAfterToolResult hook.
func stopAfterToolHit(config LoopConfig, results []ToolResult) bool {
	if config.StopAfterTool == nil && config.StopAfterToolResult == nil {
		return false
	}
	for _, tr := range results {
		if tr.IsError {
			continue
		}
		if config.StopAfterTool != nil && config.StopAfterTool(tr.ToolName) {
			return true
		}
		if config.StopAfterToolResult != nil && config.StopAfterToolResult(tr.ToolName, tr.Content) {
			return true
		}
	}
	return false
}

// consultStopGuard runs the guard at a would-stop point. A non-empty inject
// keeps the loop alive with that message; escalate ends the run with
// ErrStopGuard. Both zero values mean the stop is allowed (including when no
// guard is configured, or when the guard denies without an InjectMessage —
// never stall silently).
func consultStopGuard(ctx context.Context, config LoopConfig, info StopInfo) (inject string, escalate bool) {
	if config.StopGuard == nil {
		return "", false
	}
	decision := config.StopGuard(ctx, info)
	if decision.Escalate {
		return "", true
	}
	if !decision.Allow && decision.InjectMessage != "" {
		return decision.InjectMessage, false
	}
	return "", false
}

func copyMessages(msgs []AgentMessage) []AgentMessage {
	out := make([]AgentMessage, len(msgs))
	copy(out, msgs)
	return out
}
