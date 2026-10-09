package agentgo_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/compforge/agentgo"
	agentcontext "github.com/compforge/agentgo/context"
)

type artifactCompactorFunc func(context.Context, agentgo.TransformContext, float64) ([]agentgo.AgentMessage, error)

func (f artifactCompactorFunc) Compact(ctx context.Context, input agentgo.TransformContext, expect float64) ([]agentgo.AgentMessage, error) {
	return f(ctx, input, expect)
}

func TestCompactionPublishesMessagesAndArtifactsTogether(t *testing.T) {
	for _, mode := range []string{"success", "no_message_change", "compact_error", "commit_error", "panic", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			failed := agentgo.ErrContextOverflow // resembles a retryable provider error
			var live agentgo.ArtifactManager
			compactCalls, transformCalls, modelCalls, commits := 0, 0, 0, 0
			original := agentgo.UserMsg(strings.Repeat("history ", 1000))
			compactor := artifactCompactorFunc(func(_ context.Context, input agentgo.TransformContext, _ float64) ([]agentgo.AgentMessage, error) {
				compactCalls++
				if input.Artifacts == live {
					t.Fatal("compaction received live manager")
				}
				if _, ok := input.Artifacts.GetArtifact("keep"); !ok {
					t.Fatal("missing input artifacts")
				}
				input.Artifacts.DeleteArtifact("delete")
				if err := input.Artifacts.AddArtifact(runtimeArtifact{"keep", "replacement"}, true); err != nil {
					return nil, err
				}
				if err := input.Artifacts.AddArtifact(runtimeArtifact{"file", "archive.txt"}, false); err != nil {
					return nil, err
				}
				if _, ok := live.GetArtifact("file"); ok {
					t.Fatal("candidate leaked before commit")
				}
				if mode == "compact_error" {
					return nil, failed
				}
				if mode == "panic" {
					panic("failed")
				}
				if mode == "no_message_change" {
					return input.Messages, nil
				}
				return []agentgo.AgentMessage{agentgo.UserMsg("see archive.txt")}, nil
			})
			window := 1000
			if mode == "overflow" {
				window = 100000
			}
			engine := agentcontext.NewEngine(agentcontext.EngineConfig{ContextWindow: window, ReserveTokens: 100, Compactor: agentcontext.Chain(compactor), Transformers: []agentcontext.Transformer{
				agentcontext.TransformFunc(func(_ context.Context, input agentgo.TransformContext) ([]agentgo.AgentMessage, error) {
					transformCalls++
					if mode != "overflow" || modelCalls > 0 {
						if got, ok := input.Artifacts.GetArtifact("file"); !ok || got.(runtimeArtifact).Text != "archive.txt" {
							t.Error("transform ran before artifact commit")
						}
					}
					return input.Messages, nil
				}),
			}})
			var final *agentgo.AgentState
			compactEvents := 0
			events := agentgo.AgentLoop(t.Context(), []agentgo.AgentMessage{original}, agentgo.AgentContext{}, agentgo.LoopConfig{
				Model: executionTestModel{reply: "done"}, ContextManager: engine,
				BeforeTurn: func(_ context.Context, turn agentgo.BeforeTurnContext) ([]agentgo.AgentMessage, error) {
					live = turn.Artifacts
					if err := live.AddArtifact(runtimeArtifact{"keep", "original"}, false); err != nil {
						return nil, err
					}
					return nil, live.AddArtifact(runtimeArtifact{"delete", "old"}, false)
				},
				CommitContext: func(candidate agentgo.ContextCommitResult) error {
					commits++
					if candidate.Compaction != nil && candidate.Compaction.Committed {
						t.Error("candidate already marked committed")
					}
					if artifactMap(candidate.Artifacts)["file"] != "archive.txt" {
						t.Error("durable commit omitted artifacts")
					}
					if artifactMap(live.ListArtifacts())["keep"] != "original" {
						t.Error("live artifacts changed before acceptance")
					}
					if mode == "commit_error" {
						return failed
					}
					return nil
				},
				ModelMiddlewares: []agentgo.ModelMiddleware{func(ctx context.Context, execution agentgo.ModelExecution, next agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
					modelCalls++
					if mode == "overflow" && modelCalls == 1 {
						return agentgo.ModelResult{}, failed
					}
					return next(ctx, execution)
				}},
			})
			for event := range events {
				if event.Type == agentgo.EventContextCompacted {
					compactEvents++
					if !event.Compaction.Committed {
						t.Error("accepted event not committed")
					}
				}
				if event.Type == agentgo.EventAgentEnd {
					final = event.State
				}
			}
			if final == nil || compactCalls != 1 {
				t.Fatalf("final=%v compactions=%d", final, compactCalls)
			}
			failure := mode == "compact_error" || mode == "commit_error" || mode == "panic"
			got := artifactMap(final.Artifacts)
			if failure {
				if got["keep"] != "original" || got["delete"] != "old" || len(got) != 2 || len(final.Messages) != 1 || final.Messages[0].TextContent() != original.TextContent() || modelCalls != 0 || transformCalls != 0 || compactEvents != 0 {
					t.Fatalf("failed transaction leaked: artifacts=%v messages=%d model=%d transform=%d events=%d", got, len(final.Messages), modelCalls, transformCalls, compactEvents)
				}
			} else {
				if got["keep"] != "replacement" || got["file"] != "archive.txt" || len(got) != 2 || commits != 1 || modelCalls != transformCalls {
					t.Fatalf("commit incomplete: artifacts=%v commits=%d models=%d transforms=%d", got, commits, modelCalls, transformCalls)
				}
			}
		})
	}
}

type contextRetryError struct{}

func (contextRetryError) Error() string             { return "retry" }
func (contextRetryError) Retryable() bool           { return true }
func (contextRetryError) RetryAfter() time.Duration { return time.Millisecond }

func TestOrdinaryRetryTransformsOnceWithoutRecompacting(t *testing.T) {
	compacts, transforms, models := 0, 0, 0
	engine := agentcontext.NewEngine(agentcontext.EngineConfig{ContextWindow: 1, ReserveTokens: 1,
		Compactor: artifactCompactorFunc(func(_ context.Context, input agentgo.TransformContext, _ float64) ([]agentgo.AgentMessage, error) {
			compacts++
			return input.Messages, nil
		}),
		Transformers: []agentcontext.Transformer{agentcontext.TransformFunc(func(_ context.Context, input agentgo.TransformContext) ([]agentgo.AgentMessage, error) {
			transforms++
			// Expansion has no relation to the compaction trigger.
			return append(input.Messages, agentgo.UserMsg(strings.Repeat("expand ", 1000))), nil
		})},
	})
	var final *agentgo.AgentState
	for event := range agentgo.AgentLoop(t.Context(), []agentgo.AgentMessage{agentgo.UserMsg("task")}, agentgo.AgentContext{}, agentgo.LoopConfig{
		Model: executionTestModel{reply: "done"}, ContextManager: engine, MaxRetries: 1,
		ModelMiddlewares: []agentgo.ModelMiddleware{func(ctx context.Context, execution agentgo.ModelExecution, next agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
			models++
			if execution.ID != "model-1" || execution.Attempt != models {
				t.Errorf("execution=%+v", execution.Execution)
			}
			if len(execution.Request.Messages) != 2 {
				t.Error("projection accumulated across retries")
			}
			if models == 1 {
				return agentgo.ModelResult{}, contextRetryError{}
			}
			return next(ctx, execution)
		}},
	}) {
		if event.Type == agentgo.EventAgentEnd {
			final = event.State
		}
	}
	if compacts != 1 || transforms != 2 || models != 2 || final == nil || len(final.Messages) != 2 {
		t.Fatalf("compacts=%d transforms=%d models=%d final=%v", compacts, transforms, models, final)
	}
}

func TestAfterTurnFailurePreservesAcceptedCheckpoint(t *testing.T) {
	for _, panicHook := range []bool{false, true} {
		var checkpoint, final *agentgo.AgentState
		for event := range agentgo.AgentLoop(t.Context(), []agentgo.AgentMessage{agentgo.UserMsg("task")}, agentgo.AgentContext{}, agentgo.LoopConfig{
			Model: executionTestModel{reply: "done"},
			AfterTurn: func(_ context.Context, turn agentgo.AfterTurnContext) error {
				if len(turn.State.Artifacts) != 0 {
					t.Error("hook input is not entry snapshot")
				}
				if err := turn.Artifacts.AddArtifact(runtimeArtifact{"completed", "yes"}, false); err != nil {
					return err
				}
				if panicHook {
					panic("hook failed")
				}
				return errors.New("hook failed")
			},
		}) {
			if event.Type == agentgo.EventTurnEnd {
				checkpoint = event.State
			}
			if event.Type == agentgo.EventAgentEnd {
				final = event.State
				if event.Err == nil {
					t.Error("hook failure lost")
				}
			}
		}
		if checkpoint == nil || final == nil || checkpoint.Progress.CompletedTurns != 1 || len(checkpoint.Messages) != 2 || artifactMap(checkpoint.Artifacts)["completed"] != "yes" || len(final.Artifacts) != 1 {
			t.Fatalf("lost completed turn: checkpoint=%+v final=%+v", checkpoint, final)
		}
	}
}
