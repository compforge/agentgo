package context

import (
	"context"
	"strings"
	"testing"

	"github.com/compforge/agentgo"
)

// sessionMemoryConvo builds a long enough conversation that cutting it at
// KeepRecentTokens produces a non-zero history prefix. Each user/assistant
// pair contributes enough tokens to exceed the 20k default reserve.
func sessionMemoryConvo() []agentgo.AgentMessage {
	msgs := []agentgo.AgentMessage{}
	body := strings.Repeat("lorem ipsum dolor sit amet ", 400) // ~100 tokens per copy
	for range 20 {
		msgs = append(msgs,
			agentgo.UserMsg(body),
			agentgo.Message{
				Role:    agentgo.RoleAssistant,
				Content: []agentgo.ContentBlock{{Type: agentgo.ContentText, Text: body}},
			},
		)
	}
	// Recent tail that should be kept verbatim.
	msgs = append(msgs,
		agentgo.UserMsg("recent user question"),
		agentgo.Message{
			Role:    agentgo.RoleAssistant,
			Content: []agentgo.ContentBlock{{Type: agentgo.ContentText, Text: "recent assistant reply"}},
		},
	)
	return msgs
}

func TestSessionMemoryCompactorNoopWhenNoSeed(t *testing.T) {
	t.Parallel()

	s := NewSessionMemoryCompactor(SessionMemoryConfig{
		SeedFn:           func() (string, error) { return "", nil },
		KeepRecentTokens: 1000,
	})
	msgs := sessionMemoryConvo()
	out, err := s.Compact(context.Background(), msgs, 0.25)
	if err != nil {
		t.Fatalf("apply err: %v", err)
	}
	if len(out) != len(msgs) {
		t.Fatalf("view mutated, got %d msgs want %d", len(out), len(msgs))
	}
}

func TestSessionMemoryCompactorNoopAtFullRatio(t *testing.T) {
	t.Parallel()

	s := NewSessionMemoryCompactor(SessionMemoryConfig{
		SeedFn:           func() (string, error) { return "# Session Memory\n\nRich state.", nil },
		KeepRecentTokens: 1000,
	})
	msgs := sessionMemoryConvo()
	_, err := s.Compact(context.Background(), msgs, 1)
	if err != nil {
		t.Fatalf("apply err: %v", err)
	}
}

func TestSessionMemoryCompactorAppliesSeedWithoutLLM(t *testing.T) {
	t.Parallel()

	const seed = "# Session Memory\n\n## Current State\nHarness optimization in progress."
	callCount := 0
	s := NewSessionMemoryCompactor(SessionMemoryConfig{
		SeedFn: func() (string, error) {
			callCount++
			return seed, nil
		},
		KeepRecentTokens: 200,
	})
	msgs := sessionMemoryConvo()
	out, err := s.Compact(context.Background(), msgs, 0.25)
	if err != nil {
		t.Fatalf("apply err: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("SeedFn should be invoked exactly once, got %d", callCount)
	}
	if len(out) == 0 {
		t.Fatal("compacted view must not be empty")
	}
	cs, ok := out[0].(ContextSummary)
	if !ok {
		t.Fatalf("first message must be a ContextSummary checkpoint, got %T", out[0])
	}
	if !strings.Contains(cs.Summary, "Harness optimization in progress") {
		t.Fatalf("summary body must be sourced from the seed, got %q", cs.Summary)
	}
	if len(cs.RawMessages) == 0 {
		t.Fatal("summary must retain the raw messages it replaced")
	}
}

func TestSessionMemoryCompactorTruncatesOversizedSeed(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", 30000)
	s := NewSessionMemoryCompactor(SessionMemoryConfig{
		SeedFn:           func() (string, error) { return big, nil },
		KeepRecentTokens: 200,
		MaxSeedRunes:     1000,
	})
	msgs := sessionMemoryConvo()
	out, err := s.Compact(context.Background(), msgs, 0.25)
	if err != nil {
		t.Fatalf("apply err: %v", err)
	}
	cs := out[0].(ContextSummary)
	if !strings.Contains(cs.Summary, "truncated for compact budget") {
		t.Fatal("oversized seed must include the truncation notice so the model knows content was dropped")
	}
	if len([]rune(cs.Summary)) > 1500 {
		t.Fatalf("truncation ineffective: summary is %d runes", len([]rune(cs.Summary)))
	}
}

func TestSessionMemoryCompactorSeedErrorFallsThrough(t *testing.T) {
	t.Parallel()

	s := NewSessionMemoryCompactor(SessionMemoryConfig{
		SeedFn:           func() (string, error) { return "", context.DeadlineExceeded },
		KeepRecentTokens: 200,
	})
	msgs := sessionMemoryConvo()
	out, err := s.Compact(context.Background(), msgs, 0.25)
	if err != nil {
		t.Fatalf("errors from SeedFn must not bubble up: %v", err)
	}
	if len(out) != len(msgs) {
		t.Fatal("view must not be mutated on seed error")
	}
}
