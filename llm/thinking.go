package llm

import "github.com/compforge/agentgo"

const ThinkingAuto agentgo.ThinkingLevel = ""

var ThinkingLevelOrder = []agentgo.ThinkingLevel{
	agentgo.ThinkingOff,
	agentgo.ThinkingMinimal,
	agentgo.ThinkingLow,
	agentgo.ThinkingMedium,
	agentgo.ThinkingHigh,
	agentgo.ThinkingXHigh,
	agentgo.ThinkingMax,
}

type ThinkingPolicy struct {
	Available []agentgo.ThinkingLevel
}

func ThinkingPolicyFor(model any) ThinkingPolicy {
	cp, ok := model.(CapabilityProvider)
	if !ok {
		return ThinkingPolicy{Available: append([]agentgo.ThinkingLevel{ThinkingAuto}, ThinkingLevelOrder...)}
	}
	return ThinkingPolicyFromCapabilities(cp.Capabilities())
}

func ThinkingPolicyFromCapabilities(caps Capabilities) ThinkingPolicy {
	if !caps.ProviderBaseline {
		return exactThinkingPolicy(caps.Thinking)
	}
	return baselineThinkingPolicy(caps.Thinking)
}

func baselineThinkingPolicy(caps ThinkingCapabilities) ThinkingPolicy {
	if caps.Supported == SupportNo {
		return ThinkingPolicy{Available: []agentgo.ThinkingLevel{ThinkingAuto}}
	}
	available := []agentgo.ThinkingLevel{ThinkingAuto}
	for _, level := range ThinkingLevelOrder {
		if level == agentgo.ThinkingOff && caps.Disable == SupportNo {
			continue
		}
		available = append(available, level)
	}
	return ThinkingPolicy{Available: available}
}

func exactThinkingPolicy(caps ThinkingCapabilities) ThinkingPolicy {
	if caps.Supported == SupportUnknown && caps.Disable == SupportUnknown && len(caps.Efforts) == 0 {
		return ThinkingPolicy{Available: append([]agentgo.ThinkingLevel{ThinkingAuto}, ThinkingLevelOrder...)}
	}
	available := []agentgo.ThinkingLevel{ThinkingAuto}
	if caps.Disable == SupportYes || caps.Disable == SupportPartial {
		available = append(available, agentgo.ThinkingOff)
	}
	available = append(available, caps.Efforts...)
	return ThinkingPolicy{Available: uniqueThinkingLevels(available)}
}

func (p ThinkingPolicy) Allows(level agentgo.ThinkingLevel) bool {
	for _, available := range p.Available {
		if available == level {
			return true
		}
	}
	return false
}

func (p ThinkingPolicy) Resolve(level agentgo.ThinkingLevel) (agentgo.ThinkingLevel, bool) {
	level = agentgo.NormalizeThinkingLevel(level)
	if p.Allows(level) {
		return level, true
	}
	return ThinkingAuto, false
}

func uniqueThinkingLevels(levels []agentgo.ThinkingLevel) []agentgo.ThinkingLevel {
	seen := make(map[agentgo.ThinkingLevel]struct{}, len(levels))
	out := make([]agentgo.ThinkingLevel, 0, len(levels))
	for _, level := range levels {
		if _, ok := seen[level]; ok {
			continue
		}
		seen[level] = struct{}{}
		out = append(out, level)
	}
	return out
}
