package context

import (
	"context"
	"sort"

	"github.com/compforge/agentgo"
)

// CompactionStrategy asks application messages for progressively smaller
// semantic representations before generic trimming or summarization discards
// structure the application understands better.
type CompactionStrategy struct{}

func NewCompactionStrategy() *CompactionStrategy {
	return &CompactionStrategy{}
}

func (s *CompactionStrategy) Name() string { return "message_compaction" }

func (s *CompactionStrategy) Apply(
	_ context.Context,
	_ []agentgo.AgentMessage,
	view []agentgo.AgentMessage,
	budget Budget,
) ([]agentgo.AgentMessage, StrategyResult, error) {
	if len(view) == 0 || budget.Tokens <= budget.Threshold {
		return view, StrategyResult{Name: s.Name()}, nil
	}

	out := copyMessages(view)
	tokens := budget.Tokens
	saved := 0
	applied := false

	// Exhaust lower-priority messages before touching a higher-priority tier.
	// Within one tier, advance each message by one step from newest to oldest;
	// this keeps the longest possible prompt prefix byte-identical for cache
	// reuse without letting one message monopolize the tier.
	for _, priority := range compactionPriorities(out) {
		for tokens > budget.Threshold {
			progressed := false
			for i := len(out) - 1; i >= 0; i-- {
				message := out[i]
				if message.Priority() != priority {
					continue
				}
				compactable, ok := message.(agentgo.CompactableAgentMessage)
				if !ok {
					continue
				}
				next, ok := compactable.Compact()
				if !ok || next == nil {
					continue
				}

				before := EstimateTokens(message)
				after := EstimateTokens(next)
				if after >= before {
					continue
				}

				out[i] = next
				delta := before - after
				tokens -= delta
				saved += delta
				applied = true
				progressed = true
				if tokens <= budget.Threshold {
					break
				}
			}
			if !progressed {
				break
			}
		}
		if tokens <= budget.Threshold {
			break
		}
	}

	return out, StrategyResult{
		Applied:     applied,
		TokensSaved: saved,
		Name:        s.Name(),
	}, nil
}

func compactionPriorities(messages []agentgo.AgentMessage) []int {
	seen := make(map[int]struct{})
	for _, message := range messages {
		if _, ok := message.(agentgo.CompactableAgentMessage); ok {
			seen[message.Priority()] = struct{}{}
		}
	}
	priorities := make([]int, 0, len(seen))
	for priority := range seen {
		priorities = append(priorities, priority)
	}
	sort.Ints(priorities)
	return priorities
}
