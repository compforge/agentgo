package agentgo

// RunProgress is the loop-owned progress needed to continue an unfinished
// run from a completed turn boundary.
type RunProgress struct {
	Active                bool           `codec:"active"` // false after terminal run finalization
	NextTurn              bool           `codec:"next_turn"`
	CompletedTurns        int            `codec:"completed_turns"`
	LengthRecoveries      int            `codec:"length_recoveries"`
	ToolCalls             int            `codec:"tool_calls"`
	ToolErrors            int            `codec:"tool_errors"`
	ConsecutiveToolErrors map[string]int `codec:"consecutive_tool_errors,omitempty"`
	PendingMessages       []AgentMessage `codec:"pending_messages,omitempty"` // loop-owned messages accepted for the next turn
}

// AgentState is the Loop-owned execution state. Codec tags define its portable
// projection; model, tools, hooks, streams and in-flight calls remain
// process-local and are rebound or recreated by the host.
type AgentState struct {
	SystemPrompt     string
	Messages         []AgentMessage `codec:"messages"`
	Tools            []Tool
	IsRunning        bool
	StreamMessage    AgentMessage
	PendingToolCalls map[string]struct{}
	TotalUsage       Usage       `codec:"total_usage"`
	Progress         RunProgress `codec:"progress"`
	Error            string
}

// AgentSnapshot is the stateful Agent's point-in-time view. Its codec
// projection contains Loop-owned state plus input accepted by Agent but not
// yet handed to the Loop.
type AgentSnapshot struct {
	State         AgentState     `codec:"state"`
	SteeringQueue []AgentMessage `codec:"steering_queue,omitempty"`
	FollowUpQueue []AgentMessage `codec:"follow_up_queue,omitempty"`
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
