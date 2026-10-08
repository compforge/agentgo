# AgentGo

**AgentGo** is a minimal, composable Go library for building AI agent applications.

AgentGo evolved from [AgentCore](https://github.com/voocel/agentcore) and now develops independently.

[English](README.md) | [中文](README_CN.md)

## What it provides

- Extensible artifacts alongside messages: share material through run hooks, tools, and middleware, with application-defined prompt rendering.
- A message-native Agent Loop: applications keep `AgentMessage`; model-level `Message` exists only at the call boundary.
- A single event stream for model output, tools, context projection and compaction, retries, and completion.
- One execution coordinate across middleware and events for model, tool, and compaction work.
- Replaceable models, tools, context management, compaction, stop guards, turn hooks, and permission gates.
- Stateful `Agent` and standalone `AgentLoop` entry points over the same execution kernel.
- Steering, follow-up, background tasks, sub-agents, and multi-agent team primitives.
- Trajectory-ready context contracts: `ContextItem` records what the projected context contains, while `ContextDemand` provides the matching application-neutral demand shape.

The kernel stays policy-light: applications decide what information means, which tools are allowed, when work is complete, and how trajectories are evaluated.

## Install

```bash
go get github.com/compforge/agentgo
```

## Quick Start

```go
package main

import (
    "fmt"
    "os"

    "github.com/compforge/agentgo"
    "github.com/compforge/agentgo/llm"
    "github.com/compforge/agentgo/tools"
)

func main() {
    model, _ := llm.NewModel(
        "openai",
        "gpt-5-mini",
        llm.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
    )

    agent := agentgo.NewAgent(
        agentgo.WithModel(model),
        agentgo.WithSystemPrompt("You are a helpful coding assistant."),
        agentgo.WithTools(tools.NewRead(".", tools.NewFileReadState())),
    )

    agent.Subscribe(func(event agentgo.Event) {
        if event.Type == agentgo.EventMessageEnd {
            if message, ok := event.Message.(agentgo.Message); ok && message.Role == agentgo.RoleAssistant {
                fmt.Println(message.TextContent())
            }
        }
    })

    agent.Prompt("Summarize this repository.")
    agent.WaitForIdle()
}
```

## Core flow

```text
AgentMessage
    ─ContextManager / Compactor─▶ projected AgentMessage
    ─ToMessage─▶ model Message
    ─Model / Tool─▶ Event stream
    ─commit─▶ AgentMessage history
```

`Artifact` represents application-owned material independently of the transcript. Implement its `ID()` and `Kind()` methods with your own payload type, then use the runtime-provided `Artifacts` capability to add, get, list, replace, or delete values. IDs are unique within a manager; kinds and content remain application-defined.

Callers still supply messages. AgentGo creates and owns the manager. A `WithBeforeRun` hook can extract initial artifacts through `run.Artifacts`; turn hooks and model/tool middleware receive the same capability. A `context.Transformer` receives `TransformContext{Messages, Artifacts}` to choose how material enters each request—for example, one expansion with references at later occurrences. AgentGo supplies the mechanism; extraction, message associations, rendering, and persistence timing belong to the application. Material survives consecutive runs and message compaction; `AgentState.Artifacts` carries its values through snapshots and codec-based restoration. The [offline artifact example](examples/artifacts) demonstrates the complete flow without an API key:

```bash
go run ./examples/artifacts
```

`ContextItemProvider` lets an application message expose identifiable information without changing its model rendering. Before each model call, `EventContextProjected` reports the inventory from the actual projected context. `ContextItem` and `ContextDemand` share `ContextKey`; applications and evaluators own all label meanings and demand-extraction rules.

`AgentState` is the Loop-owned execution state. A stateful `Agent` exposes `AgentSnapshot`, which adds steering and follow-up input already accepted by the Agent but not yet handed to the Loop. Both are codec-aware without being tied to storage or transport. `agentgo.NewCodec` registers AgentGo's built-in state types; applications register their own concrete `AgentMessage` and `Artifact` types with one stable type ID. Fields opt in through `codec` tags, while custom handlers cover special wire representations. Hosts can use the same encoded snapshot for persistence, process handoff, or future RPC protocols.

```go
stateCodec, _ := agentgo.NewCodec(
    codec.Type[*ProgressMessage]("example.progress-message.v1"),
)

data, _ := stateCodec.Marshal(agent.Snapshot())
_ = snapshotStore.Save(ctx, data)

restored := agentgo.NewAgent(
    agentgo.WithModel(model),
    agentgo.WithSnapshotLoader(func(ctx context.Context, run agentgo.SnapshotLoadContext) (agentgo.AgentSnapshot, error) {
        data, err := snapshotStore.Load(ctx)
        if err != nil {
            return agentgo.AgentSnapshot{}, err
        }
        var snapshot agentgo.AgentSnapshot
        if err := stateCodec.Unmarshal(data, &snapshot); err != nil {
            return agentgo.AgentSnapshot{}, err
        }
        return snapshot, nil
    }),
    agentgo.WithAfterRun(func(ctx context.Context, run agentgo.AfterRunContext) error {
        data, err := stateCodec.Marshal(run.Snapshot)
        if err != nil {
            return err
        }
        return snapshotStore.Save(ctx, data)
    }),
)
_ = restored.Continue(ctx)
```

`WithSnapshotLoader` recovers the baseline before `BeforeRun` initializes its materials. `BeforeRun` returns only an error and receives an isolated material collection populated from the recovered snapshot. Successful preparation publishes messages, queues and materials together; loading or initialization failure leaves the accepted state unchanged, so a later `Continue` can retry. `AfterRun` observes the completed run outside the Loop and before terminal listeners. Recovery adapters can pair a snapshot loader with `AfterRun` persistence.

`Execution` gives expensive or externally visible work one run-scoped identity. A retry keeps the same `ID` and increments `Attempt`; `ModelExecution` and `ToolExecution` carry that coordinate through middleware and the Event stream. Internal summary calls are child executions of compaction, so hosts can correlate or replay known outcomes without AgentGo depending on a ledger or tracing model.

## Extension points

| Need | Contract |
|------|----------|
| Model provider | `ChatModel` |
| Application message | `AgentMessage` |
| Application material | `Artifact` / `ArtifactManager` |
| Tool capability | `Tool` and optional tool interfaces |
| Tool authorization | `ToolGate` |
| Context projection and recovery | `ContextManager` |
| Compaction policy | `context.Compactor` |
| Stateful Agent restoration and finalization | `AgentSnapshot` / `WithSnapshotLoader` / `WithBeforeRun` / `WithAfterRun` |
| Turn preparation and state observation | `WithBeforeTurn` / `WithAfterTurn` |
| Model execution interception | `WithModelMiddlewares` |
| Tool execution interception | `WithToolMiddlewares` |
| Stop policy | `StopGuard` |
| UI, logging, and trajectory capture | `<-chan Event` / `Agent.Subscribe` |

Lifecycle events carry source timestamps and execution coordinates, including context preparation, retry waits, tool scheduling, and actual invocation. Middleware propagates derived contexts into model and tool calls. See the [lifecycle contract](docs/kernel.md#扩展与生命周期事实) for failure, cancellation, and committed-turn semantics.

Built-in packages include typed state encoding under `codec/`, model adapters under `llm/`, context strategies under `context/`, coding tools under `tools/`, and optional `subagent/`, `team/`, `task/`, `proxy/`, and `permission/` capabilities.

## Design and API

- [`docs/kernel.md`](docs/kernel.md) — message-native boundaries and trajectory-driven loop optimization.
- [Go package documentation](https://pkg.go.dev/github.com/compforge/agentgo) — complete public API.
- [`examples/`](examples/) — runnable single-agent and multi-agent examples.

## Stability

`Agent`, `AgentLoop`, `Event`, `Tool`, `AgentMessage`, and `Message` are the primary stable surface. Examples and internal implementation details may evolve faster.

## License

Apache License 2.0
