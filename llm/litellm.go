// Package llm adapts LLM providers to the [agentgo.ChatModel] interface. It
// wraps litellm to reach OpenAI, Anthropic, Gemini, and other backends, and
// classifies provider errors onto agentgo's retry and overflow contracts.
// Construct a model with [NewModel].
package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/compforge/agentgo"
	"github.com/compforge/agentgo/internal/streaming"
	"github.com/voocel/litellm"
	"github.com/voocel/litellm/provider"
	"github.com/voocel/litellm/provider/bedrock"
	"github.com/voocel/litellm/retry"
)

// ProviderConfig is kept for source compatibility with older agentgo callers.
// New litellm providers expose explicit Config structs; this package maps the
// common subset that agentgo needs.
type ProviderConfig struct {
	Transport http.RoundTripper
	APIKey    string
	BaseURL   string
	Timeout   time.Duration
	Extra     map[string]any
	Retry     *retry.Policy
}

// ResilienceConfig is the compatibility shape for retry and stream idle knobs.
type ResilienceConfig struct {
	StreamIdleTimeout time.Duration
	Retry             *retry.Policy
}

// LiteLLMAdapter adapts litellm to the agentgo.ChatModel interface.
type LiteLLMAdapter struct {
	*BaseModel
	client         *litellm.Client
	model          string
	requestTimeout time.Duration
	extra          map[string]any // model-level Extra merged into every request
}

// NewLiteLLMAdapter wraps an existing litellm.Client as a ChatModel. Use this
// when you need to reuse a Client or inject a custom Provider instance; for
// the common case prefer NewModel.
func NewLiteLLMAdapter(model string, client *litellm.Client) *LiteLLMAdapter {
	modelInfo := ModelInfo{
		Name:     model,
		Provider: client.ProviderName(),
		Capabilities: []string{
			string(CapabilityChat),
			string(CapabilityCompletion),
			string(CapabilityStreaming),
			string(CapabilityToolCalling),
		},
	}

	return &LiteLLMAdapter{
		BaseModel: NewBaseModel(modelInfo, DefaultGenerationConfig),
		client:    client,
		model:     model,
	}
}

// modelConfig collects optional knobs for NewModel.
type modelConfig struct {
	transport     http.RoundTripper
	apiKey        string
	baseURL       string
	timeout       time.Duration
	streamIdle    time.Duration
	streamIdleSet bool // distinguishes "unset" from explicit 0 (disable watchdog)
	resilience    *ResilienceConfig
	clientOpts    []litellm.ClientOption
	extra         map[string]any
	providerExtra map[string]any
}

// ModelOption configures NewModel.
type ModelOption func(*modelConfig)

func WithAPIKey(key string) ModelOption  { return func(c *modelConfig) { c.apiKey = key } }
func WithBaseURL(url string) ModelOption { return func(c *modelConfig) { c.baseURL = url } }

// WithHTTPTransport reuses a caller-owned transport, including connection
// pools and transport diagnostics, through litellm's provider factory.
func WithHTTPTransport(t http.RoundTripper) ModelOption {
	return func(c *modelConfig) { c.transport = t }
}

// WithRequestTimeout sets an optional per-request timeout. Zero leaves timeout control to the caller context.
func WithRequestTimeout(d time.Duration) ModelOption {
	return func(c *modelConfig) { c.timeout = d }
}

// WithStreamIdleTimeout aborts a streaming response if no chunk arrives within
// the window (default 120s). Pass 0 to disable the watchdog explicitly.
func WithStreamIdleTimeout(d time.Duration) ModelOption {
	return func(c *modelConfig) {
		c.streamIdle = d
		c.streamIdleSet = true
	}
}

// WithResilience replaces the entire resilience config; later With* options
// may still override specific fields.
func WithResilience(rc ResilienceConfig) ModelOption {
	return func(c *modelConfig) { c.resilience = &rc }
}

// WithClientOptions forwards litellm ClientOptions (e.g. litellm.WithObserver) to
// the underlying client, letting callers attach observability or other
// cross-cutting behaviour without this package importing those concerns.
func WithClientOptions(opts ...litellm.ClientOption) ModelOption {
	return func(c *modelConfig) { c.clientOpts = append(c.clientOpts, opts...) }
}

// WithExtra sets model-level, provider-specific request parameters merged into
// every request's Extra map (e.g. min_p, presence_penalty, or provider keys like
// chat_template_kwargs). OpenAI-compatible providers pass these through verbatim
// into the request body — the extra_body convention. Per-call Extra entries
// (e.g. session_id) are added alongside. Explicit WithThinking/WithThinkingBudget
// replace raw thinking defaults for that call; provider-specific mapping remains
// in the provider. Other Extra keys are preserved.
func WithExtra(extra map[string]any) ModelOption {
	return func(c *modelConfig) { c.extra = extra }
}

// WithProviderExtra sets provider-level configuration passed to
// litellm's provider factory. Use it for name, HTTP headers or other provider
// client options, while WithExtra remains request-body Extra.
func WithProviderExtra(extra map[string]any) ModelOption {
	return func(c *modelConfig) { c.providerExtra = extra }
}

// cloneExtra copies the model-level extra map so per-call mutations (e.g.
// session_id) never leak back into the shared model config across requests.
func cloneExtra(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	c := make(map[string]any, len(m))
	maps.Copy(c, m)
	return c
}

// NewModel constructs a ChatModel by provider name. The provider must be
// registered in litellm (builtin or via litellm.RegisterProvider).
func NewModel(provider, model string, opts ...ModelOption) (*LiteLLMAdapter, error) {
	cfg := modelConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	retryPolicy := (*retry.Policy)(nil)
	if cfg.resilience != nil {
		retryPolicy = cfg.resilience.Retry
	}
	pcfg := ProviderConfig{
		Transport: cfg.transport,
		APIKey:    cfg.apiKey,
		BaseURL:   cfg.baseURL,
		Timeout:   cfg.timeout,
		Extra:     cloneExtra(cfg.providerExtra),
		Retry:     retryPolicy,
	}
	providerImpl, err := newProvider(provider, pcfg)
	if err != nil {
		return nil, fmt.Errorf("llm: %s: %w", provider, err)
	}
	clientOpts := append([]litellm.ClientOption(nil), cfg.clientOpts...)
	if cfg.streamIdleSet {
		clientOpts = append(clientOpts, litellm.WithStreamIdleTimeout(cfg.streamIdle))
	} else if cfg.resilience != nil && cfg.resilience.StreamIdleTimeout > 0 {
		clientOpts = append(clientOpts, litellm.WithStreamIdleTimeout(cfg.resilience.StreamIdleTimeout))
	} else {
		clientOpts = append(clientOpts, litellm.WithStreamIdleTimeout(120*time.Second))
	}
	client, err := litellm.New(providerImpl, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("llm: %s client: %w", provider, err)
	}
	adapter := NewLiteLLMAdapter(model, client)
	adapter.extra = cfg.extra
	adapter.requestTimeout = cfg.timeout
	return adapter, nil
}

// IsProviderRegistered delegates provider discovery to litellm.
func IsProviderRegistered(name string) bool {
	for _, p := range provider.Names() {
		if p == strings.ToLower(strings.TrimSpace(name)) {
			return true
		}
	}
	return false
}
func RegisteredProviders() []string { return provider.Names() }

// ProviderName returns the provider name (e.g. "openai", "anthropic").
// Implements agentgo.ProviderNamer for per-provider API key resolution.
func (l *LiteLLMAdapter) ProviderName() string {
	return l.Info().Provider
}

// Capabilities returns the provider/model capability view exposed by litellm.
func (l *LiteLLMAdapter) Capabilities() Capabilities {
	if l == nil {
		return Capabilities{}
	}
	if l.client == nil {
		caps := Capabilities{Model: l.model}
		if l.BaseModel != nil {
			info := l.Info()
			caps.Provider = info.Provider
			if caps.Model == "" {
				caps.Model = info.Name
			}
		}
		return caps
	}
	raw, ok := l.client.Capabilities()
	caps := Capabilities{Provider: l.ProviderName(), Model: l.model, ProviderBaseline: true}
	if ok {
		caps = fromLiteLLMCapabilities(raw)
		caps.Provider = l.ProviderName()
		caps.Model = l.model
	}
	return caps
}

// Generate produces a synchronous response.
func (l *LiteLLMAdapter) Generate(ctx context.Context, messages []agentgo.Message, tools []agentgo.ToolSpec, opts ...agentgo.CallOption) (*agentgo.LLMResponse, error) {
	if l.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.requestTimeout)
		defer cancel()
	}
	cfg := l.GetConfig()
	llmMessages := convertMessages(messages)

	ltReq := &litellm.Request{
		Model:           l.model,
		Messages:        llmMessages,
		MaxTokens:       &cfg.MaxTokens,
		ProviderOptions: nil,
	}
	if cfg.MaxTokens <= 0 {
		ltReq.MaxTokens = nil
	}
	applySamplingConfig(ltReq, cfg)

	var err error
	ltReq.ProviderOptions, err = litellm.NewProviderOptions(cloneExtra(l.extra))
	if err != nil {
		return nil, err
	}
	ctx, err = applyCallConfig(ctx, ltReq, opts)
	if err != nil {
		return nil, err
	}
	if err := applyToolConfig(ltReq, tools); err != nil {
		return nil, err
	}

	ltResp, err := l.client.Chat(ctx, *ltReq)
	if ltResp == nil {
		return nil, wrapProviderError(err)
	}

	msg := convertResponse(ltResp)
	recordModelSettings(&msg, ltReq)
	if msg.Usage != nil {
		msg.Usage.Cost = CalculateCost(pricingForUsage(l.Info().Pricing, msg.Usage), msg.Usage)
	}
	return &agentgo.LLMResponse{Message: msg}, wrapProviderError(err)
}

// GenerateStream adapts litellm's validated block lifecycle without interpreting
// provider events. Upstream owns parsing, aggregation and replay state.
func (l *LiteLLMAdapter) GenerateStream(ctx context.Context, messages []agentgo.Message, tools []agentgo.ToolSpec, opts ...agentgo.CallOption) (<-chan agentgo.StreamEvent, error) {
	ctx, cancel := routeContext(ctx, l.requestTimeout)
	cfg := l.GetConfig()
	extra, err := litellm.NewProviderOptions(cloneExtra(l.extra))
	if err != nil {
		cancel()
		return nil, err
	}
	request := &litellm.Request{Model: l.model, Messages: convertMessages(messages), ProviderOptions: extra}
	if cfg.MaxTokens > 0 {
		request.MaxTokens = &cfg.MaxTokens
	}
	applySamplingConfig(request, cfg)
	ctx, err = applyCallConfig(ctx, request, opts)
	if err != nil {
		cancel()
		return nil, err
	}
	if err = applyToolConfig(request, tools); err != nil {
		cancel()
		return nil, err
	}
	stream, err := l.client.Stream(ctx, *request)
	if err != nil {
		cancel()
		return nil, wrapProviderError(err)
	}
	events := make(chan agentgo.StreamEvent, 100)
	go func() {
		defer close(events)
		defer cancel()
		defer stream.Close()
		partial := agentgo.Message{Role: agentgo.RoleAssistant}
		send := func(event agentgo.StreamEvent) error {
			select {
			case events <- event:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		response, err := litellm.Handle(stream, func(event litellm.Event) error {
			out := agentgo.StreamEvent{}
			switch e := event.(type) {
			case litellm.BlockStart:
				block, ok := fromProviderBlock(e.Block)
				if !ok {
					return nil
				}
				for len(partial.Content) <= e.Index {
					partial.Content = append(partial.Content, agentgo.ContentBlock{})
				}
				partial.Content[e.Index] = block
				out.ContentIndex = e.Index
				switch b := e.Block.(type) {
				case litellm.TextBlock:
					out.Type = agentgo.StreamEventTextStart
				case litellm.ReasoningBlock:
					out.Type = agentgo.StreamEventThinkingStart
				case litellm.ToolUseBlock:
					out.Type = agentgo.StreamEventToolCallStart
					out.ToolID = b.ID
				default:
					return nil
				}
			case litellm.TextDelta:
				partial.Content[e.Index].Text += e.Text
				out.Type = agentgo.StreamEventTextDelta
				out.ContentIndex = e.Index
				out.Delta = e.Text
			case litellm.ReasoningDelta:
				partial.Content[e.Index].Thinking += e.Text
				out.Type = agentgo.StreamEventThinkingDelta
				out.ContentIndex = e.Index
				out.Delta = e.Text
			case litellm.ToolUseDelta:
				out.Type = agentgo.StreamEventToolCallDelta
				out.ContentIndex = e.Index
				out.Delta = e.Arguments
				out.ToolID = partial.Content[e.Index].ToolCall.ID
			case litellm.BlockEnd:
				block, ok := fromProviderBlock(e.Block)
				if !ok {
					return nil
				}
				partial.Content[e.Index] = block
				out.ContentIndex = e.Index
				switch block.Type {
				case agentgo.ContentText:
					out.Type = agentgo.StreamEventTextEnd
				case agentgo.ContentThinking:
					out.Type = agentgo.StreamEventThinkingEnd
				case agentgo.ContentToolCall:
					out.Type = agentgo.StreamEventToolCallEnd
					out.ToolID = block.ToolCall.ID
					call := *block.ToolCall
					out.CompletedToolCall = &call
				default:
					return nil
				}
			default:
				return nil
			}
			out.Message = streaming.Snapshot(partial)
			return send(out)
		})
		if err != nil {
			_ = send(agentgo.StreamEvent{Type: agentgo.StreamEventError, Message: streaming.Snapshot(partial), Err: wrapProviderError(err)})
			return
		}
		final := convertResponse(response)
		recordModelSettings(&final, request)
		if final.Usage != nil {
			final.Usage.Cost = CalculateCost(l.Info().Pricing, final.Usage)
		}
		_ = send(agentgo.StreamEvent{Type: agentgo.StreamEventDone, Message: final, StopReason: final.StopReason})
	}()
	return events, nil
}

// convertMessages converts agentgo.Message to litellm.Message.
// Handles multipart content with ordered litellm Blocks.
//
// Drops two classes of assistant turns that strict OpenAI-compatible providers
// reject with "assistant must provide content, reasoning_content or
// tool_calls":
//   - Fully empty turns (no content/reasoning/tool_calls/images at all).
//   - Reasoning-only turns that stopped naturally (stop_reason=stop, no
//     externally-visible action). These are valid on reasoning-aware
//     providers (DeepSeek/GLM/Qwen) which accept reasoning_content on
//     replay, but trip providers that ignore reasoning_content on the
//     request side. Skipping is semantically equivalent to "this turn
//     produced nothing": no text was uttered, no tool was invoked, so the
//     next request lets the model decide afresh from prior history without
//     leaking the discarded internal reasoning back into the transcript.
func convertMessages(messages []agentgo.Message) []litellm.Message {
	llmMessages := make([]litellm.Message, 0, len(messages))
	for _, msg := range messages {
		converted := convertSingleMessage(msg)
		if msg.Role == agentgo.RoleAssistant {
			if isEmptyAssistantMessage(converted) {
				continue
			}
			if isReasoningOnlyStopAssistant(msg, converted) {
				continue
			}
		}
		llmMessages = append(llmMessages, converted)
	}
	return llmMessages
}

// isEmptyAssistantMessage reports whether a converted assistant message has
// no payload that any provider could consume.
func isEmptyAssistantMessage(m litellm.Message) bool {
	return len(m.Blocks) == 0
}

// isReasoningOnlyStopAssistant reports whether an assistant turn carried only
// internal reasoning and stopped without any externally-visible action.
func isReasoningOnlyStopAssistant(orig agentgo.Message, converted litellm.Message) bool {
	if orig.StopReason != agentgo.StopReasonStop {
		return false
	}
	if len(converted.Blocks) != 1 {
		return false
	}
	block, ok := converted.Blocks[0].(litellm.ReasoningBlock)
	return ok && block.State == nil
}

// hasImageContent reports whether any content block is an image.
func hasImageContent(blocks []agentgo.ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == agentgo.ContentImage && b.Image != nil {
			return true
		}
	}
	return false
}

// convertSingleMessage converts one agentgo.Message to litellm.Message.
func convertSingleMessage(msg agentgo.Message) litellm.Message {
	llmMsg := litellm.Message{Role: litellm.Role(msg.Role)}
	var blocks []litellm.Block
	cache := cacheControlFromMetadata(msg.Metadata)
	if msg.Role == agentgo.RoleTool {
		toolCallID, _ := msg.Metadata["tool_call_id"].(string)
		isError, _ := msg.Metadata["is_error"].(bool)
		// Cache lands on the outer tool_result block only; nested content
		// cannot carry provider cache breakpoints.
		content := convertAgentBlocks(msg.Content, nil)
		blocks = append(blocks, litellm.ToolResultBlock{ToolUseID: toolCallID, Content: content, IsError: isError, Cache: cache})
	} else {
		blocks = convertAgentBlocks(msg.Content, cache)
	}
	llmMsg.Blocks = blocks
	return llmMsg
}

// cacheControlFromMetadata parses the "cache_control" metadata value into a
// litellm CacheControl. The value is "type" or "type:ttl" — e.g. "ephemeral"
// (provider-default TTL) or "ephemeral:1h" (extended TTL where supported).
// TTL validity is enforced by the provider layer.
func cacheControlFromMetadata(metadata map[string]any) *litellm.CacheControl {
	value, _ := metadata["cache_control"].(string)
	if value == "" {
		return nil
	}
	if _, ttl, ok := strings.Cut(value, ":"); ok {
		return &litellm.CacheControl{TTL: ttl}
	}
	return &litellm.CacheControl{}
}

func cloneCacheControl(cache *litellm.CacheControl) *litellm.CacheControl {
	if cache == nil {
		return nil
	}
	return &litellm.CacheControl{TTL: cache.TTL}
}

// convertAgentBlocks converts content blocks to litellm blocks. A non-nil
// cache is attached to the LAST converted block only: message-level
// cache_control means "write one breakpoint after this message", and marking
// every block would burn one provider breakpoint per block (Anthropic allows
// at most 4 per request).
func convertAgentBlocks(content []agentgo.ContentBlock, cache *litellm.CacheControl) []litellm.Block {
	blocks := make([]litellm.Block, 0, len(content))
	for _, b := range content {
		switch b.Type {
		case agentgo.ContentText:
			if b.Text != "" {
				blocks = append(blocks, litellm.TextBlock{Text: b.Text, State: toProviderState(b.ProviderState)})
			}
		case agentgo.ContentThinking:
			if b.Thinking != "" || b.ProviderState != nil {
				blocks = append(blocks, litellm.ReasoningBlock{Text: b.Thinking, State: toProviderState(b.ProviderState)})
			}
		case agentgo.ContentImage:
			if b.Image != nil {
				image := convertImageBlock(*b.Image)
				image.State = toProviderState(b.ProviderState)
				blocks = append(blocks, image)
			}
		case agentgo.ContentToolCall:
			if b.ToolCall != nil {
				tc := sanitizeOutgoingToolCall(*b.ToolCall)
				blocks = append(blocks, litellm.ToolUseBlock{
					ID:        tc.ID,
					Name:      tc.Name,
					Arguments: string(tc.Args),
					State:     toProviderState(b.ProviderState),
				})
			}
		case agentgo.ContentToolRef:
			if b.ToolName != "" {
				blocks = append(blocks, litellm.ToolReferenceBlock{ToolName: b.ToolName})
			}
		}
	}
	if cache != nil {
		// Anthropic rejects cache_control on thinking blocks — land the
		// breakpoint on the last cacheable block instead.
		for i := len(blocks) - 1; i >= 0; i-- {
			if _, isReasoning := blocks[i].(litellm.ReasoningBlock); isReasoning {
				continue
			}
			blocks[i] = withBlockCache(blocks[i], cache)
			break
		}
	}
	return blocks
}

// withBlockCache returns a copy of the block with cache_control attached.
// Blocks are value types, so mutating the copy never touches the caller's.
func withBlockCache(b litellm.Block, cache *litellm.CacheControl) litellm.Block {
	cc := cloneCacheControl(cache)
	switch v := b.(type) {
	case litellm.TextBlock:
		v.Cache = cc
		return v
	case litellm.ImageBlock:
		v.Cache = cc
		return v
	case litellm.ToolUseBlock:
		v.Cache = cc
		return v
	case litellm.ToolReferenceBlock:
		v.Cache = cc
		return v
	}
	return b
}

func sanitizeOutgoingToolCall(tc agentgo.ToolCall) agentgo.ToolCall {
	if len(tc.Args) > 0 && json.Valid(tc.Args) {
		return tc
	}
	raw := string(tc.Args)
	if raw == "" {
		raw = tc.ArgsRawText
	}
	n := normalizeArgs(raw)
	tc.Args = n.Args
	if n.Invalid {
		tc.ArgsInvalid = true
		if tc.ArgsRawText == "" {
			tc.ArgsRawText = n.RawText
		}
		if tc.ArgsParseError == "" {
			tc.ArgsParseError = n.ParseErr
		}
	}
	return tc
}

func convertImageBlock(img agentgo.ImageData) litellm.ImageBlock {
	if img.URL != "" {
		return litellm.ImageBlock{URL: img.URL, MIME: img.MimeType}
	}
	return litellm.ImageBlock{URL: "data:" + img.MimeType + ";base64," + img.Data, MIME: img.MimeType}
}

// convertResponse converts litellm.Response to agentgo.Message with content blocks.
func convertResponse(response *litellm.Response) agentgo.Message {
	content := convertResponseContent(response)

	var usage *agentgo.Usage
	if response != nil && (response.Usage.InputTokens != 0 || response.Usage.OutputTokens != 0) {
		provider, model := responseUsageModel(response)
		usage = &agentgo.Usage{
			Provider:    provider,
			Model:       model,
			Input:       response.Usage.InputTokens - response.Usage.CacheWriteTokens,
			Output:      response.Usage.OutputTokens,
			Reasoning:   response.Usage.ReasoningTokens,
			CacheRead:   response.Usage.CacheReadTokens,
			CacheWrite:  response.Usage.CacheWriteTokens,
			TotalTokens: response.Usage.InputTokens + response.Usage.OutputTokens,
		}
	}

	finish := agentgo.StopReasonStop
	if response != nil {
		finish = mapStopReason(response.FinishReason)
	}
	return agentgo.Message{
		Role:       agentgo.RoleAssistant,
		Content:    content,
		StopReason: finish,
		Usage:      usage,
		Metadata:   responseMetadata(response),
	}
}

func responseMetadata(response *litellm.Response) map[string]any {
	if response == nil {
		return nil
	}
	var metadata map[string]any
	if response.FinishReasonRaw != "" {
		if metadata == nil {
			metadata = make(map[string]any)
		}
		metadata["finish_reason_raw"] = response.FinishReasonRaw
	}
	return metadata
}

func mergeResponseMetadata(message *agentgo.Message, response *litellm.Response) {
	if message == nil {
		return
	}
	for key, value := range responseMetadata(response) {
		if message.Metadata == nil {
			message.Metadata = make(map[string]any)
		}
		message.Metadata[key] = value
	}
}

func convertResponseContent(response *litellm.Response) []agentgo.ContentBlock {
	if response == nil {
		return nil
	}
	out := make([]agentgo.ContentBlock, 0, len(response.Blocks))
	for _, block := range response.Blocks {
		if converted, ok := fromProviderBlock(block); ok {
			out = append(out, converted)
		}
	}
	return out
}
func fromProviderBlock(block litellm.Block) (agentgo.ContentBlock, bool) {
	var out agentgo.ContentBlock
	switch b := block.(type) {
	case litellm.TextBlock:
		out = agentgo.TextBlock(b.Text)
		out.ProviderState = fromProviderState(b.State)
	case litellm.ReasoningBlock:
		out = agentgo.ThinkingBlock(b.Text)
		out.ProviderState = fromProviderState(b.State)
	case litellm.ToolUseBlock:
		out = agentgo.ToolCallBlock(buildToolCall(b.ID, b.Name, b.Arguments, ""))
		out.ProviderState = fromProviderState(b.State)
	case litellm.ImageBlock:
		out = agentgo.ContentBlock{Type: agentgo.ContentImage, Image: &agentgo.ImageData{URL: b.URL, Data: base64.StdEncoding.EncodeToString(b.Data), MimeType: b.MIME}, ProviderState: fromProviderState(b.State)}
	default:
		return out, false
	}
	return out, true
}
func toProviderState(state *agentgo.ProviderState) *litellm.ProviderState {
	if state == nil {
		return nil
	}
	return &litellm.ProviderState{Provider: state.Provider, Model: state.Model, Data: append(json.RawMessage(nil), state.Data...)}
}
func fromProviderState(state *litellm.ProviderState) *agentgo.ProviderState {
	if state == nil {
		return nil
	}
	return &agentgo.ProviderState{Provider: state.Provider, Model: state.Model, Data: append(json.RawMessage(nil), state.Data...)}
}
func responseUsageModel(response *litellm.Response) (string, string) {
	if response == nil {
		return "", ""
	}
	return response.Provider, response.Model
}

func pricingForUsage(fallback *ModelPricing, usage *agentgo.Usage) *ModelPricing {
	return fallback
}

// mapStopReason maps litellm canonical FinishReason to agentgo StopReason.
func mapStopReason(reason litellm.FinishReason) agentgo.StopReason {
	switch reason {
	case litellm.FinishReasonStop, litellm.FinishReason(""):
		return agentgo.StopReasonStop
	case litellm.FinishReasonLength:
		return agentgo.StopReasonLength
	case litellm.FinishReasonToolCall:
		return agentgo.StopReasonToolUse
	case litellm.FinishReasonError:
		return agentgo.StopReasonError
	case litellm.FinishReasonSafety:
		return agentgo.StopReasonSafety
	default:
		return agentgo.StopReason(string(reason))
	}
}

type apiKeyOverrideKey struct{}

func contextWithAPIKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, apiKeyOverrideKey{}, key)
}

func apiKeyFunc(defaultKey string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if key, ok := ctx.Value(apiKeyOverrideKey{}).(string); ok && key != "" {
			return key, nil
		}
		return defaultKey, nil
	}
}

func applySamplingConfig(req *litellm.Request, cfg *GenerationConfig) {
	if req == nil || cfg == nil {
		return
	}
	if cfg.Temperature != DefaultGenerationConfig.Temperature {
		req.Temperature = &cfg.Temperature
	}
}

// applyCallConfig resolves CallOptions once and applies API key, thinking,
// session config, cache routing, and response format to the litellm request.
// Native options are validated by litellm; this adapter keeps no vendor whitelist.
func applyCallConfig(ctx context.Context, req *litellm.Request, opts []agentgo.CallOption) (context.Context, error) {
	cfg := agentgo.ResolveCallConfig(opts)
	ctx = contextWithAPIKey(ctx, cfg.APIKey)
	if cfg.ThinkingBudget < 0 {
		return ctx, fmt.Errorf("llm: thinking budget cannot be negative")
	}
	if cfg.ThinkingBudget > 0 && cfg.ThinkingLevel == agentgo.ThinkingOff {
		return ctx, fmt.Errorf("llm: thinking budget cannot be set when thinking is off")
	}
	if cfg.ThinkingLevel != "" || cfg.ThinkingBudget > 0 {
		// Explicit typed settings replace raw defaults; wire mapping and acceptance
		// of particular efforts or budgets remain entirely in litellm/the endpoint.
		for _, key := range []string{"thinking", "reasoning_effort", "reasoning", "enable_thinking", "thinking_budget"} {
			delete(req.ProviderOptions, key)
		}
		if raw, ok := req.ProviderOptions["output_config"]; ok {
			var output map[string]json.RawMessage
			if json.Unmarshal(raw, &output) == nil {
				delete(output, "effort")
				data, _ := json.Marshal(output)
				req.ProviderOptions["output_config"] = data
			}
		}
		req.Thinking = &litellm.Thinking{Disabled: cfg.ThinkingLevel == agentgo.ThinkingOff}
		if !req.Thinking.Disabled {
			req.Thinking.Effort = string(cfg.ThinkingLevel)
		}
		if cfg.ThinkingBudget > 0 {
			req.Thinking.BudgetTokens = &cfg.ThinkingBudget
		}
	}
	if req.ProviderOptions == nil {
		req.ProviderOptions = make(litellm.ProviderOptions)
	}
	if cfg.SessionID != "" {
		if err := req.ProviderOptions.Set("session_id", cfg.SessionID); err != nil {
			return ctx, err
		}
	}
	if cfg.PromptCacheKey != "" {
		if err := req.ProviderOptions.Set("prompt_cache_key", cfg.PromptCacheKey); err != nil {
			return ctx, err
		}
	}
	if cfg.MaxTokens > 0 {
		req.MaxTokens = &cfg.MaxTokens
	}
	if cfg.Temperature != nil {
		req.Temperature = cfg.Temperature
	}
	if cfg.ToolChoice != nil {
		choice, err := convertToolChoice(cfg.ToolChoice)
		if err != nil {
			return ctx, err
		}
		req.ToolChoice = choice
	}
	if cfg.ResponseFormat != nil {
		format, err := convertResponseFormat(cfg.ResponseFormat)
		if err != nil {
			return ctx, err
		}
		req.ResponseFormat = format
	}
	return ctx, nil
}
func convertToolChoice(value any) (*litellm.ToolChoice, error) {
	switch v := value.(type) {
	case string:
		return &litellm.ToolChoice{Mode: litellm.ToolChoiceMode(v)}, nil
	case map[string]any:
		if name, _ := v["name"].(string); name != "" {
			return &litellm.ToolChoice{Name: name}, nil
		}
		if fn, ok := v["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				return &litellm.ToolChoice{Name: name}, nil
			}
		}
		if mode, _ := v["mode"].(string); mode != "" {
			return &litellm.ToolChoice{Mode: litellm.ToolChoiceMode(mode)}, nil
		}
	}
	return nil, fmt.Errorf("llm: unsupported tool choice %T", value)
}

func convertResponseFormat(format *agentgo.ResponseFormat) (*litellm.ResponseFormat, error) {
	if format == nil {
		return nil, nil
	}
	out := &litellm.ResponseFormat{
		Type: litellm.ResponseFormatType(format.Type),
	}
	if format.JSONSchema != nil {
		schema, err := litellm.SchemaFrom(format.JSONSchema.Schema)
		if err != nil {
			return nil, fmt.Errorf("llm: response format schema: %w", err)
		}
		out.JSONSchema = &litellm.JSONSchema{
			Name:        format.JSONSchema.Name,
			Description: format.JSONSchema.Description,
			Schema:      schema,
			Strict:      format.JSONSchema.Strict,
		}
	}
	return out, nil
}

func applyToolConfig(request *litellm.Request, tools []agentgo.ToolSpec) error {
	if len(tools) == 0 {
		return nil
	}
	ltTools := make([]litellm.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		schema, err := litellm.SchemaFrom(t.Parameters)
		if err != nil {
			return fmt.Errorf("llm: tool %q schema: %w", t.Name, err)
		}
		ltTools = append(ltTools, litellm.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
			Strict:      t.Strict, Deferred: t.DeferLoading,
		})
	}
	request.Tools = ltTools
	return nil
}

func newProvider(name string, cfg ProviderConfig) (litellm.Provider, error) {
	headers, err := headersFromExtra(cfg.Extra)
	if err != nil {
		return nil, err
	}
	client := retry.NewHTTPClient(&http.Client{Transport: cfg.Transport}, cfg.Retry)
	pcfg := provider.Config{APIKeyFunc: apiKeyFunc(cfg.APIKey), BaseURL: cfg.BaseURL, HTTPClient: client, Headers: headers, UserAgent: stringFromExtra(cfg.Extra, "user_agent"), Name: stringFromExtra(cfg.Extra, "name"), API: firstStringFromExtra(cfg.Extra, "api", "api_mode"), AllowUnknownProviderOptions: true}
	if name == "bedrock" {
		pcfg.Region = firstStringFromExtra(cfg.Extra, "region", "aws_region")
		pcfg.Credentials = bedrock.StaticCredentials(firstStringFromExtra(cfg.Extra, "access_key_id", "aws_access_key_id"), firstStringFromExtra(cfg.Extra, "secret_access_key", "aws_secret_access_key"), firstStringFromExtra(cfg.Extra, "session_token", "aws_session_token"))
	}
	return provider.New(strings.ToLower(strings.TrimSpace(name)), pcfg)
}

func headersFromExtra(extra map[string]any) (map[string]string, error) {
	raw, ok := extra["headers"]
	if !ok || raw == nil {
		return nil, nil
	}
	switch h := raw.(type) {
	case map[string]string:
		return maps.Clone(h), nil
	case map[string]any:
		out := make(map[string]string, len(h))
		for k, v := range h {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("llm: provider header %q must be string", k)
			}
			out[k] = s
		}
		return out, nil
	default:
		return nil, fmt.Errorf("llm: provider headers must be map[string]string")
	}
}

func stringFromExtra(extra map[string]any, key string) string {
	v, _ := extra[key].(string)
	return v
}

func firstStringFromExtra(extra map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringFromExtra(extra, key); value != "" {
			return value
		}
	}
	return ""
}

// normalizedArgs is the parsed shape of raw LLM tool-call args.
//   - Args is always valid JSON so the parent ToolCall stays JSON-serializable
//     (json.RawMessage marshalling validates its bytes; invalid args here would
//     break agent.ExportMessages → json.Marshal for persistence).
//   - When the raw payload was malformed, Args is the "{}" placeholder, and
//     RawText + ParseErr carry the original bytes and parser diagnostic so
//     downstream validation can point at the true root cause (stream
//     truncation, provider bug) instead of "missing field" against {}.
type normalizedArgs struct {
	Args     json.RawMessage
	Invalid  bool
	RawText  string
	ParseErr string
}

func normalizeArgs(raw string) normalizedArgs {
	if raw == "" {
		return normalizedArgs{Args: json.RawMessage("{}")}
	}
	if json.Valid([]byte(raw)) {
		return normalizedArgs{Args: json.RawMessage(raw)}
	}
	var probe any
	parseErr := json.Unmarshal([]byte(raw), &probe)
	return normalizedArgs{
		Args:     json.RawMessage("{}"),
		Invalid:  true,
		RawText:  raw,
		ParseErr: parseErr.Error(),
	}
}

// buildToolCall constructs an agentgo.ToolCall from raw litellm fields,
// routing malformed args into dedicated diagnostic fields (see ToolCall doc).
func buildToolCall(id, name, rawArgs, thoughtSignature string) agentgo.ToolCall {
	n := normalizeArgs(rawArgs)
	return agentgo.ToolCall{
		ID:               id,
		Name:             name,
		Args:             n.Args,
		ArgsInvalid:      n.Invalid,
		ArgsRawText:      n.RawText,
		ArgsParseError:   n.ParseErr,
		ThoughtSignature: thoughtSignature,
	}
}

// recordModelSettings captures the adapter's resolved request, not an assertion
// about a provider's internal effort mapping. No raw provider options are logged.
func recordModelSettings(message *agentgo.Message, req *litellm.Request) {
	settings := agentgo.ModelCallSettings{}
	if req.MaxTokens != nil {
		settings.MaxTokens = *req.MaxTokens
	}
	if req.Thinking != nil {
		if req.Thinking.Disabled {
			settings.ThinkingLevel = agentgo.ThinkingOff
		} else {
			settings.ThinkingLevel = agentgo.ThinkingLevel(req.Thinking.Effort)
		}
		if req.Thinking.BudgetTokens != nil {
			settings.ThinkingBudget = *req.Thinking.BudgetTokens
		}
	}
	if message.Metadata == nil {
		message.Metadata = make(map[string]any)
	}
	message.Metadata["model_request_settings"] = map[string]any{
		"thinking_level":  string(settings.ThinkingLevel),
		"thinking_budget": settings.ThinkingBudget,
		"max_tokens":      settings.MaxTokens,
	}
}
