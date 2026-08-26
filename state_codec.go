package agentgo

import "github.com/compforge/agentgo/codec"

const (
	agentStateTypeID    codec.TypeID = "agentgo.agent-state.v1"
	agentSnapshotTypeID codec.TypeID = "agentgo.agent-snapshot.v1"
	messageTypeID       codec.TypeID = "agentgo.message.v1"
)

// NewCodec constructs a codec with AgentGo's portable state types already
// registered. Applications add their own concrete AgentMessage types through
// codec.Type or codec.WithHandler.
func NewCodec(options ...codec.Option) (codec.Codec, error) {
	builtins := []codec.Option{
		codec.Type[AgentState](agentStateTypeID),
		codec.Type[AgentSnapshot](agentSnapshotTypeID),
		codec.Type[Message](messageTypeID),
	}
	return codec.NewJSON(append(builtins, options...)...)
}
