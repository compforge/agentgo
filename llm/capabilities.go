package llm

import (
	"github.com/compforge/agentgo"
	"github.com/voocel/litellm"
)

// Support describes whether a model/provider supports a capability.
type Support int

const (
	SupportUnknown Support = iota
	SupportNo
	SupportYes
	SupportPartial
)

// CapabilityProvider is implemented by models that can expose provider/model
// capabilities for UI preflight and configuration validation. It is advisory:
// request execution remains the source of truth and should still fail loudly.
type CapabilityProvider interface {
	Capabilities() Capabilities
}

// Capabilities is agentgo's provider-neutral view of model capabilities.
type Capabilities struct {
	Provider         string
	Model            string
	ProviderBaseline bool // model-specific limits are enforced by the provider

	Thinking ThinkingCapabilities
}

type ThinkingCapabilities struct {
	Supported Support
	Disable   Support
	Efforts   []agentgo.ThinkingLevel
}

func fromLiteLLMCapabilities(c litellm.Capabilities) Capabilities {
	support := func(v bool) Support {
		if v {
			return SupportYes
		}
		return SupportNo
	}
	return Capabilities{ProviderBaseline: true, Thinking: ThinkingCapabilities{Supported: support(c.ThinkingEffort), Disable: support(c.DisableThinking)}}
}

func (c Capabilities) ThinkingPolicy() ThinkingPolicy {
	return ThinkingPolicyFromCapabilities(c)
}
