package context

import (
	"context"
	"strings"
	"testing"

	"github.com/compforge/agentgo"
)

type stubModel struct {
	generate func(ctx context.Context, messages []agentgo.Message, tools []agentgo.ToolSpec, opts ...agentgo.CallOption) (*agentgo.LLMResponse, error)
}

func (m stubModel) Generate(ctx context.Context, messages []agentgo.Message, tools []agentgo.ToolSpec, opts ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
	return m.generate(ctx, messages, tools, opts...)
}

func (m stubModel) GenerateStream(ctx context.Context, messages []agentgo.Message, tools []agentgo.ToolSpec, opts ...agentgo.CallOption) (<-chan agentgo.StreamEvent, error) {
	return nil, context.Canceled
}

func (m stubModel) SupportsTools() bool { return true }

func TestFindCutPoint_SkipsToolResultBoundary(t *testing.T) {
	msgs := []agentgo.AgentMessage{
		agentgo.UserMsg("old"),
		agentgo.Message{
			Role:    agentgo.RoleAssistant,
			Content: []agentgo.ContentBlock{agentgo.ToolCallBlock(agentgo.ToolCall{ID: "tc1", Name: "read"})},
		},
		agentgo.ToolResultMsg("tc1", []byte(`"ok"`), false),
		agentgo.UserMsg("recent"),
	}

	cut := findCutPoint(msgs, 2)
	if cut.firstKeptIndex != 3 {
		t.Fatalf("expected cut to advance past tool result to index 3, got %d", cut.firstKeptIndex)
	}
	if cut.isSplitTurn {
		t.Fatal("expected cut at user boundary, got split turn")
	}
}

func TestFindCutPoint_ReportsSplitTurn(t *testing.T) {
	msgs := []agentgo.AgentMessage{
		agentgo.UserMsg("old"),
		agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("done")}},
		agentgo.UserMsg("current task"),
		agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("working")}},
	}

	cut := findCutPoint(msgs, 1)
	if cut.firstKeptIndex != 3 {
		t.Fatalf("expected assistant message to be first kept item, got %d", cut.firstKeptIndex)
	}
	if !cut.isSplitTurn {
		t.Fatal("expected split turn to be reported")
	}
	if cut.turnStartIndex != 2 {
		t.Fatalf("expected split turn to start at index 2, got %d", cut.turnStartIndex)
	}
}

func TestExtractFileOps_DeduplicatesAndSeparates(t *testing.T) {
	msgs := []agentgo.AgentMessage{
		agentgo.Message{
			Role: agentgo.RoleAssistant,
			Content: []agentgo.ContentBlock{
				agentgo.ToolCallBlock(agentgo.ToolCall{ID: "1", Name: "read", Args: []byte(`{"path":"a.go"}`)}),
				agentgo.ToolCallBlock(agentgo.ToolCall{ID: "2", Name: "read", Args: []byte(`{"path":"b.go"}`)}),
				agentgo.ToolCallBlock(agentgo.ToolCall{ID: "3", Name: "edit", Args: []byte(`{"path":"b.go"}`)}),
				agentgo.ToolCallBlock(agentgo.ToolCall{ID: "4", Name: "write", Args: []byte(`{"path":"c.go"}`)}),
				agentgo.ToolCallBlock(agentgo.ToolCall{ID: "5", Name: "read", Args: []byte(`{"path":"a.go"}`)}),
			},
		},
	}

	readFiles, modifiedFiles := extractFileOps(msgs)
	if got := strings.Join(readFiles, ","); got != "a.go" {
		t.Fatalf("expected read-only files to be a.go, got %q", got)
	}
	if got := strings.Join(modifiedFiles, ","); got != "b.go,c.go" {
		t.Fatalf("expected modified files to be b.go,c.go, got %q", got)
	}
}

func TestRunSummaryCompaction_CompactsAndPreservesRecentMessages(t *testing.T) {
	model := stubModel{
		generate: func(ctx context.Context, messages []agentgo.Message, tools []agentgo.ToolSpec, opts ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
			return &agentgo.LLMResponse{
				Message: agentgo.Message{
					Role:    agentgo.RoleAssistant,
					Content: []agentgo.ContentBlock{agentgo.TextBlock("摘要内容")},
				},
			}, nil
		},
	}

	cfg := summaryRunConfig{
		Model:            model,
		ContextWindow:    16,
		ReserveTokens:    4,
		KeepRecentTokens: 1,
	}

	msgs := []agentgo.AgentMessage{
		agentgo.UserMsg(strings.Repeat("a", 80)),
		agentgo.Message{
			Role: agentgo.RoleAssistant,
			Content: []agentgo.ContentBlock{
				agentgo.ToolCallBlock(agentgo.ToolCall{ID: "1", Name: "read", Args: []byte(`{"path":"old.go"}`)}),
				agentgo.ToolCallBlock(agentgo.ToolCall{ID: "2", Name: "edit", Args: []byte(`{"path":"new.go"}`)}),
			},
		},
		agentgo.UserMsg("keep"),
	}

	out, info, err := runSummaryCompaction(context.Background(), cfg, msgs, true)
	if err != nil {
		t.Fatalf("unexpected compaction error: %v", err)
	}
	if info == nil {
		t.Fatal("expected compaction info")
	}
	if len(out) != 2 {
		t.Fatalf("expected compacted summary + recent message, got %d entries", len(out))
	}

	summary, ok := out[0].(ContextSummary)
	if !ok {
		t.Fatalf("expected first message to be ContextSummary, got %T", out[0])
	}
	if !strings.Contains(summary.Summary, "摘要内容") {
		t.Fatalf("expected generated summary content, got %q", summary.Summary)
	}
	if !strings.Contains(summary.Summary, "<read-files>\nold.go\n</read-files>") {
		t.Fatalf("expected read file section, got %q", summary.Summary)
	}
	if !strings.Contains(summary.Summary, "<modified-files>\nnew.go\n</modified-files>") {
		t.Fatalf("expected modified file section, got %q", summary.Summary)
	}
	if out[1].TextContent() != "keep" {
		t.Fatalf("expected recent message to be preserved, got %q", out[1].TextContent())
	}
}

func TestEstimateContextTokens_UsesLastAssistantUsage(t *testing.T) {
	msgs := []agentgo.AgentMessage{
		agentgo.UserMsg("before"),
		agentgo.Message{
			Role: agentgo.RoleAssistant,
			Content: []agentgo.ContentBlock{
				agentgo.TextBlock("done"),
			},
			Usage: &agentgo.Usage{TotalTokens: 100},
		},
		agentgo.UserMsg(strings.Repeat("x", 20)),
	}

	estimate := EstimateContextTokens(msgs)
	if estimate.UsageTokens != 100 {
		t.Fatalf("expected usage tokens=100, got %d", estimate.UsageTokens)
	}
	if estimate.TrailingTokens == 0 {
		t.Fatal("expected trailing tokens to be estimated")
	}
	if estimate.Tokens != estimate.UsageTokens+estimate.TrailingTokens {
		t.Fatalf("unexpected total tokens: %+v", estimate)
	}
}
