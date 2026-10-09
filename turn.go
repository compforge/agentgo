package agentgo

import (
	"context"
	"fmt"
)

// BeforeTurnContext describes the runtime immediately before one model call.
// TurnIndex is one-based. Context is a snapshot; mutating it does not change
// the running loop.
type BeforeTurnContext struct {
	Artifacts ArtifactManager // Runtime-owned material; do not retain beyond the callback.
	TurnIndex int
	Context   AgentContext
}

// AfterTurnContext describes one committed model/tool turn, including a model
// response that ends the run with an error or abort reason. Context includes
// the assistant message and all tool results from that turn. State is the hook
// entry snapshot; artifact writes appear in the subsequent EventTurnEnd.State.
type AfterTurnContext struct {
	Artifacts   ArtifactManager // Runtime-owned material; do not retain beyond the callback.
	TurnIndex   int
	Message     AgentMessage
	ToolResults []ToolResult
	Context     AgentContext
	State       AgentState
}

// BeforeTurnHook runs once before each logical turn, not once per retry. Returned messages are committed
// to the transcript before the request, allowing applications to prepare or
// steer the next turn without teaching the loop about business phases.
type BeforeTurnHook func(context.Context, BeforeTurnContext) ([]AgentMessage, error)

// AfterTurnHook runs after a turn has been committed and the loop has decided
// whether and how to advance. State therefore reflects a complete turn
// boundary. It is not a finally hook: preparation/provider/commit failures do
// not call it. Their incomplete turn_end event carries Err without a State.
// Writes are included in EventTurnEnd.State before publication. Returning an
// error or panicking stops the run after publishing that completed checkpoint.
type AfterTurnHook func(context.Context, AfterTurnContext) error

func snapshotAgentContext(current *AgentContext) AgentContext {
	return AgentContext{
		SystemPrompt: current.SystemPrompt,
		SystemBlocks: append([]SystemBlock(nil), current.SystemBlocks...),
		Messages:     copyMessages(current.Messages),
		Tools:        append([]Tool(nil), current.Tools...),
	}
}

func callAfterTurn(ctx context.Context, hook AfterTurnHook, turn AfterTurnContext) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return hook(ctx, turn)
}
