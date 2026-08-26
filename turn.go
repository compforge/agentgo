package agentgo

import "context"

// BeforeTurnContext describes the runtime immediately before one model call.
// TurnIndex is one-based. Context is a snapshot; mutating it does not change
// the running loop.
type BeforeTurnContext struct {
	TurnIndex int
	Context   AgentContext
}

// AfterTurnContext describes one committed model/tool turn, including a model
// response that ends the run with an error or abort reason. Context includes
// the assistant message and all tool results from that turn.
type AfterTurnContext struct {
	TurnIndex   int
	Message     AgentMessage
	ToolResults []ToolResult
	Context     AgentContext
	State       AgentState
}

// BeforeTurnHook runs before each model call. Returned messages are committed
// to the transcript before the request, allowing applications to prepare or
// steer the next turn without teaching the loop about business phases.
type BeforeTurnHook func(context.Context, BeforeTurnContext) ([]AgentMessage, error)

// AfterTurnHook runs after a turn has been committed and the loop has decided
// whether and how to advance. State is therefore a complete turn-boundary
// checkpoint. Returning an error stops the run.
type AfterTurnHook func(context.Context, AfterTurnContext) error

func snapshotAgentContext(current *AgentContext) AgentContext {
	return AgentContext{
		SystemPrompt: current.SystemPrompt,
		SystemBlocks: append([]SystemBlock(nil), current.SystemBlocks...),
		Messages:     copyMessages(current.Messages),
		Tools:        append([]Tool(nil), current.Tools...),
	}
}
