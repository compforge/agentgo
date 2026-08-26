package agentgo

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// Agent Events
// ---------------------------------------------------------------------------

// EventType identifies agent lifecycle event types.
type EventType string

const (
	EventAgentStart EventType = "agent_start"
	EventAgentEnd   EventType = "agent_end"
	EventTurnStart  EventType = "turn_start"
	EventTurnEnd    EventType = "turn_end"
	// EventModelExecStart and EventModelExecEnd cover every AgentGo-owned model
	// attempt, including internal context summarization calls.
	EventModelExecStart EventType = "model_exec_start"
	EventModelExecEnd   EventType = "model_exec_end"
	// EventModelResponse fires after a conversation model call and any tool
	// executions it triggered. Internal model calls such as context summaries
	// use model_exec_start/model_exec_end without becoming conversation output.
	EventModelResponse  EventType = "model_response"
	EventMessageStart   EventType = "message_start"
	EventMessageUpdate  EventType = "message_update"
	EventMessageEnd     EventType = "message_end"
	EventToolExecStart  EventType = "tool_exec_start"
	EventToolExecUpdate EventType = "tool_exec_update"
	EventToolExecEnd    EventType = "tool_exec_end"
	// EventContextProjected fires before each model call with the identifiable
	// items exposed by the actual projected AgentMessage context. It is a trace
	// fact, not a judgment that any item was useful or sufficient.
	EventContextProjected EventType = "context_projected"
	// EventContextCompacted fires once after a ContextManager completes a
	// context compaction transaction and before the compacted view is sent to
	// the model. Internal compactor stages do not emit separate events.
	EventContextCompacted EventType = "context_compacted"
	EventRetry            EventType = "retry"
	EventError            EventType = "error"
)

// ToolExecUpdateKind distinguishes update payload semantics for tool_exec_update events.
type ToolExecUpdateKind string

const (
	ToolExecUpdatePreview  ToolExecUpdateKind = "preview"
	ToolExecUpdateProgress ToolExecUpdateKind = "progress"
)

// EndReason describes why a single agent run stopped.
type EndReason string

const (
	EndReasonStop     EndReason = "stop"
	EndReasonMaxTurns EndReason = "max_turns"
	EndReasonAborted  EndReason = "aborted"
	EndReasonError    EndReason = "error"
)

// RunSummary captures loop facts that are known at the end of a run.
// It intentionally excludes higher-level policy judgments.
type RunSummary struct {
	TurnCount  int
	ToolCalls  int
	ToolErrors int
	EndReason  EndReason
}

// DeltaKind identifies what kind of content a message_update delta carries.
type DeltaKind string

const (
	DeltaText     DeltaKind = ""         // default: regular text
	DeltaThinking DeltaKind = "thinking" // model reasoning/thinking
	DeltaToolCall DeltaKind = "toolcall" // tool call argument JSON
)

// Event is a lifecycle event emitted by the agent loop.
// This is the single output channel for all lifecycle information.
type Event struct {
	Type         EventType
	Execution    *Execution      // shared coordinate for model/tool/compaction execution facts
	Message      AgentMessage    // for message_start/update/end, turn_end
	Delta        string          // text delta for message_update
	DeltaKind    DeltaKind       // for message_update: what kind of delta
	ToolID       string          // for tool_exec_*
	Tool         string          // tool name for tool_exec_*
	ToolLabel    string          // human-readable tool label (from ToolLabeler)
	Args         json.RawMessage // tool args for tool_exec_start/tool_exec_update
	Result       json.RawMessage // tool result for tool_exec_end and preview updates
	Progress     *ProgressPayload
	UpdateKind   ToolExecUpdateKind
	IsError      bool // tool error flag for tool_exec_end
	Preview      json.RawMessage
	ToolResults  []ToolResult    // for turn_end: all tool results from this turn
	Err          error           // for error events
	NewMessages  []AgentMessage  // for agent_end: messages added during this loop
	RetryInfo    *RetryInfo      // for retry events
	ContextItems []ContextItem   // for context_projected
	Compaction   *CompactionInfo // for context_compacted
	Summary      *RunSummary     // for agent_end: factual run summary
	State        *AgentState     // for agent_start/turn_end/agent_end: loop state at that boundary
}

// RetryInfo carries retry context for EventRetry events.
type RetryInfo struct {
	MaxRetries int
	Delay      time.Duration
	Err        error
}

// ---------------------------------------------------------------------------
// Event Helpers
// ---------------------------------------------------------------------------

// eventSink delivers loop events bound to the run context. It is created
// once per run at the AgentLoop entry points, so every emission site shares
// the same lifetime regardless of narrower contexts (per-tool cancellation
// must not affect event delivery).
type eventSink struct {
	ctx      context.Context
	ch       chan<- Event
	terminal *terminalEvent
}

type terminalEvent struct {
	event *Event
	err   error
}

// emit sends an event to the channel, blocking when it is full — backpressure,
// never event loss, while the run is live. Once the run context is canceled
// delivery degrades to best-effort: a buffered/ready send still succeeds (a
// draining reader receives the terminal events), but when the channel stays
// full the event is dropped so an abandoned channel cannot leak the loop
// goroutine.
func (s eventSink) emit(ev Event) {
	if ev.Type == EventError && s.terminal != nil && ev.Err != nil {
		s.terminal.err = errors.Join(s.terminal.err, ev.Err)
	}
	if ev.Type == EventAgentEnd && s.terminal != nil {
		if ev.Err == nil {
			ev.Err = s.terminal.err
		}
		copy := ev
		s.terminal.event = &copy
		return
	}
	s.emitNow(ev)
}

func (s eventSink) emitNow(ev Event) {
	select {
	case s.ch <- ev:
	default:
		select {
		case s.ch <- ev:
		case <-s.ctx.Done():
		}
	}
}

func (s eventSink) flushTerminal() {
	if s.terminal == nil || s.terminal.event == nil {
		return
	}
	s.emitNow(*s.terminal.event)
}

// emitError sends an error event followed by agent_end.
func (s eventSink) emitError(err error, summary *RunSummary) {
	s.emit(Event{Type: EventError, Err: err})
	s.emit(Event{Type: EventAgentEnd, Err: err, Summary: summary})
}

// ---------------------------------------------------------------------------
// Message Sequence Repair
// ---------------------------------------------------------------------------

// dequeue drains all messages from the queue.
func dequeue(queue *[]AgentMessage) []AgentMessage {
	if len(*queue) == 0 {
		return nil
	}
	result := *queue
	*queue = nil
	return result
}
