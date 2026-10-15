// Package streaming contains the shared publication boundary for model adapters.
package streaming

import (
	"maps"
	"slices"

	"github.com/compforge/agentgo"
)

// Snapshot detaches the mutable message fields used by streaming adapters.
// Metadata values are opaque, read-only values; adapters replace entries rather
// than mutate values already published to consumers.
func Snapshot(message agentgo.Message) agentgo.Message {
	message.Content = slices.Clone(message.Content)
	for i := range message.Content {
		block := &message.Content[i]
		if block.ToolCall != nil {
			call := *block.ToolCall
			call.Args = slices.Clone(call.Args)
			block.ToolCall = &call
		}
		if block.Image != nil {
			image := *block.Image
			block.Image = &image
		}
	}
	message.Metadata = maps.Clone(message.Metadata)
	if message.Usage != nil {
		usage := *message.Usage
		if usage.Cost != nil {
			cost := *usage.Cost
			usage.Cost = &cost
		}
		message.Usage = &usage
	}
	return message
}
