package agentgo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"
)

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
