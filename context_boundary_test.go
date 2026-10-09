package agentgo_test

import (
	"context"
	"testing"

	"github.com/compforge/agentgo"
	agentcontext "github.com/compforge/agentgo/context"
)

func TestTransformDoesNotReplaceAcceptedBaseline(t *testing.T) {
	engine := agentcontext.NewEngine(agentcontext.EngineConfig{ContextWindow: 10000,
		Transformers: []agentcontext.Transformer{agentcontext.TransformFunc(func(_ context.Context, input agentgo.TransformContext) ([]agentgo.AgentMessage, error) {
			out := []agentgo.AgentMessage{}
			for _, m := range input.Messages {
				if m.TextContent() != "omit" {
					out = append(out, m)
				}
			}
			return out, nil
		})},
	})
	polls, observed := 0, 0
	var final *agentgo.AgentState
	for event := range agentgo.AgentLoopContinue(t.Context(), agentgo.AgentContext{Messages: []agentgo.AgentMessage{agentgo.UserMsg("omit"), agentgo.UserMsg("keep")}}, agentgo.LoopConfig{
		ContextManager: engine, Model: executionTestModel{reply: "done"},
		GetSteeringMessages: func() []agentgo.AgentMessage {
			polls++
			if polls == 2 {
				return []agentgo.AgentMessage{agentgo.UserMsg("late steering")}
			}
			return nil
		},
		ModelMiddlewares: []agentgo.ModelMiddleware{func(ctx context.Context, execution agentgo.ModelExecution, next agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
			observed = engine.Snapshot().TranscriptMessages
			return next(ctx, execution)
		}},
	}) {
		if event.Type == agentgo.EventAgentEnd {
			final = event.State
		}
	}
	if final == nil || len(final.Messages) != 4 {
		t.Fatalf("unexpected baseline: %+v", final)
	}
	if observed != 3 {
		t.Fatalf("actual runtime baseline has 3 messages before reply, engine observed baseline has %d", observed)
	}
}

func TestAfterTurnArtifactIncludedInCheckpoint(t *testing.T) {
	var afterCalled bool
	var checkpoint *agentgo.AgentState
	events := agentgo.AgentLoop(t.Context(), []agentgo.AgentMessage{agentgo.UserMsg("go")}, agentgo.AgentContext{}, agentgo.LoopConfig{
		Model: executionTestModel{reply: "done"},
		AfterTurn: func(_ context.Context, turn agentgo.AfterTurnContext) error {
			afterCalled = true
			return turn.Artifacts.AddArtifact(runtimeArtifact{"turn", "complete"}, true)
		},
	})
	for event := range events {
		if event.Type == agentgo.EventTurnEnd {
			checkpoint = event.State
		}
	}
	if !afterCalled || checkpoint == nil {
		t.Fatal("missing boundary")
	}
	replayCalls := 0
	var restored *agentgo.AgentState
	for event := range agentgo.AgentLoopContinue(t.Context(), agentgo.AgentContext{Messages: checkpoint.Messages}, agentgo.LoopConfig{
		InitialState: *checkpoint, Model: executionTestModel{reply: "done"},
		AfterTurn: func(_ context.Context, turn agentgo.AfterTurnContext) error { replayCalls++; return nil },
	}) {
		if event.Type == agentgo.EventAgentEnd {
			restored = event.State
		}
	}
	if restored == nil {
		t.Fatal("missing resumed state")
	}
	if len(restored.Artifacts) != 1 {
		t.Fatalf("completed-turn checkpoint has %d artifacts; after resume %d artifacts, AfterTurn replays=%d", len(checkpoint.Artifacts), len(restored.Artifacts), replayCalls)
	}
}
