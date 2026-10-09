package context

import (
	"context"
	"strings"
	"testing"

	"github.com/compforge/agentgo"
)

func TestTransformRunsBelowThresholdAndRetainsRaw(t *testing.T) {
	original := agentgo.Message{Role: agentgo.RoleAssistant, Content: []agentgo.ContentBlock{agentgo.TextBlock("abc")}, Usage: &agentgo.Usage{Input: 50}}
	calls := 0
	engine := NewEngine(EngineConfig{ContextWindow: 10000, Transformers: []Transformer{
		TransformFunc(func(_ context.Context, input agentgo.TransformContext) ([]agentgo.AgentMessage, error) {
			messages := input.Messages
			calls++
			if messages[0].TextContent() == "xyz" {
				return messages, nil
			}
			current, _ := messages[0].ToMessage()
			current.Content = []agentgo.ContentBlock{agentgo.TextBlock("xyz")}
			return []agentgo.AgentMessage{newProjectedMessage(messages[0], current)}, nil
		}),
	}})
	input := []agentgo.AgentMessage{original}
	view, err := engine.Transform(t.Context(), agentgo.TransformContext{Messages: input})
	if err != nil || calls != 1 || view[0].TextContent() != "xyz" || input[0].TextContent() != "abc" || view[0].Raw().TextContent() != "abc" {
		t.Fatalf("view=%v err=%v calls=%d", view, err, calls)
	}
	// Equal-sized rewrites still invalidate the old prompt calibration.
	if engine.Usage().UsageTokens != 0 {
		t.Fatal("stale usage survived an equal-sized rewrite")
	}
	again, err := engine.Transform(t.Context(), agentgo.TransformContext{Messages: view})
	if err != nil || again[0].TextContent() != view[0].TextContent() || again[0].Raw().TextContent() != "abc" {
		t.Fatal("transform not repeatable")
	}
}

func TestThresholdUsesBaselineIndependentlyOfTransform(t *testing.T) {
	compactor := &recordingCompactor{result: []agentgo.AgentMessage{agentgo.UserMsg("compact baseline")}}
	transform := TransformFunc(func(_ context.Context, input agentgo.TransformContext) ([]agentgo.AgentMessage, error) {
		messages := input.Messages
		current := agentgo.UserMsg("request view")
		return []agentgo.AgentMessage{newProjectedMessage(messages[0], current)}, nil
	})
	engine := NewEngine(EngineConfig{ContextWindow: 1000, ReserveTokens: 100, Compactor: compactor, Transformers: []Transformer{transform}})
	input := []agentgo.AgentMessage{agentgo.UserMsg(strings.Repeat("raw ", 2000))}
	result, err := engine.Compact(t.Context(), agentgo.TransformContext{Messages: input}, agentgo.CompactReasonThreshold)
	if err != nil || !result.Changed || len(compactor.expects) != 1 {
		t.Fatalf("baseline compaction: %+v %v", result, err)
	}
	engine.SetContextWindow(1)
	engine.SetReserveTokens(1)
	result, err = engine.Compact(t.Context(), agentgo.TransformContext{Messages: input}, agentgo.CompactReasonThreshold)
	if err != nil || !result.Changed || result.Messages[0].TextContent() != "compact baseline" {
		t.Fatalf("committed transient view: %+v %v", result, err)
	}
	view, err := engine.Transform(t.Context(), agentgo.TransformContext{Messages: result.Messages})
	if err != nil || view[0].TextContent() != "request view" || view[0].Raw().TextContent() != "compact baseline" {
		t.Fatal("post-compaction view not rebuilt")
	}
}

func TestTransformExpansionDoesNotTriggerCompaction(t *testing.T) {
	calls := 0
	compactor := &recordingCompactor{}
	engine := NewEngine(EngineConfig{ContextWindow: 1000, ReserveTokens: 100, Compactor: compactor,
		Transformers: []Transformer{TransformFunc(func(_ context.Context, input agentgo.TransformContext) ([]agentgo.AgentMessage, error) {
			calls++
			return []agentgo.AgentMessage{newProjectedMessage(input.Messages[0], agentgo.UserMsg(strings.Repeat("expanded ", 1000)))}, nil
		})},
	})
	input := []agentgo.AgentMessage{agentgo.UserMsg("reference")}
	engine.Sync(input)
	result, err := engine.Compact(t.Context(), agentgo.TransformContext{Messages: input}, agentgo.CompactReasonThreshold)
	if err != nil || result.Changed || calls != 0 || len(compactor.expects) != 0 {
		t.Fatalf("compact coupled to transformer: %+v %v calls=%d", result, err, calls)
	}
	view, err := engine.Transform(t.Context(), agentgo.TransformContext{Messages: input})
	if err != nil || calls != 1 || EstimateTotal(view) <= 1000 || engine.Snapshot().BaselineUsage.Tokens != EstimateTotal(input) {
		t.Fatalf("expansion changed baseline: %v %+v", err, engine.Snapshot())
	}
}
