package agentgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
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

type llmCallInfo struct {
	Execution             Execution
	HasCompletedToolCalls bool
}

// callLLMWithRetry wraps callLLM with retry logic for retryable errors.
// Context overflow errors trigger automatic compaction and a single retry.
//
// Tool execution is deliberately outside this function and starts only after
// a complete response has been committed, so retrying a failed stream cannot
// replay tool side effects. callOptions are selected once per logical model
// call and reused by every retry attempt.
func callLLMWithRetry(ctx context.Context, agentCtx *AgentContext, config LoopConfig, turnIndex int, sink eventSink) (Message, llmCallInfo, error) {
	ctx = withModelExecutionRuntime(ctx, config.ModelMiddlewares, sink.emit)
	maxRetries := config.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	var lastErr error
	var lastInfo llmCallInfo
	for attempt := 1; attempt <= maxRetries+1; attempt++ {
		msg, info, err := callLLM(ctx, agentCtx, config, turnIndex, attempt, sink)
		if err == nil {
			return msg, info, nil
		}
		lastErr = err
		lastInfo = info

		// Context overflow: compact and retry once (not a normal retry)
		if IsContextOverflow(err) {
			return recoverOverflow(ctx, agentCtx, config, turnIndex, attempt+1, sink, err)
		}

		// User cancellation is never retryable: the next attempt would just
		// rediscover ctx.Done(), and emitting EventRetry in that window surfaces
		// confusing "retry (1/N)" messages to users who already aborted. Aligns
		// with IsFailoverEligible (errors.go), which also treats
		// context.Canceled as terminal.
		if errors.Is(err, context.Canceled) {
			return Message{}, info, err
		}

		// PartialStreamError (stream closed without done) is treated as retryable:
		// it most often signals a transient network/provider stream-format issue
		// that a fresh request can recover from.
		var pse *PartialStreamError
		retryable := isRetryable(err) || errors.As(err, &pse)
		if !retryable || attempt == maxRetries+1 {
			return Message{}, info, err
		}

		delay := retryDelay(err, attempt-1)

		sink.emit(Event{
			Type:      EventRetry,
			Execution: executionRef(info.Execution),
			Err:       err,
			RetryInfo: &RetryInfo{
				MaxRetries: maxRetries,
				Delay:      delay,
				Err:        err,
			},
		})

		select {
		case <-ctx.Done():
			return Message{}, lastInfo, ctx.Err()
		case <-time.After(delay):
		}
	}
	return Message{}, lastInfo, lastErr
}

// recoverOverflow attempts to compact the context via the ContextManager and
// retry the LLM call. If no ContextManager is configured, the original error
// is returned.
func recoverOverflow(ctx context.Context, agentCtx *AgentContext, config LoopConfig, turnIndex, attempt int, sink eventSink, originalErr error) (Message, llmCallInfo, error) {
	failedExecution := newModelExecution(turnIndex, attempt-1).Execution
	if config.ContextManager == nil {
		return Message{}, llmCallInfo{Execution: failedExecution}, &ContextOverflowError{Cause: fmt.Errorf("no compaction configured: %w", originalErr)}
	}

	sink.emit(Event{
		Type:      EventRetry,
		Execution: executionRef(failedExecution),
		Err:       originalErr,
		RetryInfo: &RetryInfo{
			MaxRetries: 1,
			Err:        fmt.Errorf("context overflow detected, compacting and retrying"),
		},
	})

	compactExecution := newCompactExecution(turnIndex, CompactReasonOverflow, 1)
	recoveryCtx := ContextWithExecution(ctx, compactExecution)
	recovery, err := config.ContextManager.RecoverOverflow(recoveryCtx, agentCtx.Messages, originalErr)
	if err != nil {
		return Message{}, llmCallInfo{Execution: failedExecution}, &ContextOverflowError{Cause: fmt.Errorf("compaction failed: %w", err)}
	}
	if len(recovery.View) == 0 {
		return Message{}, llmCallInfo{Execution: failedExecution}, &ContextOverflowError{Cause: errors.New("compaction returned empty prompt view")}
	}
	agentCtx.Messages = recovery.View
	if recovery.ShouldCommit && len(recovery.CommitMessages) > 0 && config.CommitContext != nil {
		if err := config.CommitContext(recovery.CommitMessages, recovery.Usage); err != nil {
			return Message{}, llmCallInfo{}, &ContextOverflowError{Cause: fmt.Errorf("commit failed: %w", err)}
		}
	}
	if recovery.Compaction != nil {
		sink.emit(Event{Type: EventContextCompacted, Execution: executionRef(compactExecution), Compaction: recovery.Compaction})
	}
	return callLLM(ctx, agentCtx, config, turnIndex, attempt, sink)
}

// retryDelay calculates the wait duration using exponential backoff.
// Respects Retry-After from rate limit errors. Capped at defaultMaxRetryDelay.
func retryDelay(err error, attempt int) time.Duration {
	maxDelay := defaultMaxRetryDelay
	if after := retryAfterHint(err); after > 0 {
		d := after
		if d > maxDelay {
			d = maxDelay
		}
		return d
	}
	// Exponential backoff: 1s, 2s, 4s, 8s...
	d := time.Duration(math.Pow(2, float64(attempt))) * time.Second
	if d > maxDelay {
		d = maxDelay
	}
	return d
}

// callLLM applies the two-stage pipeline and calls the model.
func callLLM(ctx context.Context, agentCtx *AgentContext, config LoopConfig, turnIndex, attempt int, sink eventSink) (Message, llmCallInfo, error) {
	execution := newModelExecution(turnIndex, attempt)
	info := llmCallInfo{Execution: execution.Execution}
	messages := agentCtx.Messages

	// Stage 1: ContextManager / TransformContext
	if config.ContextManager != nil {
		compactExecution := newCompactExecution(turnIndex, CompactReasonThreshold, attempt)
		projectionCtx := ContextWithExecution(ctx, compactExecution)
		projection, err := config.ContextManager.Project(projectionCtx, messages)
		if err != nil {
			return Message{}, info, fmt.Errorf("project context: %w", err)
		}
		if projection.ShouldCommit && len(projection.CommitMessages) > 0 {
			if config.CommitContext != nil {
				if err := config.CommitContext(projection.CommitMessages, projection.Usage); err != nil {
					return Message{}, info, fmt.Errorf("project context commit failed: %w", err)
				}
			}
			agentCtx.Messages = copyMessages(projection.CommitMessages)
			messages = copyMessages(projection.CommitMessages)
		}
		if projection.Messages != nil {
			messages = projection.Messages
		}
		if projection.Compaction != nil {
			sink.emit(Event{Type: EventContextCompacted, Execution: executionRef(compactExecution), Compaction: projection.Compaction})
		}
	}
	sink.emit(Event{Type: EventContextProjected, Execution: executionRef(execution.Execution), ContextItems: CollectContextItems(messages)})

	// Stage 2: AgentMessage[] → Message[] + repair tool-call / tool-result
	// pairing for provider compatibility.
	llmMessages := RepairMessageSequence(ToMessages(messages))

	// Build tool specs
	toolSpecs := buildToolSpecs(agentCtx.Tools)

	// Prepend the static system prompt as first message(s). Keeping it at the
	// head and byte-stable across turns lets providers with prefix-based
	// caching (OpenAI) serve it from cache.
	var prefix []Message
	if len(agentCtx.SystemBlocks) > 0 {
		for _, b := range agentCtx.SystemBlocks {
			m := SystemMsg(b.Text)
			if b.CacheControl != "" {
				m.Metadata = map[string]any{"cache_control": b.CacheControl}
			}
			prefix = append(prefix, m)
		}
	} else if agentCtx.SystemPrompt != "" {
		m := SystemMsg(agentCtx.SystemPrompt)
		if config.CacheLastMessage != "" {
			// Cache floor: pin the static system prompt with its own
			// breakpoint so a fresh session — or a turn whose tail entry was
			// evicted — still reads the system+tools prefix from cache.
			// SystemBlocks users control placement explicitly and are left
			// untouched.
			m.Metadata = map[string]any{"cache_control": config.CacheLastMessage}
		}
		prefix = append(prefix, m)
	}
	if len(prefix) > 0 {
		llmMessages = append(prefix, llmMessages...)
	}

	// Place a single cache write breakpoint on the last non-system message when
	// the application has opted into explicit cache orchestration. The helper
	// scans from the tail and skips system reminders, so the breakpoint lands
	// on the freshest user input / tool_result / assistant turn — whichever is
	// last in this request. Inside a tool loop this means each LLM call writes
	// an entry covering the latest tool_use+tool_result, so the next call in
	// the loop reads them from cache instead of re-uploading.
	if config.CacheLastMessage != "" {
		llmMessages = markLastMessageForCache(llmMessages, config.CacheLastMessage)
	}

	// Build per-call options
	var callOpts []CallOption

	// Thinking level. Forward any explicit level, including ThinkingOff — the
	// litellm adapter translates "off" into an explicit disabled-thinking request
	// so models that think by default can actually be turned off. Empty means
	// "unset": leave it to the provider/model default.
	if config.ThinkingLevel != "" {
		callOpts = append(callOpts, WithThinking(config.ThinkingLevel))
	}

	// Prompt-cache routing identity — forwarded per call so the adapter can
	// gate it on provider capability.
	if config.PromptCacheKey != "" {
		callOpts = append(callOpts, WithCallPromptCacheKey(config.PromptCacheKey))
	}

	execution.Request = LLMRequest{Messages: llmMessages, Tools: toolSpecs}
	execution.Options = callOpts
	execute := func(ctx context.Context, call ModelExecution) (ModelResult, error) {
		if config.Model == nil {
			return ModelResult{}, ErrNoModel
		}
		message, callInfo, err := callLLMStream(ctx, config.Model, call, sink)
		return ModelResult{Message: message, HasCompletedToolCalls: callInfo.HasCompletedToolCalls}, err
	}
	result, err := ExecuteModel(ctx, execution, execute)
	info.HasCompletedToolCalls = result.HasCompletedToolCalls
	return result.Message, info, err
}

// modelExecutionID is derived from the turn rather than the physical attempt so a
// retry—or a run reloaded from the preceding turn boundary—addresses the
// same logical call. Hosts that persist calls must scope this opaque ID with
// their own run identity.
func modelExecutionID(turnIndex int) string {
	return fmt.Sprintf("model-%d", turnIndex)
}

func newModelExecution(turnIndex, attempt int) ModelExecution {
	return ModelExecution{Execution: Execution{
		ID:        modelExecutionID(turnIndex),
		Kind:      ExecutionKindModel,
		TurnIndex: turnIndex,
		Attempt:   attempt,
	}}
}

func newCompactExecution(turnIndex int, reason CompactReason, attempt int) Execution {
	return Execution{
		ID:        fmt.Sprintf("compact-%d-%s", turnIndex, reason),
		Kind:      ExecutionKindCompact,
		TurnIndex: turnIndex,
		Attempt:   attempt,
	}
}

// markLastMessageForCache returns a copy of messages with cache_control attached
// to the metadata of the last non-system message. System messages are skipped so
// trailing per-turn reminders (which change every turn) don't end up carrying
// the breakpoint. The caller's slice and the original Message values are left
// untouched.
func markLastMessageForCache(messages []Message, cacheControl string) []Message {
	idx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != RoleSystem {
			idx = i
			break
		}
	}
	if idx < 0 {
		return messages
	}
	out := slices.Clone(messages)
	md := maps.Clone(out[idx].Metadata)
	if md == nil {
		md = map[string]any{}
	}
	md["cache_control"] = cacheControl
	out[idx].Metadata = md
	return out
}

// callLLMStream uses GenerateStream and emits real-time events.
// The adapter builds partial Messages with ContentBlocks and emits fine-grained StreamEvents.
//
// Stream init failure is surfaced as an error — there is no silent fallback to
// non-streaming Generate, because callers (TUIs, event subscribers) typically
// depend on stream events for live rendering, tool-call deltas, and cancellation
// semantics. Switching execution model without notice changes the contract.
func callLLMStream(ctx context.Context, model ChatModel, execution ModelExecution, sink eventSink) (Message, llmCallInfo, error) {
	info := llmCallInfo{Execution: execution.Execution}
	streamCh, err := model.GenerateStream(ctx, execution.Request.Messages, execution.Request.Tools, execution.Options...)
	if err != nil {
		return Message{}, info, err
	}

	var (
		started bool
		partial Message
	)

	for ev := range streamCh {
		switch ev.Type {
		case StreamEventTextStart, StreamEventThinkingStart, StreamEventToolCallStart:
			partial = ev.Message
			if !started {
				started = true
				sink.emit(Event{Type: EventMessageStart, Execution: executionRef(execution.Execution), Message: partial})
			}

		case StreamEventTextDelta, StreamEventThinkingDelta, StreamEventToolCallDelta:
			partial = ev.Message
			if !started {
				started = true
				sink.emit(Event{Type: EventMessageStart, Execution: executionRef(execution.Execution), Message: partial})
			}
			var dk DeltaKind
			switch ev.Type {
			case StreamEventThinkingDelta:
				dk = DeltaThinking
			case StreamEventToolCallDelta:
				dk = DeltaToolCall
			}
			sink.emit(Event{Type: EventMessageUpdate, Execution: executionRef(execution.Execution), Message: partial, Delta: ev.Delta, DeltaKind: dk})

		case StreamEventTextEnd, StreamEventThinkingEnd, StreamEventToolCallEnd:
			partial = ev.Message
			if ev.CompletedToolCall != nil {
				info.HasCompletedToolCalls = true
			}

		case StreamEventDone:
			finalMsg := ev.Message
			finalMsg.Timestamp = time.Now()
			if !started {
				sink.emit(Event{Type: EventMessageStart, Execution: executionRef(execution.Execution), Message: finalMsg})
			}
			return finalMsg, info, nil

		case StreamEventError:
			return Message{}, info, ev.Err
		}
	}

	// Stream closed without done event — surface as PartialStreamError instead
	// of pretending the message completed. Emitting EventMessageEnd here would
	// let callers persist a half-finished message (missing StopReason, possibly
	// truncated tool_call args, unclosed thinking blocks) into history — the
	// next LLM call would then see structurally invalid context.
	return Message{}, info, &PartialStreamError{Partial: partial}
}

// executeToolCalls runs tool calls for one committed assistant turn using the
// shared concurrency and steering scheduler.
func executeToolCalls(ctx context.Context, turnIndex int, tools []Tool, calls []ToolCall, config LoopConfig, toolErrors map[string]int, sink eventSink) ([]ToolResult, []AgentMessage) {
	exec := newTurnToolExecutor(ctx, turnIndex, tools, config, toolErrors, sink)
	for _, call := range calls {
		exec.Add(call)
	}
	return exec.Wait()
}

// executeSingleToolCall wraps the complete validation, authorization and
// execution pipeline. A middleware can return a known ToolResult without
// invoking next, so replay never reaches gates or external side effects.
func executeSingleToolCall(ctx context.Context, tools []Tool, execution ToolExecution, config LoopConfig, failCount int, sink eventSink) ToolResult {
	call := execution.Call
	tool := findTool(tools, call.Name)
	label := toolLabel(tool)
	sink.emit(Event{
		Type:      EventToolExecStart,
		Execution: executionRef(execution.Execution),
		ToolID:    call.ID,
		Tool:      call.Name,
		ToolLabel: label,
		Args:      call.Args,
	})

	execute := func(ctx context.Context, execution ToolExecution) (ToolResult, error) {
		return executeToolCallCore(ctx, tool, execution, config, failCount, label, sink), nil
	}
	if len(config.ToolMiddlewares) > 0 {
		execute = buildToolMiddlewareChain(execute, config.ToolMiddlewares)
	}
	result, err := execute(ctx, execution)
	if err != nil {
		result = toolFailureResult(call, err.Error(), true)
	}
	// The assistant-issued ID is the durable pairing key. Middleware may
	// replace the outcome, but cannot retarget it to another tool call.
	result.ToolCallID = call.ID
	if !result.IsError && result.ToolName == "" {
		result.ToolName = call.Name
	}

	sink.emit(Event{
		Type:      EventToolExecEnd,
		Execution: executionRef(execution.Execution),
		ToolID:    call.ID,
		Tool:      call.Name,
		ToolLabel: label,
		Result:    result.Content,
		IsError:   result.IsError,
	})
	return result
}

// executeToolCallCore performs one real tool call without middleware or the
// start/end lifecycle events owned by executeSingleToolCall.
func executeToolCallCore(ctx context.Context, tool Tool, execution ToolExecution, config LoopConfig, failCount int, label string, sink eventSink) ToolResult {
	call := execution.Call
	if ctx.Err() != nil {
		return toolFailureResult(call, "Tool execution cancelled.", false)
	}
	if config.MaxToolErrors > 0 && failCount >= config.MaxToolErrors {
		return toolFailureResult(call,
			fmt.Sprintf("tool %q disabled after %d consecutive errors", call.Name, config.MaxToolErrors), false)
	}

	if tool == nil {
		return toolFailureResult(call, fmt.Sprintf("tool %q not found", call.Name), false)
	} else if err := validateToolArgs(tool, call); err != nil {
		return toolFailureResult(call, err.Error(), true)
	} else {
		// Stage 1: business-level input validation. Distinct from schema
		// validation above — Validators check semantics (write-before-read,
		// mtime drift, ...) using state the tool was constructed with.
		// Failures are surfaced as a normal tool_result with IsError=true so
		// the LLM can self-correct without prompting the user. Validators
		// MUST NOT prompt or mutate persistent state.
		if v, ok := tool.(Validator); ok {
			vr := v.Validate(ctx, call.Args)
			if !vr.OK {
				msg := vr.Message
				if msg == "" {
					msg = "tool input validation failed"
				}
				return toolFailureResult(call, msg, true)
			}
		}

		var preview json.RawMessage

		// Preview: if tool supports it, compute and emit preview before execution.
		// Preview runs only after args are validated so approval UIs can use it.
		if p, ok := tool.(Previewer); ok {
			data, err := p.Preview(ctx, call.Args)
			if err != nil {
				return toolFailureResult(call, fmt.Sprintf("preview tool %q: %v", call.Name, err), true)
			}
			preview = data
			sink.emit(Event{
				Type:       EventToolExecUpdate,
				Execution:  executionRef(execution.Execution),
				ToolID:     call.ID,
				Tool:       call.Name,
				ToolLabel:  label,
				Args:       call.Args,
				Result:     data,
				UpdateKind: ToolExecUpdatePreview,
			})
		}

		if config.ToolGate != nil {
			gateReq := GateRequest{
				Tool:      tool,
				Call:      call,
				ToolLabel: label,
				Preview:   preview,
			}
			decision, err := config.ToolGate(ctx, gateReq)
			if err != nil {
				decision = &GateDecision{Allowed: false, Reason: err.Error()}
			}
			if decision != nil && !decision.Allowed {
				reason := decision.Reason
				if reason == "" {
					reason = "tool execution denied"
				}
				return toolFailureResult(call, reason, false)
			}
			// Adopt the gate's rewrite before execution so the tool, progress
			// events, and middleware all see the approved arguments. The
			// assistant message keeps the model's original args — like the
			// exec-start event above, it records what was requested.
			if decision != nil && len(decision.UpdatedArgs) > 0 {
				call.Args = decision.UpdatedArgs
			}
		}

		// Inject progress callback so tools can report partial results
		progressCtx := WithToolProgress(ctx, func(progress ProgressPayload) {
			p := progress
			sink.emit(Event{
				Type:       EventToolExecUpdate,
				Execution:  executionRef(execution.Execution),
				ToolID:     call.ID,
				Tool:       call.Name,
				ToolLabel:  label,
				Args:       call.Args,
				Progress:   &p,
				UpdateKind: ToolExecUpdateProgress,
			})
		})

		var result ToolResult
		// ContentTool returns rich content blocks such as images.
		if ct, ok := tool.(ContentTool); ok {
			blocks, output, execErr := executeContentTool(progressCtx, ct, call)
			if execErr != nil {
				errContent, _ := json.Marshal(execErr.Error())
				result = ToolResult{
					ToolCallID: call.ID,
					Content:    errContent,
					IsError:    true,
				}
			} else {
				// When tool_reference blocks are present, keep them alongside
				// any text blocks in ContentBlocks. The provider layer splits
				// tool_reference into tool_result content and text into sibling
				// blocks within the same user message.
				refBlocks, siblingText := splitToolRefBlocks(blocks)
				if len(refBlocks) > 0 {
					resultBlocks := make([]ContentBlock, 0, len(refBlocks)+1)
					resultBlocks = append(resultBlocks, refBlocks...)
					if siblingText != "" {
						resultBlocks = append(resultBlocks, TextBlock(siblingText))
					}
					result = ToolResult{
						ToolCallID:    call.ID,
						Content:       pickContentSummary(output, blocks),
						ContentBlocks: resultBlocks,
					}
				} else {
					summary := pickContentSummary(output, blocks)
					result = ToolResult{
						ToolCallID:    call.ID,
						Content:       summary,
						ContentBlocks: blocks,
					}
				}
			}
		} else {
			output, execErr := tool.Execute(progressCtx, call.Args)
			if execErr != nil {
				errContent, _ := json.Marshal(execErr.Error())
				result = ToolResult{
					ToolCallID: call.ID,
					Content:    errContent,
					IsError:    true,
				}
			} else {
				result = ToolResult{
					ToolCallID: call.ID,
					Content:    output,
				}
			}
		}
		result.ToolName = call.Name
		return result
	}
}

func toolFailureResult(call ToolCall, msg string, countErr bool) ToolResult {
	content, _ := json.Marshal(msg)
	result := ToolResult{ToolCallID: call.ID, Content: content, IsError: true}
	if countErr {
		result.ToolName = call.Name
	}
	return result
}

// failToolCall emits the terminal tool_exec_end event and returns the error
// result for a call that failed before the tool ran. countErr sets ToolName
// on the result so the caller counts the failure toward toolErrors; denials,
// skips, and cancellations pass false (see executeSingleToolCall).
func failToolCall(sink eventSink, execution ToolExecution, label, msg string, countErr bool) ToolResult {
	call := execution.Call
	result := toolFailureResult(call, msg, countErr)
	sink.emit(Event{
		Type:      EventToolExecEnd,
		Execution: executionRef(execution.Execution),
		ToolID:    call.ID,
		Tool:      call.Name,
		ToolLabel: label,
		Result:    result.Content,
		IsError:   true,
	})
	return result
}

// skipToolCall creates a skipped result for an interrupted tool call.
func skipToolCall(execution ToolExecution, tools []Tool, sink eventSink) ToolResult {
	return skipToolCallWithMessage(execution, tools, sink, "Skipped due to queued user message.")
}

func skipToolCallWithMessage(execution ToolExecution, tools []Tool, sink eventSink, message string) ToolResult {
	call := execution.Call
	label := toolLabel(findTool(tools, call.Name))

	sink.emit(Event{
		Type:      EventToolExecStart,
		Execution: executionRef(execution.Execution),
		ToolID:    call.ID,
		Tool:      call.Name,
		ToolLabel: label,
		Args:      call.Args,
	})

	return failToolCall(sink, execution, label, message, false)
}

func newToolExecution(turnIndex int, call ToolCall) ToolExecution {
	return ToolExecution{
		Execution: Execution{
			ID:        call.ID,
			Kind:      ExecutionKindTool,
			TurnIndex: turnIndex,
			Attempt:   1,
		},
		Call: call,
	}
}

func findToolCall(calls []ToolCall, id string) ToolCall {
	for _, call := range calls {
		if call.ID == id {
			return call
		}
	}
	return ToolCall{ID: id}
}

func toolResultMessage(config LoopConfig, call ToolCall, result ToolResult) AgentMessage {
	if config.ToolResultMessageFactory != nil {
		if message := config.ToolResultMessageFactory(call, result); message != nil {
			return message
		}
	}
	return toolResultToMessage(result)
}

// toolResultToMessage converts a ToolResult into the default Message stored in
// context when the application has not installed a richer message factory.
func toolResultToMessage(tr ToolResult) Message {
	if len(tr.ContentBlocks) > 0 {
		return Message{
			Role:    RoleTool,
			Content: tr.ContentBlocks,
			Metadata: map[string]any{
				"tool_call_id": tr.ToolCallID,
				"tool_name":    tr.ToolName,
				"is_error":     tr.IsError,
			},
			Timestamp: time.Now(),
		}
	}
	msg := ToolResultMsg(tr.ToolCallID, tr.Content, tr.IsError)
	if tr.ToolName != "" {
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]any)
		}
		msg.Metadata["tool_name"] = tr.ToolName
	}
	return msg
}

// stripToolCallBlocks removes ContentToolCall blocks from a content slice.
// Used when the model's output was truncated (StopReasonLength), where tool
// call arguments are likely incomplete / malformed JSON.
func stripToolCallBlocks(blocks []ContentBlock) []ContentBlock {
	filtered := blocks[:0:0]
	for _, b := range blocks {
		if b.Type != ContentToolCall {
			filtered = append(filtered, b)
		}
	}
	return filtered
}

// toolLabel returns the human-readable label for a tool.
func toolLabel(tool Tool) string {
	if tool == nil {
		return ""
	}
	if labeler, ok := tool.(ToolLabeler); ok {
		return labeler.Label()
	}
	return ""
}

// buildToolSpecs converts Tool interfaces to ToolSpec for the LLM.
// When a DeferFilter is present among the tools, unactivated deferred tools
// are excluded and activated deferred tools are sent with DeferLoading: true.
func buildToolSpecs(tools []Tool) []ToolSpec {
	if len(tools) == 0 {
		return nil
	}

	var filter DeferFilter
	for _, tool := range tools {
		if candidate, ok := tool.(DeferFilter); ok {
			filter = candidate
			break
		}
	}

	specs := make([]ToolSpec, 0, len(tools))
	for _, t := range tools {
		spec := ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Schema(),
		}
		if filter != nil && filter.IsDeferred(t.Name()) {
			continue // unactivated deferred → don't send schema
		}
		if filter != nil && filter.WasDeferred(t.Name()) {
			spec.DeferLoading = true // activated deferred → send with defer_loading
		}
		if s, ok := t.(StrictSchemaTool); ok {
			strict := s.StrictSchema()
			spec.Strict = &strict
		}
		specs = append(specs, spec)
	}
	return specs
}

// buildToolMiddlewareChain wraps a complete tool call with the middleware stack.
// Outermost middleware is called first; innermost calls the actual tool.
func buildToolMiddlewareChain(exec ToolExecuteFunc, middlewares []ToolMiddleware) ToolExecuteFunc {
	for i := len(middlewares) - 1; i >= 0; i-- {
		mw := middlewares[i]
		next := exec
		exec = func(ctx context.Context, execution ToolExecution) (ToolResult, error) {
			return mw(ctx, execution, next)
		}
	}
	return exec
}

func executeContentTool(ctx context.Context, ct ContentTool, call ToolCall) ([]ContentBlock, json.RawMessage, error) {
	blocks, err := ct.ExecuteContent(ctx, call.Args)
	return blocks, contentBlocksTextSummary(blocks), err
}

func pickContentSummary(output json.RawMessage, blocks []ContentBlock) json.RawMessage {
	if len(output) > 0 {
		return output
	}
	return contentBlocksTextSummary(blocks)
}

func findTool(tools []Tool, name string) Tool {
	for _, t := range tools {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

func copyMessages(msgs []AgentMessage) []AgentMessage {
	out := make([]AgentMessage, len(msgs))
	copy(out, msgs)
	return out
}

// splitToolRefBlocks separates tool_reference blocks from text blocks.
// Returns the tool_reference blocks and concatenated text from text blocks.
// Used to format tool search results: tool_reference goes into tool_result
// content, text becomes a sibling block outside the tool_result.
func splitToolRefBlocks(blocks []ContentBlock) (refs []ContentBlock, text string) {
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case ContentToolRef:
			refs = append(refs, b)
		case ContentText:
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
	}
	text = strings.Join(texts, "\n")
	return
}

// contentBlocksTextSummary extracts text from ContentBlocks as a JSON string
// for the Event.Result field. Returns nil if no text content.
func contentBlocksTextSummary(blocks []ContentBlock) json.RawMessage {
	var texts []string
	for _, b := range blocks {
		if b.Type == ContentText {
			texts = append(texts, b.Text)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	summary, _ := json.Marshal(strings.Join(texts, "\n"))
	return summary
}
