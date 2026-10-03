package context

import (
	"context"
	"github.com/compforge/agentgo"
	"strconv"
	"strings"
	"testing"
)

// Embedded domain messages must behave just like model messages at a cut.
type cutMessage struct{ agentgo.AgentMessage }

func TestFindCutPointKeepsWholeToolGroups(t *testing.T) {
	for _, domain := range []bool{false, true} {
		msgs := []agentgo.AgentMessage{agentgo.UserMsg("task")}
		for i := 1; i <= 3; i++ {
			msgs = append(msgs, agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{
				agentgo.ToolCallBlock(agentgo.ToolCall{ID: strconv.Itoa(i), Name: "read"}),
			}}, agentgo.ToolResultMsg(strconv.Itoa(i), []byte(strconv.Quote(strings.Repeat("a", 400))), false))
		}
		if domain {
			for i, m := range msgs {
				msgs[i] = cutMessage{m}
			}
		}
		cut := findCutPoint(msgs, 150)
		if cut.firstKeptIndex != 3 || !cut.isSplitTurn || cut.turnStartIndex != 0 {
			t.Fatalf("domain=%v cut=%+v; want groups 2 and 3 kept", domain, cut)
		}
		if cut := findCutPoint(msgs, 10000); cut.firstKeptIndex != 0 {
			t.Fatalf("oversized suffix: %+v", cut)
		}
		if cut := findCutPoint(msgs[5:], 50); cut.firstKeptIndex != 0 {
			t.Fatalf("only group cannot be split: %+v", cut)
		}
	}
}

func TestFindCutPointKeepsParallelResultsTogether(t *testing.T) {
	msgs := []agentgo.AgentMessage{agentgo.UserMsg("task"), agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{
		agentgo.ToolCallBlock(agentgo.ToolCall{ID: "a", Name: "read"}),
		agentgo.ToolCallBlock(agentgo.ToolCall{ID: "b", Name: "read"}),
	}}, agentgo.ToolResultMsg("a", []byte(strconv.Quote(strings.Repeat("a", 400))), false),
		agentgo.ToolResultMsg("b", []byte(strconv.Quote(strings.Repeat("b", 400))), false), agentgo.UserMsg("continue")}
	if cut := findCutPoint(msgs, 50); cut.firstKeptIndex != 1 {
		t.Fatalf("cut=%+v; parallel results must stay with their call", cut)
	}
}

func TestSummaryCompactsToolOnlyHistoryAndPreservesRecentPairs(t *testing.T) {
	msgs := []agentgo.AgentMessage{agentgo.UserMsg("task")}
	for _, id := range []string{"a", "b", "c"} {
		msgs = append(msgs, agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{
			agentgo.ToolCallBlock(agentgo.ToolCall{ID: id, Name: "read"}),
		}}, agentgo.ToolResultMsg(id, []byte(strconv.Quote(strings.Repeat("x", 400))), false))
	}
	calls := 0
	model := stubModel{generate: func(context.Context, []agentgo.Message, []agentgo.ToolSpec, ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
		calls++
		return &agentgo.LLMResponse{Message: agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("<summary>task and A reviewed</summary>")}}}, nil
	}}
	next, info, err := runSummaryCompaction(t.Context(), summaryRunConfig{Model: model, ContextWindow: 2000, ReserveTokens: 200, KeepRecentTokens: 150}, msgs, msgs, true)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || info == nil || info.CompactedCount != 3 || info.KeptCount != 4 {
		t.Fatalf("calls=%d info=%+v", calls, info)
	}
	if err := agentgo.AssertMessageSequence(agentgo.ToMessages(next)); err != nil {
		t.Fatal(err)
	}
	summary, ok := next[0].(ContextSummary)
	if !ok || len(summary.RawMessages) != 3 {
		t.Fatalf("summary lost original evidence: %+v", next[0])
	}
	for i, m := range next[1:] {
		want, _ := msgs[i+3].ToMessage()
		got, _ := m.ToMessage()
		if got.TextContent() != want.TextContent() || got.GetRole() != want.GetRole() {
			t.Fatalf("kept message %d changed", i)
		}
	}
}
