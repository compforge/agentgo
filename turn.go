package agentgo

import "context"

// BeforeTurnContext describes the runtime immediately before one model call.
// TurnIndex is one-based. Context is a snapshot; mutating it does not change
// the running loop.
type BeforeTurnContext struct {
	TurnIndex int
	Context   AgentContext
}

// BeforeModelCallContext describes the runtime after BeforeTurn messages have
// been committed and before one logical model call begins. TurnIndex is
// one-based. Context is the transcript baseline before context projection; it
// is a snapshot, and mutating it does not change the running loop.
type BeforeModelCallContext struct {
	TurnIndex int
	Context   AgentContext
}

// AfterModelCallContext describes a completed logical model call before its
// assistant message is committed or any requested tools are executed. Context
// is the transcript baseline at that boundary and does not include Message.
type AfterModelCallContext struct {
	TurnIndex int
	Message   Message
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

// BeforeModelCallHook runs once before each logical model call. Returned call
// options apply to every internal retry of that call and override static loop
// call options when they configure the same field.
type BeforeModelCallHook func(context.Context, BeforeModelCallContext) ([]CallOption, error)

// AfterModelCallHook runs once after a logical model call returns a Message and
// before that message is committed or any requested tools are executed.
// Returning an error rejects the response and stops the run.
type AfterModelCallHook func(context.Context, AfterModelCallContext) error

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
