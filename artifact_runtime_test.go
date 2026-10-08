package agentgo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/compforge/agentgo"
	"github.com/compforge/agentgo/codec"
	agentcontext "github.com/compforge/agentgo/context"
)

type runtimeArtifact struct {
	Key  string `codec:"key"`
	Text string `codec:"text"`
}

func (a runtimeArtifact) ID() string   { return a.Key }
func (a runtimeArtifact) Kind() string { return "application-defined" }

func artifactMap(values []agentgo.Artifact) map[string]string {
	result := make(map[string]string)
	for _, value := range values {
		result[value.ID()] = value.(runtimeArtifact).Text
	}
	return result
}

func TestArtifactHookAndTransformRuntime(t *testing.T) {
	var manager agentgo.ArtifactManager
	var after, transformed, beforeTurn, afterTurn, models int
	check := func(got agentgo.ArtifactManager) {
		t.Helper()
		if got == nil || got != manager {
			t.Error("extension did not receive the Agent's manager")
		}
	}
	engine := agentcontext.NewEngine(agentcontext.EngineConfig{ContextWindow: 32000,
		Transformers: []agentcontext.Transformer{agentcontext.TransformFunc(func(_ context.Context, input agentgo.TransformContext) ([]agentgo.AgentMessage, error) {
			check(input.Artifacts)
			if _, ok := input.Artifacts.GetArtifact("initial"); !ok {
				t.Error("BeforeRun material not available to transformer")
			}
			transformed++
			return input.Messages, nil
		})},
	})
	agent := agentgo.NewAgent(
		agentgo.WithModel(executionTestModel{reply: "done"}), agentgo.WithContextManager(engine),
		agentgo.WithBeforeRun(func(_ context.Context, run agentgo.BeforeRunContext) (agentgo.AgentSnapshot, error) {
			if manager == nil {
				manager = run.Artifacts
			} else {
				check(run.Artifacts)
			}
			return run.Snapshot, run.Artifacts.AddArtifact(runtimeArtifact{"initial", "seed"}, true)
		}),
		agentgo.WithBeforeTurn(func(_ context.Context, turn agentgo.BeforeTurnContext) ([]agentgo.AgentMessage, error) {
			check(turn.Artifacts)
			beforeTurn++
			return nil, nil
		}),
		agentgo.WithAfterTurn(func(_ context.Context, turn agentgo.AfterTurnContext) error {
			check(turn.Artifacts)
			afterTurn++
			return turn.Artifacts.AddArtifact(runtimeArtifact{"turn", "complete"}, true)
		}),
		agentgo.WithModelMiddlewares(func(ctx context.Context, execution agentgo.ModelExecution, next agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
			check(execution.Artifacts)
			models++
			return next(ctx, execution)
		}),
		agentgo.WithAfterRun(func(_ context.Context, run agentgo.AfterRunContext) error {
			check(run.Artifacts)
			after++
			if run.Err != nil {
				t.Error(run.Err)
			}
			if artifactMap(run.Snapshot.State.Artifacts)["turn"] != "complete" {
				t.Error("AfterRun snapshot omitted material")
			}
			return run.Artifacts.AddArtifact(runtimeArtifact{"final", "accepted"}, true)
		}),
	)
	var terminal agentgo.AgentState
	agent.Subscribe(func(event agentgo.Event) {
		if event.Type == agentgo.EventAgentEnd && event.State != nil {
			terminal = *event.State
		}
	})
	for range 2 {
		if err := agent.Prompt(t.Context(), "go"); err != nil {
			t.Fatal(err)
		}
		agent.WaitForIdle()
	}
	if after != 2 || beforeTurn != 2 || afterTurn != 2 || models != 2 || transformed < 2 {
		t.Fatal("missing lifecycle invocation")
	}
	if artifactMap(terminal.Artifacts)["final"] != "accepted" {
		t.Fatal("terminal event omitted AfterRun material")
	}
	if err := agent.SetMessages(nil); err != nil {
		t.Fatal(err)
	}
	if len(agent.State().Artifacts) != 3 {
		t.Fatal("message replacement cleared material")
	}
	if _, err := agent.BuildLLMMessages(); err != nil {
		t.Fatal(err)
	}
	if err := agent.SetSnapshot(agentgo.AgentSnapshot{}); err != nil {
		t.Fatal(err)
	}
	if len(agent.State().Artifacts) != 0 {
		t.Fatal("full snapshot replacement retained old material")
	}
}

func TestArtifactRestoreAndBeforeRunChanges(t *testing.T) {
	restore := agentgo.AgentSnapshot{State: agentgo.AgentState{
		Messages:  []agentgo.AgentMessage{agentgo.UserMsg("restored")},
		Artifacts: []agentgo.Artifact{runtimeArtifact{"keep", "stored"}, runtimeArtifact{"replace", "stored"}, runtimeArtifact{"remove", "stored"}},
	}}
	c, err := agentgo.NewCodec(codec.Type[runtimeArtifact]("test.artifact.v1"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := c.Marshal(restore)
	if err != nil {
		t.Fatal(err)
	}
	var decoded agentgo.AgentSnapshot
	if err := c.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if artifactMap(decoded.State.Artifacts)["keep"] != "stored" {
		t.Fatal("artifact codec round trip failed")
	}
	agent := agentgo.NewAgent(agentgo.WithModel(executionTestModel{reply: "done"}),
		agentgo.WithBeforeRun(func(_ context.Context, run agentgo.BeforeRunContext) (agentgo.AgentSnapshot, error) {
			if err := run.Artifacts.AddArtifact(runtimeArtifact{"replace", "hook"}, true); err != nil {
				return run.Snapshot, err
			}
			if err := run.Artifacts.AddArtifact(runtimeArtifact{"new", "hook"}, false); err != nil {
				return run.Snapshot, err
			}
			// The returned snapshot contains this ID even though the current
			// collection does not. The explicit deletion must still win.
			run.Artifacts.DeleteArtifact("remove")
			return decoded, nil
		}))
	if err := agent.Continue(t.Context()); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()
	got := artifactMap(agent.State().Artifacts)
	if len(got) != 3 || got["replace"] != "hook" || got["new"] != "hook" || got["keep"] != "stored" {
		t.Fatalf("restore and hook operations: %v", got)
	}
	if len(decoded.State.Artifacts) != 3 || artifactMap(decoded.State.Artifacts)["replace"] != "stored" {
		t.Fatal("restoration mutated caller snapshot")
	}
	// Restoring through SetSnapshot replaces both histories, not their manager handles.
	if err := agent.SetSnapshot(decoded); err != nil {
		t.Fatal(err)
	}
	if got := artifactMap(agent.State().Artifacts); len(got) != 3 || got["replace"] != "stored" {
		t.Fatalf("SetSnapshot: %v", got)
	}
}

func TestArtifactRejectedAdmissionRollsBack(t *testing.T) {
	for _, failure := range []string{"error", "panic", "invalid_snapshot"} {
		t.Run(failure, func(t *testing.T) {
			agent := agentgo.NewAgent(agentgo.WithBeforeRun(func(_ context.Context, run agentgo.BeforeRunContext) (agentgo.AgentSnapshot, error) {
				_ = run.Artifacts.AddArtifact(runtimeArtifact{"temp", "unaccepted"}, false)
				run.Artifacts.DeleteArtifact("keep")
				switch failure {
				case "error":
					return run.Snapshot, errors.New("rejected")
				case "panic":
					panic("rejected")
				default:
					run.Snapshot.State.Artifacts = []agentgo.Artifact{runtimeArtifact{"dup", "a"}, runtimeArtifact{"dup", "b"}}
					return run.Snapshot, nil
				}
			}))
			baseline := agentgo.AgentSnapshot{State: agentgo.AgentState{Artifacts: []agentgo.Artifact{runtimeArtifact{"keep", "accepted"}}}}
			if err := agent.SetSnapshot(baseline); err != nil {
				t.Fatal(err)
			}
			if err := agent.Prompt(t.Context(), "go"); err == nil {
				t.Fatal("expected rejection")
			}
			if got := artifactMap(agent.State().Artifacts); len(got) != 1 || got["keep"] != "accepted" {
				t.Fatalf("rejected hook changed material: %v", got)
			}
		})
	}
}

func TestArtifactSnapshotValidationIsAtomic(t *testing.T) {
	agent := agentgo.NewAgent()
	good := agentgo.AgentSnapshot{State: agentgo.AgentState{Messages: []agentgo.AgentMessage{agentgo.UserMsg("keep")}, Artifacts: []agentgo.Artifact{runtimeArtifact{"keep", "value"}}}}
	if err := agent.SetSnapshot(good); err != nil {
		t.Fatal(err)
	}
	bad := agentgo.AgentSnapshot{State: agentgo.AgentState{Artifacts: []agentgo.Artifact{runtimeArtifact{"dup", "one"}, runtimeArtifact{"dup", "two"}}}}
	if err := agent.SetSnapshot(bad); !errors.Is(err, agentgo.ErrArtifactExists) {
		t.Fatalf("duplicate ID: %v", err)
	}
	if len(agent.Messages()) != 1 || artifactMap(agent.State().Artifacts)["keep"] != "value" {
		t.Fatal("invalid restore partially replaced state")
	}
}

func TestArtifactAgentAndNestedLoopIsolation(t *testing.T) {
	sharedTransformer := agentcontext.TransformFunc(func(_ context.Context, input agentgo.TransformContext) ([]agentgo.AgentMessage, error) {
		if len(input.Artifacts.ListArtifacts()) != 1 {
			t.Error("transformer leaked another Agent's material")
		}
		return input.Messages, nil
	})
	for _, id := range []string{"first", "second"} {
		agent := agentgo.NewAgent(agentgo.WithModel(executionTestModel{reply: "done"}),
			agentgo.WithContextManager(agentcontext.NewEngine(agentcontext.EngineConfig{ContextWindow: 32000, Transformers: []agentcontext.Transformer{sharedTransformer}})),
			agentgo.WithBeforeRun(func(_ context.Context, run agentgo.BeforeRunContext) (agentgo.AgentSnapshot, error) {
				if len(run.Artifacts.ListArtifacts()) != 0 {
					t.Error("new Agent inherited material")
				}
				return run.Snapshot, run.Artifacts.AddArtifact(runtimeArtifact{id, "value"}, false)
			}),
			agentgo.WithModelMiddlewares(func(ctx context.Context, execution agentgo.ModelExecution, next agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
				var terminal *agentgo.AgentState
				nested := agentgo.AgentLoop(ctx, []agentgo.AgentMessage{agentgo.UserMsg("nested")}, agentgo.AgentContext{}, agentgo.LoopConfig{
					Model: executionTestModel{reply: "nested done"},
					BeforeTurn: func(_ context.Context, turn agentgo.BeforeTurnContext) ([]agentgo.AgentMessage, error) {
						if len(turn.Artifacts.ListArtifacts()) != 0 || turn.Artifacts == execution.Artifacts {
							t.Error("nested loop inherited parent's material")
						}
						return nil, turn.Artifacts.AddArtifact(runtimeArtifact{"nested", "private"}, false)
					},
				})
				for event := range nested {
					if event.Type == agentgo.EventAgentEnd {
						terminal = event.State
					}
				}
				if terminal == nil || artifactMap(terminal.Artifacts)["nested"] != "private" {
					t.Error("bare loop omitted artifact state")
				}
				if _, ok := execution.Artifacts.GetArtifact("nested"); ok {
					t.Error("nested material leaked into parent")
				}
				return next(ctx, execution)
			}))
		if err := agent.Prompt(t.Context(), "go"); err != nil {
			t.Fatal(err)
		}
		agent.WaitForIdle()
		if got := artifactMap(agent.State().Artifacts); len(got) != 1 || got[id] != "value" {
			t.Fatalf("Agent state: %v", got)
		}
	}
}

// Summary model calls go through the same runtime middleware while compaction
// replaces only messages. Both threshold and provider-overflow paths must retain
// the material capability when rebuilding the next request.
func TestArtifactCompactionRuntime(t *testing.T) {
	for _, mode := range []string{"threshold", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			var manager agentgo.ArtifactManager
			modelCalls, summaryCalls, transforms := 0, 0, 0
			summaryModel := executionTestModel{reply: "<summary>short summary</summary>"}
			engine := agentcontext.NewEngine(agentcontext.EngineConfig{
				ContextWindow: 1024, ReserveTokens: 128,
				Compactor: agentcontext.NewSummaryCompactor(agentcontext.FullSummaryConfig{
					Model: summaryModel, ContextWindow: 1024, ReserveTokens: 128, KeepRecentTokens: 1,
				}),
				Transformers: []agentcontext.Transformer{agentcontext.TransformFunc(func(_ context.Context, input agentgo.TransformContext) ([]agentgo.AgentMessage, error) {
					transforms++
					if input.Artifacts != manager {
						t.Error("compaction view lost material capability")
					}
					return input.Messages, nil
				})},
			})
			if mode == "overflow" {
				engine.SetContextWindow(64000)
			}
			agent := agentgo.NewAgent(agentgo.WithModel(executionTestModel{reply: "done"}), agentgo.WithContextManager(engine),
				agentgo.WithBeforeRun(func(_ context.Context, run agentgo.BeforeRunContext) (agentgo.AgentSnapshot, error) {
					manager = run.Artifacts
					return run.Snapshot, run.Artifacts.AddArtifact(runtimeArtifact{"material", "kept"}, false)
				}),
				agentgo.WithModelMiddlewares(func(ctx context.Context, execution agentgo.ModelExecution, next agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
					if execution.Artifacts != manager {
						t.Error("summary/model middleware lost material capability")
					}
					if execution.ParentID != "" {
						summaryCalls++
					} else {
						modelCalls++
						if mode == "overflow" && modelCalls == 1 {
							return agentgo.ModelResult{}, agentgo.ErrContextOverflow
						}
					}
					return next(ctx, execution)
				}))
			if err := agent.PromptMessages(t.Context(), agentgo.UserMsg(strings.Repeat("old content ", 1500)), agentgo.UserMsg("latest")); err != nil {
				t.Fatal(err)
			}
			agent.WaitForIdle()
			if agent.State().Error != "" {
				t.Fatal(agent.State().Error)
			}
			if summaryCalls == 0 || transforms < 2 {
				t.Fatalf("summary=%d transforms=%d", summaryCalls, transforms)
			}
			if got := artifactMap(agent.State().Artifacts); got["material"] != "kept" {
				t.Fatal("compaction removed independent material")
			}
		})
	}
}
