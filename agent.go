package agentgo

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// Agent is a stateful wrapper around the agent loop.
// It consumes loop events to update internal state, just like any external listener.
type Agent struct {
	// Configuration (set via options)
	model                ChatModel
	systemPrompt         string
	systemBlocks         []SystemBlock
	tools                []Tool
	maxTurns             int
	maxRetries           int
	maxToolErrors        int
	thinkingLevel        ThinkingLevel
	contextManager       ContextManager
	contextWindow        int
	contextEstimateFn    ContextEstimateFn
	toolResultFactory    func(ToolCall, ToolResult) AgentMessage
	toolGate             ToolGate
	modelMiddlewares     []ModelMiddleware
	toolMiddlewares      []ToolMiddleware
	maxToolConcurrency   int
	lengthRecoveryPrompt string
	abortMarkerText      string
	abortMarkerToolText  string
	messageCommitter     func(AgentMessage) error
	onMessage            func(AgentMessage)
	beforeTurn           BeforeTurnHook
	afterTurn            AfterTurnHook
	beforeRun            BeforeRunHook
	afterRun             AfterRunHook
	stopGuard            StopGuard
	cacheLastMessage     string
	promptCacheKey       string

	// State
	messages         []AgentMessage
	isRunning        bool
	lastError        string
	streamMessage    AgentMessage        // partial message during streaming
	pendingToolCalls map[string]struct{} // tool call IDs in flight
	totalUsage       Usage               // cumulative token usage
	runProgress      RunProgress

	// Queues
	steeringQ                   []AgentMessage
	followUpQ                   []AgentMessage
	skipNextInitialSteeringPoll bool

	// Lifecycle
	listeners       []func(Event)
	cancel          context.CancelFunc
	done            chan struct{} // closed when loop finishes
	held            int           // active HoldRuns count; >0 rejects new run starts
	wantAbortMarker atomic.Bool   // set by Abort(), read by runLoop
	runMu           sync.Mutex    // serializes run preparation and state replacement
	mu              sync.Mutex
}

// NewAgent creates a new Agent with the given options.
//
// When a ContextManager is set, the agent auto-wires the context-token
// estimator and context window when the manager implements the optional
// ContextEstimator / ContextWindowProvider interfaces.
func NewAgent(opts ...AgentOption) *Agent {
	a := &Agent{
		maxTurns:         defaultMaxTurns,
		maxRetries:       defaultMaxRetries,
		pendingToolCalls: make(map[string]struct{}),
	}
	for _, opt := range opts {
		opt(a)
	}
	if a.contextManager != nil {
		if e, ok := a.contextManager.(ContextEstimator); ok {
			a.contextEstimateFn = e.EstimateContext
		}
		if w, ok := a.contextManager.(ContextWindowProvider); ok {
			a.contextWindow = w.ContextWindow()
		}
	}
	return a
}

// Subscribe registers a listener for agent events. Returns an unsubscribe function.
//
// Dispatch contract (stable API guarantee): listeners are invoked
// synchronously, in registration order, on the agent's event-consumption
// goroutine. A listener registered before another always observes each event
// first — ordering-sensitive consumers (e.g. a budget sentinel that must veto
// before a dispatcher reacts) may rely on this. The flip side: a slow listener
// delays event delivery to all later listeners and backpressures the loop's
// event channel; offload heavy work to your own goroutine.
//
// Lifecycle contract (stable API guarantee):
//   - When EventAgentEnd listeners run, AfterRun has completed and isRunning
//     is already false — a listener may start the next run (Continue /
//     InjectContext) directly.
//   - The run's done channel — what WaitForIdle and HoldRuns wait on —
//     closes only after all listeners for the final event have returned.
//   - Therefore a listener must never call HoldRuns or Reset, and must never
//     block on a lock that may be held by a goroutine waiting on
//     WaitForIdle/HoldRuns: both are self-deadlocks.
func (a *Agent) Subscribe(fn func(Event)) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listeners = append(a.listeners, fn)
	idx := len(a.listeners) - 1
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.listeners[idx] = nil
	}
}

// Prompt starts a new conversation turn with the given input. The ctx scopes
// the run: its deadline, trace values, and cancellation propagate into the
// loop, and cancelling it aborts the run just like Abort.
func (a *Agent) Prompt(ctx context.Context, input string) error {
	return a.PromptMessages(ctx, UserMsg(input))
}

// PromptMessages starts a new conversation turn with arbitrary AgentMessages.
// See Prompt for ctx semantics.
func (a *Agent) PromptMessages(ctx context.Context, msgs ...AgentMessage) error {
	a.runMu.Lock()
	defer a.runMu.Unlock()

	a.mu.Lock()
	// Checked before isRunning: during a hold's wind-down isRunning is still
	// true, and the caller must see the stable ErrRunsHeld, not a
	// timing-dependent ErrAlreadyRunning.
	if a.held > 0 {
		a.mu.Unlock()
		return ErrRunsHeld
	}
	if a.isRunning {
		a.mu.Unlock()
		return fmt.Errorf("%w; use Steer() or FollowUp() to queue messages", ErrAlreadyRunning)
	}
	a.mu.Unlock()
	if err := a.prepareRun(ctx, RunKindPrompt, msgs); err != nil {
		return err
	}

	a.mu.Lock()
	if a.held > 0 {
		a.mu.Unlock()
		return ErrRunsHeld
	}
	if a.isRunning {
		a.mu.Unlock()
		return fmt.Errorf("%w; use Steer() or FollowUp() to queue messages", ErrAlreadyRunning)
	}
	a.startPromptRunLocked(ctx, msgs, RunKindPrompt)
	return nil
}

// Continue resumes from the current context without adding new messages.
// If the last message is from assistant, Continue first resumes any next turn
// already recorded in state; otherwise it dequeues steering/follow-up messages
// (steering first) and replays them as the new prompt.
//
// Queue retention caveat: messages queued via Steer/FollowUp survive an
// aborted run — Abort cancels execution but never consumes or clears the
// queues, so the next Continue (including any automatic resume the harness
// performs) replays them. If a queued directive must not outlive the run it
// was meant for, clear it on your abort path via ClearSteeringQueue /
// ClearFollowUpQueue / ClearAllQueues; agentgo cannot decide this — some
// harnesses queue droppable steering text, others queue task-completion
// notifications that must never be lost.
func (a *Agent) Continue(ctx context.Context) error {
	a.runMu.Lock()
	defer a.runMu.Unlock()

	a.mu.Lock()
	// Before isRunning (stable error during wind-down) and before any dequeue
	// (a held Continue must not consume queued messages just to drop them).
	if a.held > 0 {
		a.mu.Unlock()
		return ErrRunsHeld
	}
	if a.isRunning {
		a.mu.Unlock()
		return ErrAlreadyRunning
	}
	a.mu.Unlock()
	if err := a.prepareRun(ctx, RunKindContinue, nil); err != nil {
		return err
	}

	a.mu.Lock()
	if a.held > 0 {
		a.mu.Unlock()
		return ErrRunsHeld
	}
	if a.isRunning {
		a.mu.Unlock()
		return ErrAlreadyRunning
	}
	if len(a.messages) == 0 {
		a.mu.Unlock()
		return ErrNoMessages
	}

	// An assistant-tail context is valid when a saved turn already decided to
	// continue. Otherwise, accepted queue input can start a new prompt run.
	lastMsg := a.messages[len(a.messages)-1]
	if lastMsg.GetRole() == RoleAssistant {
		if a.runProgress.NextTurn || len(a.runProgress.PendingMessages) > 0 {
			a.startContinueRunLocked(ctx, RunKindContinue)
			return nil
		}
		if a.resumeQueuedLocked(ctx, RunKindContinue) {
			return nil
		}
		a.mu.Unlock()
		return ErrBadContinuation
	}

	a.startContinueRunLocked(ctx, RunKindContinue)
	return nil
}

// resumeQueuedLocked dequeues pending messages (steering first, then
// follow-up) and starts a prompt run with them. Caller must hold a.mu with an
// assistant-tail idle agent; on true the run has started and the lock has been
// released (by startPromptRunLocked), on false nothing was dequeued and the
// lock is still held.
func (a *Agent) resumeQueuedLocked(ctx context.Context, kind RunKind) bool {
	queued := dequeue(&a.steeringQ)
	if len(queued) == 0 {
		queued = dequeue(&a.followUpQ)
	}
	if len(queued) == 0 {
		return false
	}
	a.skipNextInitialSteeringPoll = true
	a.startPromptRunLocked(ctx, queued, kind)
	return true
}

// Steer queues a steering message to interrupt the agent mid-run.
// Delivered after the current tool execution; remaining tools are skipped.
func (a *Agent) Steer(msg AgentMessage) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQ = append(a.steeringQ, msg)
}

// FollowUp queues a message for the next natural continuation point. An
// active run consumes follow-ups when it reaches that point; an idle Agent
// retains them until the harness calls Continue.
func (a *Agent) FollowUp(msg AgentMessage) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQ = append(a.followUpQ, msg)
}

// Abort cancels the current execution and emits an abort marker message
// so the LLM knows the user interrupted.
//
// Abort does not touch the steering/follow-up queues: messages queued before
// the cancellation stay queued and are replayed by the next run (see
// Continue). Harnesses that treat queued input as stale after an abort must
// clear it explicitly.
func (a *Agent) Abort() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel == nil {
		// Idle: nothing to interrupt, and no run will consume the marker flag —
		// arming it here would leak onto the next cancellation (e.g. a later
		// HoldRuns drain, which must stay silent).
		return
	}
	a.wantAbortMarker.Store(true)
	a.cancel()
}

// AbortSilent cancels the current execution without emitting an abort marker.
// Use for programmatic cancellation (e.g. plan mode transitions) where the
// cancellation is not a user interruption.
func (a *Agent) AbortSilent() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
}

// HoldRuns stops the world for state surgery: it silently cancels any
// in-flight run, waits for it to drain, and rejects new run starts
// (PromptMessages / Continue / InjectContext return ErrRunsHeld) until the
// returned release is called. Use it to make multi-step mutations — swap the
// conversation, clear history, retarget stores — atomic with respect to the
// run lifecycle, including auto-continues a listener may attempt mid-surgery.
//
//	release := agent.HoldRuns()
//	defer release()
//	// no run is in flight and none can start
//
// Semantics:
//   - Counting: concurrent holders each get their own release (idempotent);
//     runs stay rejected until every release has been called. The hold only
//     freezes the run lifecycle — it does NOT mutually exclude two holders'
//     state mutations; serialize holders on the host side.
//   - The cancel is silent: HoldRuns itself never requests an abort marker.
//     (A concurrent Abort can still mark the dying run — the hold does not
//     suppress user interruptions.)
//   - Queues are untouched: like Abort, a hold never consumes or clears
//     Steer/FollowUp messages — clear them explicitly if they must not
//     outlive the held run (see Continue's queue-retention caveat).
//   - WaitForIdle after HoldRuns returns immediately: "no run in flight" is
//     guaranteed, but that does not mean starts are allowed again.
//   - MUST NOT be called from an event listener: the drained run's done
//     channel closes only after its listeners return, so a listener waiting
//     on HoldRuns deadlocks itself. (The same restriction applies to Reset.)
func (a *Agent) HoldRuns() (release func()) {
	a.mu.Lock()
	a.held++
	// Snapshotted in the same critical section as held++: a takeover run
	// cannot start after this point (its Continue fails ErrRunsHeld), so the
	// snapshot cannot go stale.
	cancel, done := a.cancel, a.done
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		// done closes after the dying run's listeners return; listeners never
		// block on a hold (their run starts fail fast), so this cannot hang.
		<-done
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.held--
			a.mu.Unlock()
		})
	}
}

// WaitForIdle blocks until the agent finishes the current run.
func (a *Agent) WaitForIdle() {
	a.mu.Lock()
	done := a.done
	a.mu.Unlock()
	if done != nil {
		<-done
	}
}

// State returns a snapshot of the agent's current state.
func (a *Agent) State() AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stateLocked()
}

func (a *Agent) stateLocked() AgentState {
	pending := make(map[string]struct{}, len(a.pendingToolCalls))
	for k, v := range a.pendingToolCalls {
		pending[k] = v
	}
	sp := a.systemPrompt
	if len(a.systemBlocks) > 0 && sp == "" {
		var sb strings.Builder
		for i, b := range a.systemBlocks {
			if i > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(b.Text)
		}
		sp = sb.String()
	}
	return AgentState{
		SystemPrompt:     sp,
		Messages:         copyMessages(a.messages),
		Tools:            a.tools,
		IsRunning:        a.isRunning,
		StreamMessage:    a.streamMessage,
		PendingToolCalls: pending,
		TotalUsage:       a.totalUsage,
		Progress:         cloneRunProgress(a.runProgress),
		Error:            a.lastError,
	}
}

// Messages returns the current message history.
func (a *Agent) Messages() []AgentMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return copyMessages(a.messages)
}

// SetMessageCommitter replaces the durable message callback used by subsequent
// runs. Returning an error stops the run before the message enters context.
func (a *Agent) SetMessageCommitter(fn func(AgentMessage) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messageCommitter = fn
}

// SetMessages replaces the message history — restore a previous conversation,
// or clear it with nil. Refused while a run is in flight (ErrAlreadyRunning):
// the loop's context commit would resurrect the replaced history as silent
// corruption. Hold the lifecycle first (HoldRuns) when mutating around live
// runs.
func (a *Agent) SetMessages(msgs []AgentMessage) error {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isRunning {
		return fmt.Errorf("cannot set messages: %w", ErrAlreadyRunning)
	}
	a.messages = copyMessages(msgs)
	a.syncContextManagerLocked()
	return nil
}

// ExportMessages returns only concrete model Messages. Use Messages with an
// application codec when custom AgentMessage values must survive persistence.
func (a *Agent) ExportMessages() []Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return CollectMessages(a.messages)
}

// ImportMessages replaces message history from deserialized Messages.
func (a *Agent) ImportMessages(msgs []Message) error {
	return a.SetMessages(ToAgentMessages(msgs))
}

// startPromptRunLocked starts a prompt-based run. Caller must hold a.mu.
// The run ctx derives from the caller's ctx, so an external cancel and Abort
// share standard OR semantics.
func (a *Agent) startPromptRunLocked(ctx context.Context, msgs []AgentMessage, kind RunKind) {
	a.isRunning = true
	a.lastError = ""

	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.done = make(chan struct{})

	agentCtx := AgentContext{
		SystemPrompt: a.systemPrompt,
		SystemBlocks: a.systemBlocks,
		Messages:     copyMessages(a.messages),
		Tools:        a.tools,
	}
	config := a.buildConfig(false)
	a.mu.Unlock()

	go a.consumeLoop(runCtx, kind, AgentLoop(runCtx, msgs, agentCtx, config))
}

// startContinueRunLocked starts a continue run from the current context. Caller must hold a.mu.
// See startPromptRunLocked for run-ctx semantics.
func (a *Agent) startContinueRunLocked(ctx context.Context, kind RunKind) {
	a.isRunning = true
	a.lastError = ""

	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.done = make(chan struct{})

	agentCtx := AgentContext{
		SystemPrompt: a.systemPrompt,
		SystemBlocks: a.systemBlocks,
		Messages:     copyMessages(a.messages),
		Tools:        a.tools,
	}
	config := a.buildConfig(true)
	a.mu.Unlock()

	go a.consumeLoop(runCtx, kind, AgentLoopContinue(runCtx, agentCtx, config))
}

// ContextUsage returns an estimate of the current context window occupancy.
// Returns nil if contextWindow or contextEstimateFn is not configured.
func (a *Agent) ContextUsage() *ContextUsage {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.contextManager != nil {
		if usage := a.contextManager.Usage(); usage != nil {
			cp := *usage
			return &cp
		}
	}

	if a.contextWindow <= 0 || a.contextEstimateFn == nil {
		return nil
	}

	tokens, usageTokens, trailingTokens := a.contextEstimateFn(a.messages)
	pct := float64(tokens) / float64(a.contextWindow) * 100

	return &ContextUsage{
		Tokens:         tokens,
		ContextWindow:  a.contextWindow,
		Percent:        pct,
		UsageTokens:    usageTokens,
		TrailingTokens: trailingTokens,
	}
}

// BaselineContextUsage returns the current runtime baseline occupancy.
// Unlike ContextUsage, this never reports a transient projected view.
func (a *Agent) BaselineContextUsage() *ContextUsage {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.contextManager != nil {
		if snap := a.contextManager.Snapshot(); snap != nil && snap.BaselineUsage != nil {
			cp := *snap.BaselineUsage
			return &cp
		}
	}

	if a.contextWindow <= 0 || a.contextEstimateFn == nil {
		return nil
	}

	tokens, usageTokens, trailingTokens := a.contextEstimateFn(a.messages)
	pct := float64(tokens) / float64(a.contextWindow) * 100

	return &ContextUsage{
		Tokens:         tokens,
		ContextWindow:  a.contextWindow,
		Percent:        pct,
		UsageTokens:    usageTokens,
		TrailingTokens: trailingTokens,
	}
}

// ContextSnapshot returns the latest context-manager snapshot for observability.
// Returns nil when no ContextManager is configured or no snapshot is available.
func (a *Agent) ContextSnapshot() *ContextSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.contextManager == nil {
		return nil
	}
	snap := a.contextManager.Snapshot()
	if snap == nil {
		return nil
	}

	out := *snap
	if snap.BaselineUsage != nil {
		usage := *snap.BaselineUsage
		out.BaselineUsage = &usage
	}
	if snap.Usage != nil {
		usage := *snap.Usage
		out.Usage = &usage
	}
	return &out
}

// TotalUsage returns the cumulative token usage across all turns.
func (a *Agent) TotalUsage() Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.totalUsage
}

// SetModel changes the LLM provider. Takes effect on the next turn.
//
// Purity contract (stable API guarantee, shared by SetThinkingLevel, SetTools,
// SetSystemPrompt and SetSystemBlocks): a pure field assignment under the
// agent's internal mutex — no events emitted, no callbacks invoked — so it is
// safe to call from event listeners and while holding host locks.
// SetContextWindow is NOT part of this contract: it calls back into the
// ContextManager outside the lock.
func (a *Agent) SetModel(m ChatModel) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = m
}

// SetContextWindow updates the context window size (in tokens). The new value
// is also pushed to the ContextManager when it implements ContextWindowSetter,
// so a model hot-switch needs exactly one call to keep agent-side usage
// reporting and engine-side compaction thresholds in agreement.
func (a *Agent) SetContextWindow(n int) {
	a.mu.Lock()
	a.contextWindow = n
	cm := a.contextManager
	a.mu.Unlock()
	// Propagate outside the agent lock: the manager has its own mutex and
	// may be queried concurrently by a running loop.
	if s, ok := cm.(ContextWindowSetter); ok {
		s.SetContextWindow(n)
	}
}

// SetSystemPrompt changes the system prompt (single-string mode).
// Clears any multi-block system prompt set via SetSystemBlocks.
// Purity contract: see SetModel.
func (a *Agent) SetSystemPrompt(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.systemPrompt = s
	a.systemBlocks = nil
}

// SetSystemBlocks sets a multi-block system prompt with per-block cache control.
// Takes precedence over SetSystemPrompt. Clears the single-string prompt.
// Purity contract: see SetModel.
func (a *Agent) SetSystemBlocks(blocks []SystemBlock) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.systemBlocks = blocks
	a.systemPrompt = ""
}

// BuildLLMMessages constructs the message list with the same system-blocks /
// converted-history layout the agent loop uses for its primary LLM call.
//
// Loop-scoped concerns are deliberately omitted: per-turn reminders are not
// appended, and no last-user cache_control marker is added. External callers
// (e.g., prompt suggestion) use this to share a stable prefix with the main
// conversation for prompt cache reads without writing new breakpoints of
// their own — the main loop's marker remains the sole writer.
//
// Malformed tool-call / result transcripts are repaired via RepairMessageSequence.
func (a *Agent) BuildLLMMessages() ([]Message, error) {
	a.mu.Lock()
	msgs := copyMessages(a.messages)
	blocks := a.systemBlocks
	sp := a.systemPrompt
	mgr := a.contextManager
	a.mu.Unlock()

	if mgr != nil {
		proj, err := mgr.Project(context.Background(), msgs)
		if err != nil {
			return nil, err
		}
		if proj.Messages != nil {
			msgs = proj.Messages
		}
	}

	llmMessages := RepairMessageSequence(ToMessages(msgs))

	if len(blocks) > 0 {
		sysMsgs := make([]Message, len(blocks))
		for i, b := range blocks {
			sysMsgs[i] = SystemMsg(b.Text)
			if b.CacheControl != "" {
				sysMsgs[i].Metadata = map[string]any{"cache_control": b.CacheControl}
			}
		}
		llmMessages = append(sysMsgs, llmMessages...)
	} else if sp != "" {
		llmMessages = append([]Message{SystemMsg(sp)}, llmMessages...)
	}
	return llmMessages, nil
}

// BuildLLMTools returns the ToolSpec list this agent would send to the LLM on
// its next turn — the exact same conversion buildToolSpecs runs inside the
// agent loop, including DeferFilter handling.
//
// Side-channel callers (the /btw side-question path, prompt suggestion) need
// this to keep their request's `tools` field byte-identical to the main
// agent's last request — Anthropic's prompt cache rejects any prefix
// difference, and `tools` precedes the system-block cache breakpoint. Without
// this method, those callers send `tools: nil` and miss the system cache.
func (a *Agent) BuildLLMTools() []ToolSpec {
	a.mu.Lock()
	tools := a.tools
	a.mu.Unlock()
	return buildToolSpecs(tools)
}

// SetTools replaces the tool set. Takes effect on the next turn.
// Purity contract: see SetModel.
func (a *Agent) SetTools(tools ...Tool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tools = tools
}

// SetThinkingLevel changes the reasoning depth. Takes effect on the next turn.
// Purity contract: see SetModel.
func (a *Agent) SetThinkingLevel(level ThinkingLevel) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.thinkingLevel = NormalizeThinkingLevel(level)
}

// SetPromptCacheKey changes the prompt-cache routing identity at runtime.
// Takes effect on the next turn. Hosts that reuse one Agent across logical
// conversations (session switch / reset) must update the key so a new
// conversation doesn't inherit the previous one's cache lineage. See
// WithPromptCacheKey for semantics.
func (a *Agent) SetPromptCacheKey(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.promptCacheKey = key
}

// ClearSteeringQueue removes all queued steering messages.
func (a *Agent) ClearSteeringQueue() {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQ = nil
}

// ClearFollowUpQueue removes all queued follow-up messages.
func (a *Agent) ClearFollowUpQueue() {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQ = nil
}

// ClearAllQueues removes all queued steering and follow-up messages.
func (a *Agent) ClearAllQueues() {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steeringQ = nil
	a.followUpQ = nil
}

// HasQueuedMessages reports whether any steering or follow-up messages are queued.
func (a *Agent) HasQueuedMessages() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.steeringQ) > 0 || len(a.followUpQ) > 0
}

// HasFollowUps reports whether follow-up messages are waiting to be consumed.
func (a *Agent) HasFollowUps() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.followUpQ) > 0
}

// Reset clears all state and queues. A run already in flight is silently
// cancelled, drained, and wiped with the rest of the state; a run start
// attempted after the drain begins fails with ErrRunsHeld instead of being
// silently clobbered. Must not be called from an event listener (see HoldRuns).
func (a *Agent) Reset() {
	release := a.HoldRuns()
	defer release()

	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = nil
	a.steeringQ = nil
	a.followUpQ = nil
	a.isRunning = false
	a.lastError = ""
	a.streamMessage = nil
	a.pendingToolCalls = make(map[string]struct{})
	a.totalUsage = Usage{}
	a.runProgress = RunProgress{}
	a.done = nil
	a.cancel = nil
	a.syncContextManagerLocked()
}
