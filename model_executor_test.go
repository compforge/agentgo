package agentgo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type executorStreamModel func(context.Context, []Message, ...CallOption) (<-chan StreamEvent, error)

func (m executorStreamModel) Generate(context.Context, []Message, []ToolSpec, ...CallOption) (*LLMResponse, error) {
	panic("executor must use streaming")
}
func (m executorStreamModel) GenerateStream(ctx context.Context, messages []Message, _ []ToolSpec, opts ...CallOption) (<-chan StreamEvent, error) {
	return m(ctx, messages, opts...)
}
func (m executorStreamModel) SupportsTools() bool { return true }

// +case:id=shared_executor_run_isolation,desc=`Two Loops share one executor concurrently`,expect=`Each run receives only its own stream and execution facts; observer sees both`
func TestModelExecutorConcurrentRuns(t *testing.T) {
	type key struct{}
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	var mu sync.Mutex
	observed := map[string]int{}
	executor := NewModelExecutor(ModelExecutorConfig{
		Middlewares: []ModelMiddleware{func(ctx context.Context, call ModelExecution, next ModelExecuteFunc) (ModelResult, error) {
			call.Request.Config.PromptCacheKey = ctx.Value(key{}).(string)
			return next(ctx, call)
		}},
		OnEvent: func(event Event) {
			if event.Type == EventModelExecEnd {
				mu.Lock()
				observed[event.Message.TextContent()]++
				mu.Unlock()
			}
		},
	})
	model := executorStreamModel(func(ctx context.Context, _ []Message, opts ...CallOption) (<-chan StreamEvent, error) {
		value := ctx.Value(key{}).(string)
		if ResolveCallConfig(opts).PromptCacheKey != value {
			t.Error("call config crossed runs")
		}
		ready <- struct{}{}
		<-release
		ReportModelAttempt(ctx, ModelAttempt{Index: 1, Endpoint: value}, nil)
		ch := make(chan StreamEvent, 2)
		message := assistantMsg(value, StopReasonStop)
		ch <- StreamEvent{Type: StreamEventTextDelta, Message: message, Delta: value}
		ch <- StreamEvent{Type: StreamEventDone, Message: message}
		close(ch)
		return ch, nil
	})
	var wg sync.WaitGroup
	for _, value := range []string{"a", "b"} {
		wg.Go(func() {
			ctx := context.WithValue(t.Context(), key{}, value)
			counts := map[EventType]int{}
			for event := range AgentLoop(ctx, []AgentMessage{UserMsg(value)}, AgentContext{}, LoopConfig{Model: model, ModelExecutor: executor}) {
				counts[event.Type]++
				if event.Type == EventModelStream && event.ModelStream.Delta != value {
					t.Error("stream crossed runs")
				}
				if event.Type == EventModelAttempt && event.ModelAttempt.Endpoint != value {
					t.Error("attempt crossed runs")
				}
				if event.Type == EventMessageUpdate && event.Delta != value {
					t.Error("conversation progress crossed runs")
				}
				if event.Type == EventModelExecEnd && event.Message.TextContent() != value {
					t.Error("result crossed runs")
				}
			}
			for _, kind := range []EventType{EventModelExecStart, EventModelExecEnd, EventModelStream, EventModelAttempt, EventMessageUpdate} {
				if counts[kind] != 1 {
					t.Errorf("run %s: %s count=%d", value, kind, counts[kind])
				}
			}
		})
	}
	for range 2 {
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("calls did not overlap")
		}
	}
	close(release)
	wg.Wait()
	if observed["a"] != 1 || observed["b"] != 1 {
		t.Fatalf("observer facts: %v", observed)
	}
}

func TestModelExecutorCancellationRetainsPartial(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	partial := assistantMsg("partial", "")
	partial.Usage = &Usage{Output: 7, Reasoning: 3}
	model := executorStreamModel(func(context.Context, []Message, ...CallOption) (<-chan StreamEvent, error) {
		ch := make(chan StreamEvent, 1)
		ch <- StreamEvent{Type: StreamEventTextDelta, Message: partial, Delta: "partial"}
		return ch, nil // deliberately uncooperative producer; caller must still return
	})
	var ended Event
	executor := NewModelExecutor(ModelExecutorConfig{OnEvent: func(event Event) {
		if event.Type == EventModelStream {
			cancel()
		}
		if event.Type == EventModelExecEnd {
			ended = event
		}
	}})
	result, err := executor.Call(ctx, model, newModelExecution(1, 1))
	if !errors.Is(err, context.Canceled) || result.Message.TextContent() != "partial" || result.Message.Usage.Reasoning != 3 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !errors.Is(ended.Err, context.Canceled) || ended.Message.TextContent() != "partial" {
		t.Fatalf("end=%+v", ended)
	}
}

func TestModelExecutorLengthIsCallerPolicy(t *testing.T) {
	calls := 0
	model := executorStreamModel(func(context.Context, []Message, ...CallOption) (<-chan StreamEvent, error) {
		calls++
		ch := make(chan StreamEvent, 1)
		ch <- StreamEvent{Type: StreamEventDone, Message: assistantMsg("limited", StopReasonLength)}
		close(ch)
		return ch, nil
	})
	var executor *ModelExecutor
	result, err := executor.Call(t.Context(), model, newModelExecution(1, 1))
	if err != nil || calls != 1 || result.Message.StopReason != StopReasonLength {
		t.Fatalf("calls=%d result=%+v err=%v", calls, result, err)
	}
}

func TestModelExecutorIgnoresLateAttemptAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release := make(chan struct{})
	reported := make(chan struct{})
	model := executorStreamModel(func(ctx context.Context, _ []Message, _ ...CallOption) (<-chan StreamEvent, error) {
		go func() {
			<-release
			ReportModelAttempt(ctx, ModelAttempt{Index: 1, Endpoint: "late"}, context.Canceled)
			close(reported)
		}()
		ch := make(chan StreamEvent, 1)
		ch <- StreamEvent{Type: StreamEventTextDelta, Message: assistantMsg("partial", "")}
		return ch, nil
	})
	var events []EventType
	executor := NewModelExecutor(ModelExecutorConfig{OnEvent: func(event Event) {
		events = append(events, event.Type)
		if event.Type == EventModelStream {
			cancel()
		}
	}})
	_, err := executor.Call(ctx, model, newModelExecution(1, 1))
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	count := len(events)
	close(release)
	<-reported
	if len(events) != count || events[count-1] != EventModelExecEnd {
		t.Fatalf("late events: %v", events)
	}
}
