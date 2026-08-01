package context

import (
	"context"

	"github.com/compforge/agentgo"
)

// MessageCompactionStrategy asks application messages for progressively
// smaller semantic representations before generic text trimming or LLM
// summarization discards structure the application understands better.
type MessageCompactionStrategy struct{}

func NewMessageCompaction() *MessageCompactionStrategy {
	return &MessageCompactionStrategy{}
}

func (s *MessageCompactionStrategy) Name() string { return "message_compaction" }

func (s *MessageCompactionStrategy) Apply(
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

	// Advance every eligible message by one step before starting another
	// round. This lets each message type define its own number and meaning of
	// compaction stages without one old message monopolizing the budget.
	for tokens > budget.Threshold {
		progressed := false
		for i, message := range out {
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

	return out, StrategyResult{
		Applied:     applied,
		TokensSaved: saved,
		Name:        s.Name(),
	}, nil
}
