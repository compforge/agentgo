package agentgo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/compforge/agentgo"
	agentcontext "github.com/compforge/agentgo/context"
)

type executionTestModel struct {
	reply string
}

func (m executionTestModel) Generate(context.Context, []agentgo.Message, []agentgo.ToolSpec, ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
	return &agentgo.LLMResponse{Message: agentgo.Message{
		Role:       agentgo.RoleAssistant,
		Content:    []agentgo.ContentBlock{agentgo.TextBlock(m.reply)},
		StopReason: agentgo.StopReasonStop,
	}}, nil
}

func (m executionTestModel) GenerateStream(context.Context, []agentgo.Message, []agentgo.ToolSpec, ...agentgo.CallOption) (<-chan agentgo.StreamEvent, error) {
	message := agentgo.Message{
		Role:       agentgo.RoleAssistant,
		Content:    []agentgo.ContentBlock{agentgo.TextBlock(m.reply)},
		StopReason: agentgo.StopReasonStop,
	}
	events := make(chan agentgo.StreamEvent, 1)
	events <- agentgo.StreamEvent{Type: agentgo.StreamEventDone, Message: message, StopReason: message.StopReason}
	close(events)
	return events, nil
}

func (m executionTestModel) SupportsTools() bool { return true }

func TestAgentLoopModelMiddlewareCoversContextSummaryExecution(t *testing.T) {
	summaryModel := executionTestModel{reply: "<summary>compacted history</summary>"}
	mainModel := executionTestModel{reply: "done"}
	manager := agentcontext.NewEngine(agentcontext.EngineConfig{
		ContextWindow:   1024,
		ReserveTokens:   128,
		CommitOnProject: true,
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
			ModelMiddlewares: []agentgo.ModelMiddleware{
				func(ctx context.Context, execution agentgo.ModelExecution, next agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
					middlewareExecutions = append(middlewareExecutions, execution.Execution)
					return next(ctx, execution)
				},
			},
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
	for i := range observed {
		event := &observed[i]
		if event.Type == agentgo.EventContextCompacted {
			compactEvent = event
		}
		if (event.Type == agentgo.EventModelExecStart || event.Type == agentgo.EventModelExecEnd) && event.Execution != nil {
			modelEvents[event.Execution.ID]++
		}
	}
	if compactEvent == nil || compactEvent.Execution == nil || compactEvent.Execution.ID != "compact-1-threshold" || compactEvent.Execution.Kind != agentgo.ExecutionKindCompact {
		t.Fatalf("compaction event = %#v", compactEvent)
	}
	if modelEvents[summaryExecution.ID] != 2 || modelEvents[mainExecution.ID] != 2 {
		t.Fatalf("model execution events = %#v, want start/end for summary and main", modelEvents)
	}
}
