package context

import (
	"github.com/compforge/agentgo"
	"github.com/compforge/agentgo/codec"
)

// CodecOptions registers every context-owned message representation, including
// private wrappers. Pass these to agentgo.NewCodec along with registrations for
// application messages/artifacts. Both the current view and Raw survive restore.
func CodecOptions() []codec.Option {
	return []codec.Option{
		codec.Type[ContextSummary]("agentgo.context-summary.v1"),
		codec.WithHandler[projectedMessage, projectedRepresentation]("agentgo.context-projected.v1", projectedCodec{}),
		codec.WithHandler[uncalibratedMessage, uncalibratedRepresentation]("agentgo.context-uncalibrated.v1", uncalibratedCodec{}),
	}
}

type projectedRepresentation struct {
	Raw     agentgo.AgentMessage `codec:"raw"`
	Current agentgo.AgentMessage `codec:"current"`
}
type projectedCodec struct{}

func (projectedCodec) Encode(m projectedMessage) (projectedRepresentation, error) {
	return projectedRepresentation{Raw: m.raw, Current: m.current}, nil
}
func (projectedCodec) Decode(m projectedRepresentation) (projectedMessage, error) {
	return projectedMessage{raw: m.Raw, current: m.Current}, nil
}

type uncalibratedRepresentation struct {
	Message agentgo.AgentMessage `codec:"message"`
}
type uncalibratedCodec struct{}

func (uncalibratedCodec) Encode(m uncalibratedMessage) (uncalibratedRepresentation, error) {
	return uncalibratedRepresentation{Message: m.AgentMessage}, nil
}
func (uncalibratedCodec) Decode(m uncalibratedRepresentation) (uncalibratedMessage, error) {
	return uncalibratedMessage{m.Message}, nil
}
