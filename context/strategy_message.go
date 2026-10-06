package context

import (
	"context"
	"sort"

	"github.com/compforge/agentgo"
)

// MessageCompactor asks application messages for progressively smaller
// semantic representations before generic trimming or summarization discards
// structure the application understands better.
type MessageCompactor struct {
	// OldestFirst compacts older messages first within a priority tier. The
	// default preserves the longest unchanged prefix for provider caches.
	OldestFirst bool
}

func NewMessageCompactor() *MessageCompactor {
	return &MessageCompactor{}
}

func (s *MessageCompactor) Compact(
	_ context.Context,
	messages []agentgo.AgentMessage,
	expect float64,
) ([]agentgo.AgentMessage, error) {
	if len(messages) == 0 || expect >= 1 {
		return messages, nil
	}

	out := copyMessages(messages)
	tokens := EstimateTotal(out)
	target := int(float64(tokens) * clampRatio(expect))

	// Priority controls importance; the selected order breaks ties only.
	for _, priority := range compactionPriorities(out) {
		for offset := 0; offset < len(out) && tokens > target; offset++ {
			i := len(out) - 1 - offset
			if s.OldestFirst {
				i = offset
			}
			message := out[i]
			if message.Priority() != priority {
				continue
			}

			before := EstimateTokens(message)
			rawTokens := EstimateTokens(message.Raw())
			if before <= 0 || rawTokens <= 0 {
				continue
			}

			deficit := tokens - target
			desiredTokens := max(0, before-deficit)
			messageExpect := float64(desiredTokens) / float64(rawTokens)
			next, _ := message.Compact(clampRatio(messageExpect))
			if next == nil {
				continue
			}

			after := EstimateTokens(next)
			if after >= before {
				continue
			}
			out[i] = next
			tokens -= before - after
		}
		if tokens <= target {
			break
		}
	}

	return out, nil
}

func compactionPriorities(messages []agentgo.AgentMessage) []int {
	seen := make(map[int]struct{})
	for _, message := range messages {
		seen[message.Priority()] = struct{}{}
	}
	priorities := make([]int, 0, len(seen))
	for priority := range seen {
		priorities = append(priorities, priority)
	}
	sort.Ints(priorities)
	return priorities
}

func clampRatio(ratio float64) float64 {
	return min(1, max(0, ratio))
}
