package agentgo

import "context"

// BeforeTurnContext describes the runtime immediately before one model call.
// TurnIndex is one-based. Context is a snapshot; mutating it does not change
// the running loop.
type BeforeTurnContext struct {
	TurnIndex int
	Context   AgentContext
}

// AfterTurnContext describes one normally completed model/tool turn. Context
// includes the assistant message and all tool results from that turn.
type AfterTurnContext struct {
	TurnIndex   int
	Message     AgentMessage
	ToolResults []ToolResult
	Context     AgentContext
}

// BeforeTurnHook runs before each model call. Returned messages are committed
// to the transcript before the request, allowing applications to prepare or
// steer the next turn without teaching the loop about business phases.
type BeforeTurnHook func(context.Context, BeforeTurnContext) ([]AgentMessage, error)

// AfterTurnHook runs after a normal turn's assistant message and tool results
// have been committed. It may update application state consumed by the next
// BeforeTurnHook. Returning an error stops the run.
type AfterTurnHook func(context.Context, AfterTurnContext) error

func snapshotAgentContext(current *AgentContext) AgentContext {
	return AgentContext{
		SystemPrompt: current.SystemPrompt,
		SystemBlocks: append([]SystemBlock(nil), current.SystemBlocks...),
		Messages:     copyMessages(current.Messages),
		Tools:        append([]Tool(nil), current.Tools...),
	}
}
