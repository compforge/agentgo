package agentgo_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/compforge/agentgo"
	agentcontext "github.com/compforge/agentgo/context"
)

type executionTestModel struct {
	reply string
}

func (m executionTestModel) Generate(context.Context, []agentgo.Message, []agentgo.ToolSpec, ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
	panic("model executor must consume GenerateStream")
}

func (m executionTestModel) GenerateStream(context.Context, []agentgo.Message, []agentgo.ToolSpec, ...agentgo.CallOption) (<-chan agentgo.StreamEvent, error) {
	message := agentgo.Message{
		Role:       agentgo.RoleAssistant,
		Content:    []agentgo.ContentBlock{agentgo.TextBlock(m.reply)},
		StopReason: agentgo.StopReasonStop,
	}
	events := make(chan agentgo.StreamEvent, 2)
	events <- agentgo.StreamEvent{Type: agentgo.StreamEventTextDelta, Message: message, Delta: m.reply}
	events <- agentgo.StreamEvent{Type: agentgo.StreamEventDone, Message: message, StopReason: message.StopReason}
	close(events)
	return events, nil
}

func (m executionTestModel) SupportsTools() bool { return true }

func TestAgentLoopModelMiddlewareCoversContextSummaryExecution(t *testing.T) {
	summaryModel := executionTestModel{reply: "<summary>compacted history</summary>"}
	mainModel := executionTestModel{reply: "done"}
	manager := agentcontext.NewEngine(agentcontext.EngineConfig{
		ContextWindow: 1024,
		ReserveTokens: 128,
		Compactor: agentcontext.NewSummaryCompactor(agentcontext.FullSummaryConfig{
			Model:            summaryModel,
			ContextWindow:    1024,
			ReserveTokens:    128,
			KeepRecentTokens: 1,
		}),
	})

	var middlewareExecutions []agentgo.Execution
	events := agentgo.AgentLoopContinue(
		context.Background(),
		agentgo.AgentContext{Messages: []agentgo.AgentMessage{
			agentgo.UserMsg(strings.Repeat("old context ", 2000)),
			agentgo.UserMsg("keep"),
		}},
		agentgo.LoopConfig{
			Model:          mainModel,
			ContextManager: manager,
			ModelExecutor: agentgo.NewModelExecutor(agentgo.ModelExecutorConfig{Middlewares: []agentgo.ModelMiddleware{
				func(ctx context.Context, execution agentgo.ModelExecution, next agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
					middlewareExecutions = append(middlewareExecutions, execution.Execution)
					return next(ctx, execution)
				},
			}}),
		},
	)

	var observed []agentgo.Event
	for event := range events {
		observed = append(observed, event)
	}

	if len(middlewareExecutions) != 2 {
		t.Fatalf("model middleware executions = %#v, want summary and main model", middlewareExecutions)
	}
	summaryExecution := middlewareExecutions[0]
	if summaryExecution.ID != "compact-1-threshold/summary-history" || summaryExecution.Kind != agentgo.ExecutionKindModel || summaryExecution.ParentID != "compact-1-threshold" || summaryExecution.TurnIndex != 1 || summaryExecution.Attempt != 1 {
		t.Fatalf("summary execution = %#v", summaryExecution)
	}
	mainExecution := middlewareExecutions[1]
	if mainExecution.ID != "model-1" || mainExecution.Kind != agentgo.ExecutionKindModel || mainExecution.ParentID != "" || mainExecution.TurnIndex != 1 || mainExecution.Attempt != 1 {
		t.Fatalf("main execution = %#v", mainExecution)
	}

	var compactEvent *agentgo.Event
	modelEvents := make(map[string]int)
	streamEvents := make(map[string]int)
	for i := range observed {
		event := &observed[i]
		if event.Type == agentgo.EventModelStream {
			streamEvents[event.Execution.ID]++
		}
		if event.Type == agentgo.EventMessageStart || event.Type == agentgo.EventMessageUpdate || event.Type == agentgo.EventMessageEnd {
			if event.Execution != nil && event.Execution.ID == summaryExecution.ID {
				t.Fatal("summary leaked into conversation message events")
			}
		}
		if event.Type == agentgo.EventContextCompacted {
			compactEvent = event
		}
		if (event.Type == agentgo.EventModelExecStart || event.Type == agentgo.EventModelExecEnd) && event.Execution != nil {
			modelEvents[event.Execution.ID]++
		}
	}
	if streamEvents[summaryExecution.ID] != 1 || streamEvents[mainExecution.ID] != 1 {
		t.Fatalf("stream facts missing: %v", streamEvents)
	}
	if compactEvent == nil || compactEvent.Execution == nil || compactEvent.Execution.ID != "compact-1-threshold" || compactEvent.Execution.Kind != agentgo.ExecutionKindCompact {
		t.Fatalf("compaction event = %#v", compactEvent)
	}
	if modelEvents[summaryExecution.ID] != 2 || modelEvents[mainExecution.ID] != 2 {
		t.Fatalf("model execution events = %#v, want start/end for summary and main", modelEvents)
	}
}

// +case:id=summary_executor_staged_artifacts,desc=`Summary middleware writes material before context commit`,expect=`Accepted compaction publishes it; a rejected commit leaves live material unchanged`
func TestSummaryExecutorStagesArtifacts(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint("reject=", reject), func(t *testing.T) {
			var live agentgo.ArtifactManager
			var final *agentgo.AgentState
			summaryCalls, commits := 0, 0
			failed := errors.New("commit rejected")
			engine := agentcontext.NewEngine(agentcontext.EngineConfig{
				ContextWindow: 1024, ReserveTokens: 128,
				Compactor: agentcontext.Chain(agentcontext.NewSummaryCompactor(agentcontext.FullSummaryConfig{
					Model: executionTestModel{reply: "<summary>short history</summary>"}, ContextWindow: 1024, ReserveTokens: 128, KeepRecentTokens: 1,
				})),
			})
			executor := agentgo.NewModelExecutor(agentgo.ModelExecutorConfig{Middlewares: []agentgo.ModelMiddleware{
				func(ctx context.Context, call agentgo.ModelExecution, next agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
					if call.ParentID != "" {
						summaryCalls++
						if call.Artifacts == nil || call.Artifacts == live {
							return agentgo.ModelResult{}, errors.New("summary lost staged artifacts")
						}
						if err := call.Artifacts.AddArtifact(runtimeArtifact{"summary", "candidate"}, false); err != nil {
							return agentgo.ModelResult{}, err
						}
					}
					return next(ctx, call)
				},
			}})
			for event := range agentgo.AgentLoop(t.Context(), []agentgo.AgentMessage{agentgo.UserMsg(strings.Repeat("old history ", 1500)), agentgo.UserMsg("latest")}, agentgo.AgentContext{}, agentgo.LoopConfig{
				Model: executionTestModel{reply: "done"}, ModelExecutor: executor, ContextManager: engine,
				BeforeTurn: func(_ context.Context, turn agentgo.BeforeTurnContext) ([]agentgo.AgentMessage, error) {
					live = turn.Artifacts
					return nil, nil
				},
				CommitContext: func(candidate agentgo.ContextCommitResult) error {
					commits++
					if artifactMap(candidate.Artifacts)["summary"] != "candidate" {
						t.Error("candidate material missing")
					}
					if _, ok := live.GetArtifact("summary"); ok {
						t.Error("material published before commit")
					}
					if reject {
						return failed
					}
					return nil
				},
			}) {
				if event.Type == agentgo.EventAgentEnd {
					final = event.State
					if reject && !errors.Is(event.Err, failed) {
						t.Errorf("error=%v", event.Err)
					}
				}
			}
			if final == nil || summaryCalls != 1 || commits != 1 {
				t.Fatalf("final=%v summary=%d commits=%d", final, summaryCalls, commits)
			}
			_, published := artifactMap(final.Artifacts)["summary"]
			if published == reject {
				t.Fatalf("published=%v reject=%v", published, reject)
			}
		})
	}
}
