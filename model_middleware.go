package agentgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// ModelExecution describes one model execution attempt, which may contain
// multiple routed endpoint attempts. Execution.ID
// identifies the logical model call and remains stable across retries and
// recovery of the same turn. Request may be adjusted by
// middleware; the embedded Execution coordinate must be preserved.
type ModelExecution struct {
	Artifacts ArtifactManager // Runtime-owned material; do not retain beyond the callback.
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

type modelExecutionRuntime struct {
	middlewares []ModelMiddleware
	artifacts   ArtifactManager
	emit        func(Event)
}

type modelExecutionRuntimeKey struct{}

func withModelExecutionRuntime(ctx context.Context, middlewares []ModelMiddleware, emit func(Event), artifacts ArtifactManager) context.Context {
	runtime := modelExecutionRuntime{
		artifacts:   artifacts,
		middlewares: append([]ModelMiddleware(nil), middlewares...),
		emit:        emit,
	}
	if emit != nil {
		runtime.emit = func(event Event) {
			if event.Timestamp.IsZero() {
				event.Timestamp = time.Now()
			}
			emit(event)
		}
	}
	return context.WithValue(ctx, modelExecutionRuntimeKey{}, runtime)
}

// WithModelRuntime installs middleware and an event observer for standalone model
// calls. AgentLoop installs its own runtime; nested model calls inherit it.
// Middleware and observers may run concurrently and must finish their work before
// returning. The caller owns the observer lifetime and recording errors.
func WithModelRuntime(ctx context.Context, middlewares []ModelMiddleware, onEvent func(Event)) context.Context {
	return withModelExecutionRuntime(ctx, middlewares, onEvent, nil)
}

// GenerateModel executes a non-streaming call through the same middleware and
// event path as AgentLoop. The caller supplies the execution identity and owns
// retries. A response returned alongside an error remains diagnostic evidence.
func GenerateModel(ctx context.Context, model ChatModel, execution ModelExecution) (ModelResult, error) {
	return ExecuteModel(ctx, execution, func(ctx context.Context, call ModelExecution) (ModelResult, error) {
		if model == nil {
			return ModelResult{}, ErrNoModel
		}
		response, err := model.Generate(ctx, call.Request.Messages, call.Request.Tools, requestConfigOption(call.Request))
		if response == nil {
			if err == nil {
				err = fmt.Errorf("model execution %s returned no response", call.ID)
			}
			return ModelResult{}, err
		}
		return ModelResult{Message: response.Message}, err
	})
}

// ExecuteModel runs one model attempt through the runtime installed by
// WithModelRuntime or AgentLoop. Custom streaming executors use this entry;
// ordinary non-streaming consumers use GenerateModel. Without a runtime it
// still establishes execution identity and catches panics, but emits no events.
// An omitted ParentID inherits the enclosing execution. Nested calls must finish
// before the owning operation returns. Request.Config is initialized before
// middleware; middleware must preserve the embedded Execution coordinate.
//
// +spec=`Request.Config is the sole call input; model_request freezes it after middleware with credentials redacted; short-circuits have no executor request`
// +link=component://docs/session.md
func ExecuteModel(ctx context.Context, execution ModelExecution, next ModelExecuteFunc) (result ModelResult, err error) {
	runtime, _ := ctx.Value(modelExecutionRuntimeKey{}).(modelExecutionRuntime)
	if runtime.artifacts != nil {
		execution.Artifacts = runtime.artifacts
	}
	if parent, ok := ExecutionFromContext(ctx); ok && execution.ParentID == "" && parent.ID != execution.ID {
		execution.ParentID = parent.ID
	}
	if execution.Request.Config == nil {
		execution.Request.Config = &CallConfig{}
	} else {
		config := *execution.Request.Config
		execution.Request.Config = &config
	}
	ctx = ContextWithExecution(ctx, execution.Execution)
	if runtime.emit != nil {
		runtime.emit(Event{Type: EventModelExecStart, Execution: executionRef(execution.Execution), ModelCall: modelCallSettings(execution.Request.Config)})
	}

	var dispatched *ModelCallSettings
	defer func() {
		if recovered := recover(); recovered != nil {
			result, err = ModelResult{}, fmt.Errorf("model execution %s panicked: %v", execution.ID, recovered)
		}
		var partial *PartialStreamError
		if errors.As(err, &partial) && result.Message.Role == "" && len(result.Message.Content) == 0 && result.Message.Usage == nil {
			result.Message = partial.Partial
		}
		if runtime.emit != nil {
			runtime.emit(Event{Type: EventModelExecEnd, Execution: executionRef(execution.Execution), Message: result.Message, ModelCall: dispatched, Err: err})
		}
	}()
	execute := func(ctx context.Context, call ModelExecution) (ModelResult, error) {
		dispatched = modelCallSettings(call.Request.Config)
		if runtime.emit != nil {
			request := snapshotModelRequest(call.Request)
			runtime.emit(Event{Type: EventModelRequest, Execution: executionRef(call.Execution), ModelRequest: &request})
		}
		return next(ctx, call)
	}
	if len(runtime.middlewares) > 0 {
		execute = buildModelMiddlewareChain(execute, runtime.middlewares)
	}
	return execute(ctx, execution)
}

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
