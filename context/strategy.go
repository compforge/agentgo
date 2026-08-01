package context

import (
	"context"

	"github.com/compforge/agentgo"
)

// Compactor is the only extension point for context reduction. AgentGo ships
// a default policy; applications may replace it directly. expect is the
// desired output ratio relative to the current model view: 1 means no change
// and 0 asks for the strongest reduction the compactor can provide.
type Compactor interface {
	Compact(ctx context.Context, messages []agentgo.AgentMessage, expect float64) ([]agentgo.AgentMessage, error)
}

type compactorChain struct {
	compactors []Compactor
}

type windowAwareCompactor interface {
	setContextWindow(window, reserve int)
}

// Chain combines compactors into one policy. Every stage receives the ratio
// still required to reach the original request, and later stages are skipped
// once that target has been met.
func Chain(compactors ...Compactor) Compactor {
	return &compactorChain{compactors: append([]Compactor(nil), compactors...)}
}

func (c *compactorChain) Compact(ctx context.Context, messages []agentgo.AgentMessage, expect float64) ([]agentgo.AgentMessage, error) {
	view := copyMessages(messages)
	target := int(float64(EstimateTotal(view)) * clampRatio(expect))
	for _, compactor := range c.compactors {
		if EstimateTotal(view) <= target {
			break
		}
		current := EstimateTotal(view)
		stageExpect := 0.0
		if current > 0 {
			stageExpect = float64(target) / float64(current)
		}
		next, err := compactor.Compact(ctx, view, clampRatio(stageExpect))
		if err != nil {
			return nil, err
		}
		view = next
	}
	return view, nil
}

func (c *compactorChain) setContextWindow(window, reserve int) {
	for _, compactor := range c.compactors {
		if aware, ok := compactor.(windowAwareCompactor); ok {
			aware.setContextWindow(window, reserve)
		}
	}
}

// ToolClassifier returns true when a tool result can be aggressively rewritten
// by a tool-result compactor.
type ToolClassifier func(toolName string) bool

// PostSummaryHook injects lightweight reminder messages after a summary
// checkpoint is produced. Hooks must be side-effect free and should not
// perform I/O such as file reads or tool execution.
type PostSummaryHook func(ctx context.Context, info SummaryInfo, kept []agentgo.AgentMessage) ([]agentgo.AgentMessage, error)
