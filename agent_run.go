package agentgo

import (
	"context"
	"errors"
	"fmt"
)

// buildConfig constructs a LoopConfig from the agent's settings. Must be called with lock held.
func (a *Agent) buildConfig(continuing bool) LoopConfig {
	skipInitialSteering := a.skipNextInitialSteeringPoll
	a.skipNextInitialSteeringPoll = false
	initialState := a.stateLocked()
	initialState.Progress = activateRunProgress(initialState.Progress, continuing)

	return LoopConfig{
		Model:                    a.model,
		MaxTurns:                 a.maxTurns,
		MaxRetries:               a.maxRetries,
		MaxToolErrors:            a.maxToolErrors,
		ThinkingLevel:            a.thinkingLevel,
		InitialState:             initialState,
		ContextManager:           a.contextManager,
		ToolResultMessageFactory: a.toolResultFactory,
		CommitContext: func(msgs []AgentMessage, usage *ContextUsage) error {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.messages = copyMessages(msgs)
			a.syncContextManagerLocked()
			return nil
		},
		CommitMessage: a.messageCommitter,
		ToolGate:      a.toolGate,
		GetSteeringMessages: func() []AgentMessage {
			a.mu.Lock()
			defer a.mu.Unlock()
			if skipInitialSteering {
				skipInitialSteering = false
				return nil
			}
			return dequeue(&a.steeringQ)
		},
		GetFollowUpMessages: func() []AgentMessage {
			a.mu.Lock()
			defer a.mu.Unlock()
			return dequeue(&a.followUpQ)
		},
		ModelMiddlewares:      a.modelMiddlewares,
		ToolMiddlewares:       a.toolMiddlewares,
		MaxToolConcurrency:    a.maxToolConcurrency,
		ShouldEmitAbortMarker: a.wantAbortMarker.Load,
		OnMessage:             a.onMessage,
		BeforeTurn:            a.beforeTurn,
		AfterTurn:             a.afterTurn,
		StopGuard:             a.stopGuard,
		LengthRecoveryPrompt:  a.lengthRecoveryPrompt,
		AbortMarkerText:       a.abortMarkerText,
		AbortMarkerToolText:   a.abortMarkerToolText,
		CacheLastMessage:      a.cacheLastMessage,
		PromptCacheKey:        a.promptCacheKey,
	}
}

// consumeLoop projects loop events into the stateful Agent view. It must not
// create AgentMessages: new entries come only from committed EventMessageEnd
// events, while Loop-owned state snapshots may replace the projected history.
func (a *Agent) consumeLoop(runCtx context.Context, kind RunKind, events <-chan Event) {
	// Capture this run's done channel and cancel up front. A new run can take
	// over before this loop's defer runs — an auto-continue may start it from
	// this run's EventAgentEnd listener — reassigning a.done/a.cancel to that
	// run. Holding our own copies keeps the defer from touching the newer run.
	a.mu.Lock()
	myDone := a.done
	myCancel := a.cancel
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()

		// Release this run's derived ctx now that the loop has drained — chiefly
		// the propagation goroutine context starts when the caller passes a
		// cancellable parent. myCancel is this run's own, so this never touches a
		// newer run's ctx.
		if myCancel != nil {
			myCancel()
		}

		// Reset shared run-state only if no newer run has taken over. An
		// auto-continue can start the next run from this run's EventAgentEnd
		// listener (which fires before this defer), reassigning a.done/a.cancel/
		// isRunning to that run — stomping them here would abort it.
		if a.done == myDone {
			a.isRunning = false
			a.streamMessage = nil
			a.pendingToolCalls = make(map[string]struct{})
			a.cancel = nil
			a.wantAbortMarker.Store(false)
		}
		a.mu.Unlock()
		close(myDone)
	}()

	for ev := range events {
		if ev.Type == EventAgentEnd {
			a.consumeAgentEnd(runCtx, kind, ev)
			continue
		}
		a.mu.Lock()
		switch ev.Type {
		case EventAgentStart:
			if ev.State != nil {
				a.applyLoopStateLocked(*ev.State)
			}

		// Message lifecycle
		case EventMessageStart:
			a.streamMessage = ev.Message

		case EventMessageUpdate:
			a.streamMessage = ev.Message

		case EventMessageEnd:
			a.streamMessage = nil
			if ev.Message != nil {
				a.messages = append(a.messages, ev.Message)
				a.syncContextManagerLocked()
				// Accumulate usage from assistant messages
				if msg, ok := ev.Message.(Message); ok && msg.Usage != nil {
					a.totalUsage.Add(msg.Usage)
				}
			}

		// Tool execution lifecycle
		case EventToolExecStart:
			if ev.ToolID != "" {
				a.pendingToolCalls[ev.ToolID] = struct{}{}
			}

		case EventToolExecEnd:
			delete(a.pendingToolCalls, ev.ToolID)

		// Turn end
		case EventModelResponse:
			if msg, ok := ev.Message.(Message); ok {
				if errStr, _ := msg.Metadata["error_message"].(string); errStr != "" {
					a.lastError = errStr
				}
			}

		case EventTurnEnd:
			if ev.State != nil {
				a.applyLoopStateLocked(*ev.State)
			}

		// Execution errors are runtime facts, not conversation messages. A model
		// may still return a terminal Message with StopReasonError; the Loop
		// commits that through EventMessageEnd like any other model outcome.
		case EventError:
			// Clear the externally-visible streaming view: listeners may
			// call State() from this EventError callback, and a stale
			// streamMessage would surface a never-completing partial that the
			// agent has already abandoned. Cleared here, before listeners run
			// (notifications fire after the lock is released below).
			a.streamMessage = nil
			if ev.Err != nil && !errors.Is(ev.Err, context.Canceled) {
				a.lastError = ev.Err.Error()
			}

		}

		// Copy listeners to avoid holding lock during callback
		listeners := make([]func(Event), len(a.listeners))
		copy(listeners, a.listeners)
		a.mu.Unlock()

		for _, fn := range listeners {
			if fn != nil {
				fn(ev)
			}
		}
	}
}

// consumeAgentEnd crosses from the AgentLoop terminal event back into the
// stateful Agent boundary. AfterRun deliberately executes here, outside the
// Loop goroutine, after final state projection and before terminal listeners
// may start another run.
func (a *Agent) consumeAgentEnd(runCtx context.Context, kind RunKind, ev Event) {
	a.runMu.Lock()

	a.mu.Lock()
	if ev.State != nil {
		a.applyLoopStateLocked(*ev.State)
	}
	a.isRunning = false
	a.streamMessage = nil
	a.pendingToolCalls = make(map[string]struct{})
	hook := a.afterRun
	snapshot := a.snapshotLocked()
	a.mu.Unlock()

	var hookErr error
	if hook != nil {
		summary := RunSummary{EndReason: EndReasonError}
		if ev.Summary != nil {
			summary = *ev.Summary
		}
		if err := callAfterRun(context.WithoutCancel(runCtx), hook, AfterRunContext{
			Kind:     kind,
			Snapshot: snapshot,
			Summary:  summary,
			Err:      ev.Err,
		}); err != nil {
			hookErr = fmt.Errorf("after run: %w", err)
			ev.Err = errors.Join(ev.Err, hookErr)
			summary.EndReason = EndReasonError
			ev.Summary = &summary
		}
	}

	a.mu.Lock()
	if hookErr != nil {
		a.lastError = ev.Err.Error()
		finalState := a.stateLocked()
		ev.State = &finalState
	}
	listeners := make([]func(Event), len(a.listeners))
	copy(listeners, a.listeners)
	a.mu.Unlock()
	a.runMu.Unlock()

	if hookErr != nil {
		notifyListeners(listeners, Event{Type: EventError, Err: hookErr})
	}
	notifyListeners(listeners, ev)
}

func callAfterRun(ctx context.Context, hook AfterRunHook, run AfterRunContext) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return hook(ctx, run)
}

func notifyListeners(listeners []func(Event), event Event) {
	for _, listener := range listeners {
		if listener != nil {
			listener(event)
		}
	}
}

func (a *Agent) applyLoopStateLocked(state AgentState) {
	a.messages = copyMessages(state.Messages)
	a.totalUsage = state.TotalUsage
	a.runProgress = cloneRunProgress(state.Progress)
	a.syncContextManagerLocked()
}

func (a *Agent) syncContextManagerLocked() {
	if a.contextManager == nil {
		return
	}
	a.contextManager.Sync(copyMessages(a.messages))
}
