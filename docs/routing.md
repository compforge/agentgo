# Model routing

`llm.Router` implements `agentgo.ChatModel` by composing configured models. The
application chooses the candidate pool, credentials, task effort and acceptable
substitutions. The router owns availability: priority or round-robin selection,
shared cooldown and bounded failover. It works in both AgentLoop and direct calls.

```go
primary, err := llm.NewModel("compat", "primary-model",
    llm.WithBaseURL(primaryURL), llm.WithAPIKey(primaryKey))
// Handle err, then construct backup in the same way.
router, err := llm.NewRouter([]llm.Route{
    {Name: "primary", Model: primary},
    {Name: "backup", Model: backup},
}, llm.RouterConfig{
    Policy: llm.RoutePriority,
    CallTimeout: 180 * time.Second,
    AttemptTimeout: 60 * time.Second,
})
// Handle err.
executor := agentgo.NewModelExecutor(agentgo.ModelExecutorConfig{
    Middlewares: middlewares, OnEvent: observeEvent,
})
result, err := executor.Call(ctx, router, agentgo.ModelExecution{
    Execution: agentgo.Execution{ID: "review", Kind: agentgo.ExecutionKindModel, Attempt: 1},
    Request: agentgo.LLMRequest{
        Messages: messages, Tools: tools,
        Config: &agentgo.CallConfig{ThinkingLevel: agentgo.ThinkingMedium},
    },
})
```

## Budgets and failures

One call has one total deadline. Each endpoint's attempt deadline is bounded by
that deadline and the parent context. Zero timeouts inherit the parent context;
applications should supply a bounded context or CallTimeout. MaxAttempts defaults
to the number of routes, and every endpoint is tried at most once. An endpoint
can have a stricter provider request timeout.

Transient provider failures and explicitly retryable provider errors may switch
endpoints. Cancellation, context overflow, authentication, request validation and
explicitly non-retryable errors stop routing. Unknown errors do not automatically
switch endpoints. Failed endpoints cool down for 30 seconds by default; a negative
Cooldown disables this. Cooling endpoints are placed last, so an entirely cooling
pool remains usable as a last resort.

Router owns its attempt budget. NewModel constructs providers without HTTP retry
wrappers; configure externally supplied clients the same way. WithRequestTimeout
and WithStreamIdleTimeout control transport deadlines; RouterConfig controls failover.
An exhausted or terminal routing call returns RoutingError
with Retryable false, preserving its underlying cause for inspection. AgentLoop
honors this even when the cause is a partial stream; it does not restart the pool.
Context overflow remains eligible for the loop's separate compaction recovery.

`StopReasonLength` is a response, not a failover error. Partial content and usage
return to the caller. AgentLoop's length recovery budget controls continuation.

Streaming may fail over only before publishing any stream event. After publication,
the caller receives an error and must decide how to handle the partial output.
The router never concatenates output from two endpoints. Cancellation closes the
active attempt; member models must honor their contexts.

## Facts and configuration

Each finished endpoint attempt emits `model_attempt` through the model executor,
inside AgentLoop or standalone `ModelExecutor.Call`, and can be saved by `session`.
ModelAttempt.Index is nested inside Execution.ID and
Execution.Attempt; it does not replace those execution coordinates. Facts include
the safe endpoint label, duration, requested model, reported usage when available,
error kind, timeout scope and whether another endpoint will be tried.
RouterConfig.Observe is an optional router-local observer even without a model
executor. Use the executor or Loop event stream for unified call recording, and avoid counting
both observers as separate attempts. Observe may run
concurrently and must return promptly. Endpoint labels must not contain secrets.

Successful messages carry `Metadata["route_endpoint"]`. Usage retains the actual
provider/model attribution. Attempt usage is diagnostic: it overlaps the successful
response's usage and must not be summed with that same response twice. Failed
requests without provider usage remain unknown, not zero-cost.

The final Request.Config is projected to ChatModel options once, after middleware,
and forwarded unchanged to every selected candidate. Providers validate
supported thinking settings; routing never lowers effort to make a fallback work.

## Provider boundary

AgentGo uses litellm v1.9.6 and requires Go 1.26 or newer. Provider construction,
wire serialization, effort mapping, stream aggregation, errors and opaque replay
state belong to litellm. AgentGo adapts its message contract and owns routing.

`compat` uses litellm's generic Chat Completions protocol. Effort values pass
through unchanged; off/none disables thinking. The endpoint decides which values
it accepts. Native request defaults use `WithExtra`; explicit per-call thinking
replaces raw thinking defaults. No model effort whitelist is maintained here.
Capabilities exposes only thinking
facts used by ThinkingPolicy: support, disable and optional explicitly known effort
values. A provider baseline is advisory and cannot prove model-specific acceptance.

`WithHTTPTransport` reuses a host-owned transport. `WithProviderExtra` configures
the factory (for example `name` and `headers`). Give distinct endpoints distinct
names when signed or encrypted history must remain isolated. ContentBlock's opaque
ProviderState is the sole carrier of provider replay state, including reasoning
signatures. It survives persistence and replay; litellm decides whether that state
belongs to the selected endpoint/model.

Cache breakpoints remain explicit message metadata. PromptCacheKey and SessionID
are native provider hints, validated by the selected litellm provider; applications
must only set supported hints. Sending a hint does not prove a cache hit.
NewModel preserves a 120-second stream idle timeout; WithStreamIdleTimeout can
change or disable it. Request and router budgets remain independent of this watchdog.
