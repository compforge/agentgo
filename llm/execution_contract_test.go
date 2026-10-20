package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/compforge/agentgo"
	"github.com/compforge/agentgo/session"
	"github.com/voocel/litellm"
)

// +case:id=model_failure_evidence_pipeline,desc=`Direct and streamed calls fail with reasoning, tool content, opaque state and usage`,expect=`One execution and routed attempt are recorded; failed content never commits or invokes tools`
func TestFailedModelExecutionEvidence(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		name := "direct"
		if streamed {
			name = "loop"
		}
		t.Run(name, func(t *testing.T) {
			state := &litellm.ProviderState{Provider: "capture", Model: "test", Data: json.RawMessage(`{"signature":"opaque"}`)}
			usage := litellm.Usage{InputTokens: 5, OutputTokens: 7, ReasoningTokens: 3}
			cause := errors.New("provider stream failed")
			var model agentgo.ChatModel
			var request func() *litellm.Request
			if streamed {
				provider := &captureStreamProvider{events: []litellm.Event{
					litellm.BlockStart{Index: 0, Block: litellm.ReasoningBlock{State: state}},
					litellm.ReasoningDelta{Index: 0, Text: "partial reasoning"}, litellm.BlockEnd{Index: 0},
					litellm.BlockStart{Index: 1, Block: litellm.TextBlock{}}, litellm.TextDelta{Index: 1, Text: "partial output"}, litellm.BlockEnd{Index: 1},
					litellm.BlockStart{Index: 2, Block: litellm.ToolUseBlock{ID: "call", Name: "inspect"}}, litellm.ToolUseDelta{Index: 2, Arguments: `{}`}, litellm.BlockEnd{Index: 2},
					litellm.UsageEvent{Usage: usage}, // EOF before Done must remain a failure.
				}}
				model = NewLiteLLMAdapter("test", mustClient(t, provider))
				request = func() *litellm.Request { return provider.lastReq }
			} else {
				provider := &captureProvider{chatFunc: func(context.Context, *litellm.Request) (*litellm.Response, error) {
					return &litellm.Response{Provider: "capture", Model: "test", Usage: usage, Blocks: []litellm.Block{
						litellm.ReasoningBlock{Text: "partial reasoning", State: state}, litellm.TextBlock{Text: "partial output"}, litellm.ToolUseBlock{ID: "call", Name: "inspect", Arguments: `{}`},
					}}, cause
				}}
				model = NewLiteLLMAdapter("test", mustClient(t, provider))
				request = func() *litellm.Request { return provider.lastReq }
			}
			router, err := NewRouter([]Route{{Name: "primary", Model: model}}, RouterConfig{})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "model.jsonl")
			recording, err := session.Create(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer recording.Close()
			run, err := recording.BeginRun()
			if err != nil {
				t.Fatal(err)
			}
			var events []agentgo.Event
			observe := func(event agentgo.Event) {
				events = append(events, event)
				if err := run.Record(event); err != nil {
					t.Error(err)
				}
			}
			middlewareCalls := 0
			middleware := func(ctx context.Context, call agentgo.ModelExecution, next agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
				middlewareCalls++
				call.Request.Config.ThinkingLevel = agentgo.ThinkingMedium
				call.Request.Config.MaxTokens = 321
				return next(ctx, call)
			}
			if streamed {
				tool := agentgo.NewFuncTool("inspect", "inspect", nil, func(context.Context, json.RawMessage) (json.RawMessage, error) {
					t.Error("failed tool content executed")
					return nil, nil
				})
				for event := range agentgo.AgentLoop(t.Context(), []agentgo.AgentMessage{agentgo.UserMsg("test")}, agentgo.AgentContext{Tools: []agentgo.Tool{tool}}, agentgo.LoopConfig{Model: router, MaxRetries: 3, ModelMiddlewares: []agentgo.ModelMiddleware{middleware}}) {
					observe(event)
				}
			} else {
				ctx := agentgo.WithModelRuntime(t.Context(), []agentgo.ModelMiddleware{middleware}, observe)
				ctx = agentgo.ContextWithExecution(ctx, agentgo.Execution{ID: "host"})
				result, err := agentgo.GenerateModel(ctx, router, agentgo.ModelExecution{
					Execution: agentgo.Execution{ID: "one-shot", Kind: agentgo.ExecutionKindModel, Attempt: 1},
					Request:   agentgo.LLMRequest{Messages: []agentgo.Message{agentgo.UserMsg("test")}},
				})
				if !errors.Is(err, cause) || result.Message.TextContent() != "partial output" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			}
			counts := map[agentgo.EventType]int{}
			for _, event := range events {
				counts[event.Type]++
				if event.Timestamp.IsZero() {
					t.Fatalf("missing event time: %s", event.Type)
				}
				switch event.Type {
				case agentgo.EventModelExecEnd:
					message := event.Message.(agentgo.Message)
					if event.Err == nil || message.TextContent() != "partial output" || message.Usage == nil || message.Usage.Output != 7 || message.Usage.Reasoning != 3 || message.Content[0].ProviderState == nil || string(message.Content[0].ProviderState.Data) != `{"signature":"opaque"}` || message.StopReason == agentgo.StopReasonStop {
						t.Fatalf("lost failure evidence: %+v err=%v", message, event.Err)
					}
					if !streamed && event.Execution.ParentID != "host" {
						t.Fatalf("missing parent: %+v", event.Execution)
					}
				case agentgo.EventModelAttempt:
					if event.ModelAttempt.Usage == nil || event.ModelAttempt.Usage.Output != 7 || event.ModelAttempt.WillFailover {
						t.Fatalf("attempt=%+v", event.ModelAttempt)
					}
				case agentgo.EventMessageEnd:
					if event.Message.GetRole() == agentgo.RoleAssistant {
						t.Fatal("failed message committed")
					}
				}
			}
			for _, kind := range []agentgo.EventType{agentgo.EventModelExecStart, agentgo.EventModelRequest, agentgo.EventModelAttempt, agentgo.EventModelExecEnd} {
				if counts[kind] != 1 {
					t.Fatalf("counts=%v", counts)
				}
			}
			if middlewareCalls != 1 || counts[agentgo.EventToolInvokeStart] != 0 || counts[agentgo.EventRetry] != 0 {
				t.Fatalf("middleware=%d events=%v", middlewareCalls, counts)
			}
			if request().MaxTokens == nil || *request().MaxTokens != 321 || request().Thinking == nil || string(request().Thinking.Effort) != string(agentgo.ThinkingMedium) {
				t.Fatalf("provider request=%+v", request())
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := recording.LoadCheckpoint(); err != nil || ok {
				t.Fatalf("failed execution checkpoint: %v %v", ok, err)
			}
			if err := recording.Close(); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			codec, err := agentgo.NewCodec()
			if err != nil {
				t.Fatal(err)
			}
			persisted := 0
			_, err = session.Read(file, func(record session.Record) error {
				if record.Type != string(agentgo.EventModelExecEnd) {
					return nil
				}
				persisted++
				var data session.EventData
				if err := json.Unmarshal(record.Data, &data); err != nil {
					return err
				}
				var partial agentgo.Message
				if err := codec.Unmarshal(data.PartialMessage, &partial); err != nil {
					return err
				}
				if partial.TextContent() != "partial output" || partial.Usage == nil || partial.Usage.Output != 7 || data.Error == "" || len(data.Message) > 0 {
					t.Fatalf("persisted failure=%+v partial=%+v", data, partial)
				}
				return nil
			})
			if err != nil || persisted != 1 {
				t.Fatalf("read records=%d err=%v", persisted, err)
			}
		})
	}
}
