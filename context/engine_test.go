package context

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/compforge/agentgo"
)

type failingCompactor struct{ callCount int }

func (c *failingCompactor) Compact(context.Context, []agentgo.AgentMessage, float64) ([]agentgo.AgentMessage, error) {
	c.callCount++
	return nil, fmt.Errorf("simulated failure")
}

type recordingCompactor struct {
	expects []float64
	result  []agentgo.AgentMessage
}

func (c *recordingCompactor) Compact(_ context.Context, messages []agentgo.AgentMessage, expect float64) ([]agentgo.AgentMessage, error) {
	c.expects = append(c.expects, expect)
	if c.result != nil {
		return copyMessages(c.result), nil
	}
	return copyMessages(messages), nil
}

type replacingCompactor struct {
	text  string
	calls int
}

func (c *replacingCompactor) Compact(_ context.Context, _ []agentgo.AgentMessage, _ float64) ([]agentgo.AgentMessage, error) {
	c.calls++
	return []agentgo.AgentMessage{agentgo.UserMsg(c.text)}, nil
}

func trimCompactor() Compactor {
	return NewLightTrimCompactor(LightTrimConfig{
		KeepRecent:    1,
		TextThreshold: 100,
		PreserveHead:  20,
		PreserveTail:  10,
	})
}

func TestCompactorChainStopsAtRequestedRatio(t *testing.T) {
	first := &replacingCompactor{text: strings.Repeat("x", 2000)}
	second := &replacingCompactor{text: "terminal"}
	chain := Chain(first, second)
	input := []agentgo.AgentMessage{agentgo.UserMsg(strings.Repeat("x", 4000))}

	if _, err := chain.Compact(t.Context(), input, 0.6); err != nil {
		t.Fatal(err)
	}
	if first.calls != 1 || second.calls != 0 {
		t.Fatalf("calls = (%d, %d), want (1, 0)", first.calls, second.calls)
	}

	if _, err := chain.Compact(t.Context(), input, 0.25); err != nil {
		t.Fatal(err)
	}
	if first.calls != 2 || second.calls != 1 {
		t.Fatalf("calls = (%d, %d), want (2, 1)", first.calls, second.calls)
	}
}

func TestContextEngineProjectUsesAggregateRatioAndTracksUsage(t *testing.T) {
	engine := NewEngine(EngineConfig{ContextWindow: 1024, Compactor: trimCompactor()})
	msgs := []agentgo.AgentMessage{
		agentgo.UserMsg(strings.Repeat("a", 800)),
		agentgo.UserMsg("recent"),
	}
	rawFirst := msgs[0].TextContent()

	proj, err := engine.Project(t.Context(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if proj.Messages[0].TextContent() == rawFirst {
		t.Fatal("expected the projected view to be trimmed")
	}
	if proj.Compaction == nil {
		t.Fatal("expected compaction details")
	}
	if got := proj.Compaction; got.Reason != agentgo.CompactReasonThreshold || got.Committed || got.TokensAfter >= got.TokensBefore || got.MessagesBefore != 2 || got.MessagesAfter != 2 || got.Summarized {
		t.Fatalf("unexpected compaction details: %+v", got)
	}
	if msgs[0].TextContent() != rawFirst {
		t.Fatal("project mutated the original message")
	}
	if got := proj.Messages[0].Raw().TextContent(); got != rawFirst {
		t.Fatalf("raw text = %q, want original content", got)
	}
	usage := engine.Usage()
	if usage == nil || usage.Tokens != EstimateContextTokens(proj.Messages).Tokens {
		t.Fatalf("usage = %+v, want projected usage", usage)
	}
	snapshot := engine.Snapshot()
	if snapshot == nil || snapshot.BaselineUsage == nil {
		t.Fatal("expected baseline usage")
	}
	if snapshot.BaselineUsage.Tokens != EstimateContextTokens(msgs).Tokens {
		t.Fatal("baseline usage should describe the raw transcript")
	}
}

func TestContextEngineProjectCanCommitCompactedMessages(t *testing.T) {
	engine := NewEngine(EngineConfig{ContextWindow: 1024, Compactor: trimCompactor(), CommitOnProject: true})
	msgs := []agentgo.AgentMessage{
		agentgo.UserMsg(strings.Repeat("a", 800)),
		agentgo.UserMsg("recent"),
	}
	proj, err := engine.Project(t.Context(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !proj.ShouldCommit || len(proj.CommitMessages) != len(proj.Messages) {
		t.Fatalf("projection did not request a matching commit: %+v", proj)
	}
	if proj.Compaction == nil || !proj.Compaction.Committed {
		t.Fatalf("expected committed compaction details: %+v", proj.Compaction)
	}
	if proj.CommitMessages[0].Raw().TextContent() != msgs[0].TextContent() {
		t.Fatal("committed projection lost its raw message")
	}
}

func TestContextEngineProjectBelowThresholdHasNoCompaction(t *testing.T) {
	engine := NewEngine(EngineConfig{ContextWindow: 128_000, Compactor: trimCompactor()})
	proj, err := engine.Project(t.Context(), []agentgo.AgentMessage{agentgo.UserMsg("small")})
	if err != nil {
		t.Fatal(err)
	}
	if proj.Compaction != nil {
		t.Fatalf("unexpected compaction below threshold: %+v", proj.Compaction)
	}
}

func TestContextEnginePassesCalculatedAndForcedRatios(t *testing.T) {
	compactor := &recordingCompactor{}
	engine := NewEngine(EngineConfig{ContextWindow: 100, ReserveTokens: 20, Compactor: compactor})
	msgs := []agentgo.AgentMessage{agentgo.UserMsg(strings.Repeat("x", 800))}
	before := EstimateContextTokens(msgs).Tokens

	if _, err := engine.Project(t.Context(), msgs); err != nil {
		t.Fatal(err)
	}
	want := float64(80) / float64(before)
	if len(compactor.expects) != 1 || compactor.expects[0] != want {
		t.Fatalf("project expects = %v, want [%f]", compactor.expects, want)
	}
	if _, err := engine.Compact(t.Context(), msgs, agentgo.CompactReasonManual); err != nil {
		t.Fatal(err)
	}
	if got := compactor.expects[len(compactor.expects)-1]; got != 0 {
		t.Fatalf("manual compact expect = %f, want 0", got)
	}
	if _, err := engine.RecoverOverflow(t.Context(), msgs, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	if got := compactor.expects[len(compactor.expects)-1]; got != 0 {
		t.Fatalf("overflow expect = %f, want 0", got)
	}
}

func TestContextEngineForcedCompactionReportsReason(t *testing.T) {
	msgs := []agentgo.AgentMessage{agentgo.UserMsg(strings.Repeat("x", 800))}

	manualEngine := NewEngine(EngineConfig{ContextWindow: 1024, Compactor: &replacingCompactor{text: "small"}})
	manual, err := manualEngine.Compact(t.Context(), msgs, agentgo.CompactReasonManual)
	if err != nil {
		t.Fatal(err)
	}
	if manual.Compaction == nil || manual.Compaction.Reason != agentgo.CompactReasonManual || !manual.Compaction.Committed {
		t.Fatalf("unexpected manual compaction details: %+v", manual.Compaction)
	}

	recoveryEngine := NewEngine(EngineConfig{ContextWindow: 1024, Compactor: &replacingCompactor{text: "small"}})
	recovery, err := recoveryEngine.RecoverOverflow(t.Context(), msgs, context.DeadlineExceeded)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Compaction == nil || recovery.Compaction.Reason != agentgo.CompactReasonOverflow || !recovery.Compaction.Committed {
		t.Fatalf("unexpected recovery compaction details: %+v", recovery.Compaction)
	}
}

func TestContextEngineCompactProducesSummaryAndRetainsRaw(t *testing.T) {
	var maxTokens int
	model := stubModel{generate: func(_ context.Context, _ []agentgo.Message, _ []agentgo.ToolSpec, opts ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
		// Forced compaction must still set a positive, bounded summary budget;
		// MaxTokens=0 would silently restore the provider default.
		maxTokens = agentgo.ResolveCallConfig(opts).MaxTokens
		return &agentgo.LLMResponse{Message: agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("<summary>summary body</summary>")}}}, nil
	}}
	summary := NewSummaryCompactor(FullSummaryConfig{
		Model: model, ContextWindow: 1024, ReserveTokens: 128, KeepRecentTokens: 1,
		PostSummaryHooks: []PostSummaryHook{
			func(context.Context, SummaryInfo, []agentgo.AgentMessage) ([]agentgo.AgentMessage, error) {
				return []agentgo.AgentMessage{agentgo.UserMsg("hook reminder")}, nil
			},
		},
	})
	engine := NewEngine(EngineConfig{ContextWindow: 1024, Compactor: summary})
	msgs := []agentgo.AgentMessage{agentgo.UserMsg(strings.Repeat("a", 4000)), agentgo.UserMsg("keep")}

	result, err := engine.Compact(t.Context(), msgs, agentgo.CompactReasonManual)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || len(result.Messages) < 3 {
		t.Fatalf("unexpected compact result: %+v", result)
	}
	if result.Compaction == nil || !result.Compaction.Summarized {
		t.Fatalf("expected summary compaction details: %+v", result.Compaction)
	}
	checkpoint, ok := result.Messages[0].(ContextSummary)
	if !ok || len(checkpoint.RawMessages) == 0 {
		t.Fatalf("summary did not retain compacted raw messages: %#v", result.Messages[0])
	}
	if result.Messages[1].TextContent() != "hook reminder" {
		t.Fatal("post-summary hook was not inserted")
	}
	if maxTokens <= 0 || maxTokens >= minSummaryReserveTokens {
		t.Fatalf("forced summary max tokens = %d, want a positive bounded budget", maxTokens)
	}
}

func TestSummaryCompactorKeepsCompactedRecentView(t *testing.T) {
	model := stubModel{generate: func(context.Context, []agentgo.Message, []agentgo.ToolSpec, ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
		return &agentgo.LLMResponse{Message: agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("<summary>old history</summary>")}}}, nil
	}}
	recent := stagedMessage{contents: []string{strings.Repeat("r", 8000), "recent-ref"}}
	compactor := Chain(
		NewMessageCompactor(),
		NewSummaryCompactor(FullSummaryConfig{Model: model, KeepRecentTokens: 1}),
	)
	messages := []agentgo.AgentMessage{
		agentgo.UserMsg(strings.Repeat("old", 4000)),
		recent,
	}

	out, err := compactor.Compact(t.Context(), messages, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("messages = %d, want summary plus recent message", len(out))
	}
	kept, ok := out[1].(stagedMessage)
	if !ok || kept.stage != 1 {
		t.Fatalf("recent message = %#v, want compacted stage", out[1])
	}
	if raw := kept.Raw().(stagedMessage); raw.stage != 0 {
		t.Fatalf("recent raw stage = %d, want 0", raw.stage)
	}
}

func TestSummaryCompactorSeesRawToolEvidence(t *testing.T) {
	var sawPlaceholder, sawRaw bool
	model := stubModel{generate: func(_ context.Context, messages []agentgo.Message, _ []agentgo.ToolSpec, _ ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
		for _, message := range messages {
			text := message.TextContent()
			sawPlaceholder = sawPlaceholder || strings.Contains(text, DefaultClearedToolResult)
			sawRaw = sawRaw || strings.Contains(text, "VERY_IMPORTANT_TOOL_RESULT")
		}
		return &agentgo.LLMResponse{Message: agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("<summary>summary body</summary>")}}}, nil
	}}
	compactor := Chain(
		NewToolResultCompactor(ToolResultMicrocompactConfig{KeepRecent: 1}),
		NewSummaryCompactor(FullSummaryConfig{Model: model, KeepRecentTokens: 1}),
	)
	msgs := []agentgo.AgentMessage{
		agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{
			agentgo.ToolCallBlock(agentgo.ToolCall{ID: "tc1", Name: "read", Args: []byte(`{"path":"main.go"}`)}),
			agentgo.ToolCallBlock(agentgo.ToolCall{ID: "tc2", Name: "read", Args: []byte(`{"path":"other.go"}`)}),
		}},
		agentgo.ToolResultMsg("tc1", []byte(`"VERY_IMPORTANT_TOOL_RESULT"`), false),
		agentgo.ToolResultMsg("tc2", []byte(`"recent"`), false),
		agentgo.UserMsg("keep"),
	}
	if _, err := compactor.Compact(t.Context(), msgs, 0); err != nil {
		t.Fatal(err)
	}
	if sawPlaceholder || !sawRaw {
		t.Fatalf("summary input placeholder=%v raw=%v", sawPlaceholder, sawRaw)
	}
}

func TestToolResultCompactorDeduplicatesIdenticalCalls(t *testing.T) {
	build := func(n int, argsFor func(int) string) []agentgo.AgentMessage {
		msgs := make([]agentgo.AgentMessage, 0, 2*n)
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("tc%d", i)
			msgs = append(msgs,
				agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{
					agentgo.ToolCallBlock(agentgo.ToolCall{ID: id, Name: "novel_context", Args: []byte(argsFor(i))}),
				}},
				agentgo.ToolResultMsg(id, []byte(fmt.Sprintf(`"RESULT_%d_%s"`, i, strings.Repeat("x", 80))), false),
			)
		}
		return msgs
	}
	cleared := func(msgs []agentgo.AgentMessage) []int {
		var indexes []int
		for i, message := range msgs {
			model, include := message.ToMessage()
			if include && model.Role == agentgo.RoleTool && strings.Contains(model.TextContent(), DefaultClearedToolResult) {
				indexes = append(indexes, i)
			}
		}
		return indexes
	}
	compactor := NewToolResultCompactor(ToolResultMicrocompactConfig{KeepRecent: 5})

	t.Run("identical", func(t *testing.T) {
		msgs := build(6, func(int) string { return `{"chapter":119}` })
		out, err := compactor.Compact(t.Context(), msgs, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := cleared(out), []int{1, 3, 5, 7, 9}; !slices.Equal(got, want) {
			t.Fatalf("cleared = %v, want %v", got, want)
		}
		if got := out[1].Raw().TextContent(); !strings.Contains(got, "RESULT_0") {
			t.Fatalf("raw result was lost: %q", got)
		}
	})

	t.Run("distinct", func(t *testing.T) {
		msgs := build(6, func(i int) string { return fmt.Sprintf(`{"chapter":%d}`, i) })
		out, err := compactor.Compact(t.Context(), msgs, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := cleared(out), []int{1}; !slices.Equal(got, want) {
			t.Fatalf("cleared = %v, want %v", got, want)
		}
	})
}

func TestContextEngineSnapshotAndSync(t *testing.T) {
	engine := NewEngine(EngineConfig{ContextWindow: 1024, Compactor: trimCompactor()})
	msgs := []agentgo.AgentMessage{agentgo.UserMsg(strings.Repeat("a", 800)), agentgo.UserMsg("recent")}
	if _, err := engine.Project(t.Context(), msgs); err != nil {
		t.Fatal(err)
	}
	snapshot := engine.Snapshot()
	if snapshot == nil || snapshot.Scope != "projected" || !snapshot.LastChanged || snapshot.TrimmedTextBlocks == 0 {
		t.Fatalf("unexpected projected snapshot: %+v", snapshot)
	}
	engine.Sync(msgs)
	if snapshot = engine.Snapshot(); snapshot == nil || snapshot.Scope != "baseline" {
		t.Fatalf("unexpected baseline snapshot: %+v", snapshot)
	}
}

func TestContextSummaryToMessage(t *testing.T) {
	summary := ContextSummary{Summary: "summary body", TokensBefore: 42, ReadFiles: []string{"a.go"}}
	out := agentgo.ToMessages([]agentgo.AgentMessage{summary, agentgo.UserMsg("keep")})
	if len(out) != 2 || !strings.Contains(out[0].TextContent(), "<context-summary>\nsummary body\n</context-summary>") {
		t.Fatalf("unexpected messages: %+v", out)
	}
	if out[0].Metadata["type"] != "context_summary" {
		t.Fatal("missing context summary marker")
	}
}

func TestCircuitBreakerTripsAndRetries(t *testing.T) {
	compactor := &failingCompactor{}
	var event RewriteEvent
	engine := NewEngine(EngineConfig{
		ContextWindow: 100, ReserveTokens: 1, Compactor: compactor, MaxConsecutiveFailures: 2,
		OnProject: func(ev RewriteEvent) { event = ev },
	})
	msgs := []agentgo.AgentMessage{agentgo.UserMsg(strings.Repeat("x", 500))}
	for range 2 {
		if _, err := engine.Project(t.Context(), msgs); err == nil {
			t.Fatal("expected compactor failure")
		}
	}
	if _, err := engine.Project(t.Context(), msgs); err != nil {
		t.Fatalf("breaker should skip one cycle: %v", err)
	}
	if event.Reason != "circuit_breaker" || event.Failures != 2 || event.Changed {
		t.Fatalf("unexpected breaker event: %+v", event)
	}
	if _, err := engine.Project(t.Context(), msgs); err == nil {
		t.Fatal("breaker should retry after the skipped cycle")
	}
	if compactor.callCount != 3 {
		t.Fatalf("compactor calls = %d, want 3", compactor.callCount)
	}
}

func TestCircuitBreakerResetsAfterSuccessfulRewrite(t *testing.T) {
	engine := NewEngine(EngineConfig{ContextWindow: 64, ReserveTokens: 1, Compactor: trimCompactor()})
	engine.consecutiveFailures = 2
	msgs := []agentgo.AgentMessage{agentgo.UserMsg(strings.Repeat("a", 800)), agentgo.UserMsg("recent")}
	if _, err := engine.Project(t.Context(), msgs); err != nil {
		t.Fatal(err)
	}
	if engine.ConsecutiveFailures() != 0 {
		t.Fatalf("failure count = %d, want 0", engine.ConsecutiveFailures())
	}
}

func TestDefaultReserveScalesWithWindow(t *testing.T) {
	for _, window := range []int{8_000, 16_000, 32_000, 128_000, 200_000} {
		engine := NewEngine(EngineConfig{ContextWindow: window})
		got := engine.threshold(window)
		if got <= 0 || got >= window {
			t.Errorf("window %d: threshold %d", window, got)
		}
	}
	const large = 200_000
	if got := NewEngine(EngineConfig{ContextWindow: large}).threshold(large); got != large-maxEngineReserveTokens {
		t.Fatalf("threshold = %d, want %d", got, large-maxEngineReserveTokens)
	}
}

func TestSetReserveTokensZeroRestoresDefault(t *testing.T) {
	engine := NewEngine(EngineConfig{ContextWindow: 128_000, ReserveTokens: 1_000})
	if got := engine.threshold(128_000); got != 127_000 {
		t.Fatalf("explicit threshold = %d", got)
	}
	engine.SetReserveTokens(0)
	if got := engine.threshold(128_000); got != 128_000-maxEngineReserveTokens {
		t.Fatalf("default threshold = %d", got)
	}
}
