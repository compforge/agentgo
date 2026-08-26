package agentgo

import (
	"encoding/json"
	"fmt"
)

const agentStateVersion = 1

// RunProgress is the loop-owned progress needed to continue an unfinished
// run from a completed turn boundary.
type RunProgress struct {
	Active                bool // false after AfterRun finalization
	NextTurn              bool // loop has decided to advance beyond the checkpointed turn
	CompletedTurns        int
	LengthRecoveries      int
	ToolCalls             int
	ToolErrors            int
	ConsecutiveToolErrors map[string]int
	PendingMessages       []AgentMessage // loop-owned messages accepted for the next turn
}

// AgentState is a snapshot of the agent's current state. Marshal persists only
// the durable fields; model, tools, hooks, streams and in-flight calls are
// rebound or recreated by the hosting process.
type AgentState struct {
	SystemPrompt     string
	Messages         []AgentMessage
	Tools            []Tool
	IsRunning        bool
	StreamMessage    AgentMessage
	PendingToolCalls map[string]struct{}
	TotalUsage       Usage
	Progress         RunProgress
	Error            string
}

type agentStateWire struct {
	Version    int             `json:"version"`
	Messages   []Message       `json:"messages"`
	TotalUsage Usage           `json:"total_usage"`
	Progress   runProgressWire `json:"progress"`
}

type runProgressWire struct {
	Active                bool           `json:"active"`
	NextTurn              bool           `json:"next_turn"`
	CompletedTurns        int            `json:"completed_turns"`
	LengthRecoveries      int            `json:"length_recoveries"`
	ToolCalls             int            `json:"tool_calls"`
	ToolErrors            int            `json:"tool_errors"`
	ConsecutiveToolErrors map[string]int `json:"consecutive_tool_errors,omitempty"`
	PendingMessages       []Message      `json:"pending_messages,omitempty"`
}

// Marshal serializes the durable projection of the state.
func (s AgentState) Marshal() ([]byte, error) {
	messages, err := marshalStateMessages(s.Messages)
	if err != nil {
		return nil, fmt.Errorf("marshal agent state messages: %w", err)
	}
	pending, err := marshalStateMessages(s.Progress.PendingMessages)
	if err != nil {
		return nil, fmt.Errorf("marshal agent state pending messages: %w", err)
	}
	wire := agentStateWire{
		Version:    agentStateVersion,
		Messages:   messages,
		TotalUsage: s.TotalUsage,
		Progress: runProgressWire{
			Active:                s.Progress.Active,
			NextTurn:              s.Progress.NextTurn,
			CompletedTurns:        s.Progress.CompletedTurns,
			LengthRecoveries:      s.Progress.LengthRecoveries,
			ToolCalls:             s.Progress.ToolCalls,
			ToolErrors:            s.Progress.ToolErrors,
			ConsecutiveToolErrors: cloneStringIntMap(s.Progress.ConsecutiveToolErrors),
			PendingMessages:       pending,
		},
	}
	return json.Marshal(wire)
}

// Unmarshal restores the durable projection of a state. Runtime-only fields
// remain zero and are rebound by Agent when the next run starts.
func (s *AgentState) Unmarshal(data []byte) error {
	var wire agentStateWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("unmarshal agent state: %w", err)
	}
	if wire.Version != agentStateVersion {
		return fmt.Errorf("unsupported agent state version %d", wire.Version)
	}
	*s = AgentState{
		Messages:   ToAgentMessages(wire.Messages),
		TotalUsage: wire.TotalUsage,
		Progress: RunProgress{
			Active:                wire.Progress.Active,
			NextTurn:              wire.Progress.NextTurn,
			CompletedTurns:        wire.Progress.CompletedTurns,
			LengthRecoveries:      wire.Progress.LengthRecoveries,
			ToolCalls:             wire.Progress.ToolCalls,
			ToolErrors:            wire.Progress.ToolErrors,
			ConsecutiveToolErrors: cloneStringIntMap(wire.Progress.ConsecutiveToolErrors),
			PendingMessages:       ToAgentMessages(wire.Progress.PendingMessages),
		},
	}
	return nil
}

func marshalStateMessages(messages []AgentMessage) ([]Message, error) {
	out := make([]Message, 0, len(messages))
	for i, message := range messages {
		switch value := message.(type) {
		case Message:
			out = append(out, value)
		case *Message:
			if value == nil {
				return nil, fmt.Errorf("message %d is a nil *Message", i)
			}
			out = append(out, *value)
		default:
			return nil, fmt.Errorf("message %d has unsupported durable type %T", i, message)
		}
	}
	return out, nil
}

func cloneStringIntMap(source map[string]int) map[string]int {
	if source == nil {
		return nil
	}
	clone := make(map[string]int, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneRunProgress(progress RunProgress) RunProgress {
	progress.ConsecutiveToolErrors = cloneStringIntMap(progress.ConsecutiveToolErrors)
	progress.PendingMessages = copyMessages(progress.PendingMessages)
	return progress
}
