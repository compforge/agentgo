package agentgo

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
)

// ModelExecution describes one model execution attempt, which may contain
// multiple routed endpoint attempts. Execution.ID
// identifies the logical model call and remains stable across retries and
// recovery of the same turn. Request may be adjusted by
// middleware; the embedded Execution coordinate must be preserved.
type ModelExecution struct {
	Artifacts ArtifactManager // Call-scoped material; do not retain beyond the call.
	Execution
	Request LLMRequest
}

// ModelResult carries the response, including partial evidence on failure.
// Only successful results may enter conversation history or execute tools.
// Middleware may return a known result without invoking next.
//
// +spec=`A failed model response is diagnostic evidence only, never an accepted conversation message`
// +link=component://docs/session.md
type ModelResult struct {
	Message               Message
	HasCompletedToolCalls bool
}

type ModelExecuteFunc func(context.Context, ModelExecution) (ModelResult, error)

// ModelMiddleware wraps one model execution attempt. It may adjust call
// configuration, observe next's result, or return a known result without calling next.
// The first middleware is outermost. Call next at most once: the Loop or the
// standalone caller owns retries and their Attempt coordinates. A derived context passed to next
// reaches the provider. Returning an error fails this attempt; diagnostics
// adapters must handle their own recording errors without returning them.
// Concurrent tools can invoke this middleware concurrently; keep per-call state local.
type ModelMiddleware func(context.Context, ModelExecution, ModelExecuteFunc) (ModelResult, error)

func buildModelMiddlewareChain(exec ModelExecuteFunc, middlewares []ModelMiddleware) ModelExecuteFunc {
	for i := len(middlewares) - 1; i >= 0; i-- {
		middleware := middlewares[i]
		next := exec
		exec = func(ctx context.Context, call ModelExecution) (ModelResult, error) {
			return middleware(ctx, call, next)
		}
	}
	return exec
}

// snapshotModelRequest detaches mutable message containers before publication.
// Opaque metadata values and tool schemas follow the existing read-only contract.
func snapshotModelRequest(request LLMRequest) LLMRequest {
	request.Messages = slices.Clone(request.Messages)
	request.Tools = slices.Clone(request.Tools)
	request.Config = snapshotCallConfig(request.Config)
	for i := range request.Messages {
		m := &request.Messages[i]
		m.Content = slices.Clone(m.Content)
		m.Metadata = maps.Clone(m.Metadata)
		if m.Usage != nil {
			usage := *m.Usage
			if usage.Cost != nil {
				cost := *usage.Cost
				usage.Cost = &cost
			}
			m.Usage = &usage
		}
		for j := range m.Content {
			b := &m.Content[j]
			if b.ToolCall != nil {
				call := *b.ToolCall
				call.Args = slices.Clone(call.Args)
				b.ToolCall = &call
			}
			if b.ProviderState != nil {
				state := *b.ProviderState
				state.Data = slices.Clone(state.Data)
				b.ProviderState = &state
			}
			if b.Image != nil {
				image := *b.Image
				b.Image = &image
			}
		}
	}
	return request
}

// requestConfigOption is the only projection from execution config to the
// ChatModel option protocol. Options are never a second execution input.
func requestConfigOption(request LLMRequest) CallOption {
	return func(config *CallConfig) {
		if request.Config != nil {
			*config = *request.Config
		}
	}
}

func modelCallSettings(config *CallConfig) *ModelCallSettings {
	if config == nil {
		return &ModelCallSettings{}
	}
	return &ModelCallSettings{ThinkingLevel: config.ThinkingLevel, ThinkingBudget: config.ThinkingBudget, MaxTokens: config.MaxTokens}
}

func snapshotCallConfig(config *CallConfig) *CallConfig {
	if config == nil {
		return nil
	}
	snapshot := *config
	snapshot.APIKey = ""
	if config.Temperature != nil {
		value := *config.Temperature
		snapshot.Temperature = &value
	}
	snapshot.ToolChoice = snapshotJSONValue(config.ToolChoice)
	if config.ResponseFormat != nil {
		format := *config.ResponseFormat
		if format.JSONSchema != nil {
			schema := *format.JSONSchema
			schema.Schema = snapshotJSONValue(schema.Schema)
			if schema.Strict != nil {
				strict := *schema.Strict
				schema.Strict = &strict
			}
			format.JSONSchema = &schema
		}
		snapshot.ResponseFormat = &format
	}
	return &snapshot
}

// JSON-valued options are frozen as raw JSON for observation, preserving the
// wire value without retaining arbitrary caller-owned maps or schema structs.
// Invalid JSON values are represented by a diagnostic, never a live reference.
func snapshotJSONValue(value any) any {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return map[string]string{"snapshot_error": err.Error()}
	}
	return json.RawMessage(data)
}
