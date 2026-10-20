// Package session records one logical agent conversation across loop runs.
// It has no dependency on a review, timeline, UI, or global session registry.
package session

import (
	"encoding/json"
	"time"

	"github.com/compforge/agentgo"
)

const FormatVersion = 1

const (
	SessionStart = "session_start"
	RunStart     = "run_start"
	RunClose     = "run_close"
	Checkpoint   = "checkpoint"
)

// Ref identifies one physical call attempt within a session. Execution IDs are
// run-local; neither an execution ID nor a provider tool-call ID alone is a
// durable reference. A new run always receives a new ID, including after reopen.
//
// +spec=`Session, run, execution and attempt jointly identify recorded call facts`
// +link=component://docs/session.md
type Ref struct {
	SessionID   string `json:"session_id"`
	RunID       string `json:"run_id"`
	ExecutionID string `json:"execution_id"`
	Attempt     int    `json:"attempt"`
}

// Record is one JSONL envelope. (SessionID, Seq) identifies a record; Seq is
// append order, not causality or timestamp order. Event records use the native
// EventType string and EventData payload. Checkpoint data uses the supplied codec.
type Record struct {
	Type      string             `json:"type"`
	SessionID string             `json:"session_id"`
	RunID     string             `json:"run_id,omitempty"`
	Seq       uint64             `json:"seq"`
	Timestamp time.Time          `json:"timestamp"`
	Execution *agentgo.Execution `json:"execution,omitempty"`
	Data      json.RawMessage    `json:"data,omitempty"`
}

// EventData preserves diagnostic facts without serializing runtime handles or
// a full AgentState on every event. Message is a codec-encoded AgentMessage;
// ModelRequest is the final model-facing representation, not recovery state.
type EventData struct {
	ModelAttempt     *agentgo.ModelAttempt       `json:"model_attempt,omitempty"`
	ModelCall        *agentgo.ModelCallSettings  `json:"model_call,omitempty"`
	LengthRecovery   *agentgo.LengthRecoveryInfo `json:"length_recovery,omitempty"`
	TurnIndex        int                         `json:"turn_index,omitempty"`
	Message          json.RawMessage             `json:"message,omitempty"`
	PartialMessage   json.RawMessage             `json:"partial_message,omitempty"`
	ModelRequest     *agentgo.LLMRequest         `json:"model_request,omitempty"`
	ToolID           string                      `json:"tool_id,omitempty"`
	Tool             string                      `json:"tool,omitempty"`
	ToolLabel        string                      `json:"tool_label,omitempty"`
	Args             json.RawMessage             `json:"args,omitempty"`
	ToolResult       *ToolResultData             `json:"tool_result,omitempty"`
	Disposition      agentgo.ToolDisposition     `json:"disposition,omitempty"`
	IsError          bool                        `json:"is_error,omitempty"`
	Error            string                      `json:"error,omitempty"`
	Summary          *agentgo.RunSummary         `json:"summary,omitempty"`
	ContextOperation agentgo.ContextOperation    `json:"context_operation,omitempty"`
	ContextItems     []agentgo.ContextItem       `json:"context_items,omitempty"`
	Compaction       *agentgo.CompactionInfo     `json:"compaction,omitempty"`
	Retry            *RetryData                  `json:"retry,omitempty"`
}

// RetryData is a retry observation; Error on EventData carries its cause.
type RetryData struct {
	MaxRetries int           `json:"max_retries,omitempty"`
	Delay      time.Duration `json:"delay_ns,omitempty"`
}

// ToolResultData includes rich content that ToolResult intentionally omits from
// its legacy JSON representation. Details remain application-owned JSON data.
type ToolResultData struct {
	ToolCallID    string                 `json:"tool_call_id"`
	Content       json.RawMessage        `json:"content,omitempty"`
	ContentBlocks []agentgo.ContentBlock `json:"content_blocks,omitempty"`
	IsError       bool                   `json:"is_error,omitempty"`
	Details       any                    `json:"details,omitempty"`
}
