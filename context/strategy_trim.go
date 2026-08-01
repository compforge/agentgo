package context

import (
	"context"
	"fmt"

	"github.com/compforge/agentgo"
)

type LightTrimConfig struct {
	KeepRecent    int
	TextThreshold int
	PreserveHead  int
	PreserveTail  int
}

type LightTrimCompactor struct {
	cfg LightTrimConfig
}

func NewLightTrimCompactor(cfg LightTrimConfig) *LightTrimCompactor {
	if cfg.KeepRecent <= 0 {
		cfg.KeepRecent = 4
	}
	if cfg.TextThreshold <= 0 {
		cfg.TextThreshold = 4000
	}
	if cfg.PreserveHead <= 0 {
		cfg.PreserveHead = 1200
	}
	if cfg.PreserveTail <= 0 {
		cfg.PreserveTail = 800
	}
	return &LightTrimCompactor{cfg: cfg}
}

func (s *LightTrimCompactor) Compact(_ context.Context, messages []agentgo.AgentMessage, expect float64) ([]agentgo.AgentMessage, error) {
	if len(messages) == 0 || expect >= 1 {
		return messages, nil
	}

	lastEligible := len(messages) - s.cfg.KeepRecent
	if lastEligible <= 0 {
		return messages, nil
	}

	out := copyMessages(messages)
	tokens := EstimateTotal(out)
	target := int(float64(tokens) * clampRatio(expect))

	for i := 0; i < lastEligible; i++ {
		if tokens <= target {
			break
		}
		msg, ok := out[i].ToMessage()
		if !ok {
			continue
		}
		before := EstimateTokens(out[i])
		next, changed := trimLongTextBlocks(msg, s.cfg.TextThreshold, s.cfg.PreserveHead, s.cfg.PreserveTail)
		if !changed {
			continue
		}
		out[i] = newProjectedMessage(out[i], next)
		tokens -= before - EstimateTokens(out[i])
	}

	return out, nil
}

func trimLongTextBlocks(msg agentgo.Message, threshold, preserveHead, preserveTail int) (agentgo.Message, bool) {
	newContent := make([]agentgo.ContentBlock, len(msg.Content))
	changed := false
	trimmedBlocks := 0
	for i, block := range msg.Content {
		if block.Type != agentgo.ContentText {
			newContent[i] = block
			continue
		}
		runes := []rune(block.Text)
		if len(runes) <= threshold {
			newContent[i] = block
			continue
		}
		headCount := min(preserveHead, len(runes))
		tailCount := min(preserveTail, len(runes)-headCount)
		head := string(runes[:headCount])
		tail := string(runes[len(runes)-tailCount:])
		trimmed := len(runes) - headCount - tailCount
		newContent[i] = agentgo.ContentBlock{
			Type: agentgo.ContentText,
			Text: fmt.Sprintf("%s\n%s\n%s", head, formatTrimmedPlaceholder(trimmed), tail),
		}
		changed = true
		trimmedBlocks++
	}
	if !changed {
		return msg, false
	}
	next := msg
	next.Content = newContent
	next.Metadata = cloneMetadata(msg.Metadata)
	if next.Metadata == nil {
		next.Metadata = map[string]any{}
	}
	next.Metadata["trimmed_text_blocks"] = trimmedBlocks
	return next, true
}
