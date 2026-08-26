package llm

import (
	"slices"
	"testing"

	"github.com/compforge/agentgo"
)

func TestThinkingPolicyFromCapabilities(t *testing.T) {
	tests := []struct {
		name string
		caps Capabilities
		want []agentgo.ThinkingLevel
	}{
		{
			name: "exact model capabilities",
			caps: Capabilities{Thinking: ThinkingCapabilities{
				Supported: SupportYes,
				Disable:   SupportNo,
				Efforts:   []agentgo.ThinkingLevel{agentgo.ThinkingHigh},
			}},
			want: []agentgo.ThinkingLevel{ThinkingAuto, agentgo.ThinkingHigh},
		},
		{
			name: "provider baseline leaves model efforts open",
			caps: Capabilities{
				ProviderBaseline: true,
				Thinking: ThinkingCapabilities{
					Supported: SupportYes,
					Disable:   SupportNo,
					Efforts:   []agentgo.ThinkingLevel{agentgo.ThinkingHigh},
				},
			},
			want: []agentgo.ThinkingLevel{
				ThinkingAuto,
				agentgo.ThinkingMinimal,
				agentgo.ThinkingLow,
				agentgo.ThinkingMedium,
				agentgo.ThinkingHigh,
				agentgo.ThinkingXHigh,
				agentgo.ThinkingMax,
			},
		},
		{
			name: "provider does not support thinking",
			caps: Capabilities{
				ProviderBaseline: true,
				Thinking:         ThinkingCapabilities{Supported: SupportNo},
			},
			want: []agentgo.ThinkingLevel{ThinkingAuto},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ThinkingPolicyFromCapabilities(tt.caps).Available; !slices.Equal(got, tt.want) {
				t.Fatalf("available thinking levels = %v, want %v", got, tt.want)
			}
		})
	}
}
