package context

import (
	"context"

	"github.com/compforge/agentgo"
)

// Compactor is the only extension point for context reduction. AgentGo ships
// a default policy; applications may replace it directly. expect is the
// desired output ratio relative to input.Messages: 1 means no reduction
// and 0 asks for the strongest reduction the compactor can provide. Policies
// may summarize, archive to host-owned files, and replace or register artifacts.
// AgentLoop supplies a staged ArtifactManager shared by every chain stage;
// messages and CRUD changes publish together only after successful acceptance.
// Values remain shared: replace them instead of mutating payloads in place.
// External files are host-owned side effects and are not rolled back by AgentGo.
// Direct callers own staging and acceptance outside the Loop.
type Compactor interface {
	Compact(ctx context.Context, input agentgo.TransformContext, expect float64) ([]agentgo.AgentMessage, error)
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

func (c *compactorChain) Compact(ctx context.Context, input agentgo.TransformContext, expect float64) ([]agentgo.AgentMessage, error) {
	messages := input.Messages
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
		next, err := compactor.Compact(ctx, agentgo.TransformContext{Messages: view, Artifacts: input.Artifacts}, clampRatio(stageExpect))
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
