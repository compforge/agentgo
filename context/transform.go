package context

import (
	"context"
	"reflect"

	"github.com/compforge/agentgo"
)

// Transformer changes the request view, not the runtime baseline. Implementations
// must preserve Raw and tool-call pairing, must not mutate input messages, and
// must be deterministic and idempotent. Domain coverage semantics belong here,
// not in the generic loop or ToMessage. AgentGo supplies material through
// TransformContext; determinism and idempotence apply to identical messages and
// artifact state. Material needed for this view must be registered beforehand.
type Transformer interface {
	Transform(context.Context, agentgo.TransformContext) ([]agentgo.AgentMessage, error)
}

type TransformFunc func(context.Context, agentgo.TransformContext) ([]agentgo.AgentMessage, error)

func (f TransformFunc) Transform(ctx context.Context, input agentgo.TransformContext) ([]agentgo.AgentMessage, error) {
	return f(ctx, input)
}

// Transform runs every configured transformation before estimating the effective
// view. Compaction remains a separate operation and cannot run recursively here.
func (e *ContextEngine) Transform(ctx context.Context, input agentgo.TransformContext) ([]agentgo.AgentMessage, error) {
	messages := input.Messages
	e.Sync(messages)
	view := copyMessages(messages)
	for _, transformer := range e.cfg.Transformers {
		var err error
		view, err = transformer.Transform(ctx, agentgo.TransformContext{Messages: view, Artifacts: input.Artifacts})
		if err != nil {
			return nil, err
		}
	}
	// A rewrite can change evidence without reducing tokens. Compare message views
	// rather than token counts, and retain reported billing usage in Raw.
	changed := !reflect.DeepEqual(messages, view)
	if changed {
		view = InvalidateUsage(view)
	}
	e.setLastState(view, ptrUsage(e.estimateUsage(view)), "transformed", changed, nil)
	return view, nil
}
