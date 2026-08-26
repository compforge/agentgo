# AgentGo

**AgentGo** is a minimal, composable Go library for building AI agent applications.

AgentGo evolved from [AgentCore](https://github.com/voocel/agentcore) and now develops independently.

[English](README.md) | [中文](README_CN.md)

## What it provides

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

`ContextItemProvider` lets an application message expose identifiable information without changing its model rendering. Before each model call, `EventContextProjected` reports the inventory from the actual projected context. `ContextItem` and `ContextDemand` share `ContextKey`; applications and evaluators own all label meanings and demand-extraction rules.

`AgentState` is codec-aware without being tied to storage or transport. Its portable projection includes committed messages, loop continuation, and steering/follow-up accepted by AgentGo but not yet consumed. `agentgo.NewCodec` registers AgentGo's built-in state types; applications register their own concrete `AgentMessage` types with one stable type ID. Fields opt in through `codec` tags, while custom handlers cover special wire representations. Hosts can persist state from `AfterTurn` / `AfterRun` or the corresponding events for process handoff and recovery; durable ingress before AgentGo accepts a message remains host-owned.

```go
stateCodec, _ := agentgo.NewCodec(
    codec.Type[*ProgressMessage]("example.progress-message.v1"),
)

data, _ := stateCodec.Marshal(agent.State())

var state agentgo.AgentState
_ = stateCodec.Unmarshal(data, &state)
```

`Execution` gives expensive or externally visible work one run-scoped identity. A retry keeps the same `ID` and increments `Attempt`; `ModelExecution` and `ToolExecution` carry that coordinate through middleware and the Event stream. Internal summary calls are child executions of compaction, so hosts can correlate or replay known outcomes without AgentGo depending on a ledger or tracing model.

## Extension points

| Need | Contract |
|------|----------|
| Model provider | `ChatModel` |
| Application message | `AgentMessage` |
| Tool capability | `Tool` and optional tool interfaces |
| Tool authorization | `ToolGate` |
| Context projection and recovery | `ContextManager` |
| Compaction policy | `context.Compactor` |
| Run state injection and finalization | `WithBeforeRun` / `WithAfterRun` |
| Turn preparation and state observation | `WithBeforeTurn` / `WithAfterTurn` |
| Model execution interception | `WithModelMiddlewares` |
| Tool execution interception | `WithToolMiddlewares` |
| Stop policy | `StopGuard` |
| UI, logging, and trajectory capture | `<-chan Event` / `Agent.Subscribe` |

Built-in packages include typed state encoding under `codec/`, model adapters under `llm/`, context strategies under `context/`, coding tools under `tools/`, and optional `subagent/`, `team/`, `task/`, `proxy/`, and `permission/` capabilities.

## Design and API

- [`docs/kernel.md`](docs/kernel.md) — message-native boundaries and trajectory-driven loop optimization.
- [Go package documentation](https://pkg.go.dev/github.com/compforge/agentgo) — complete public API.
- [`examples/`](examples/) — runnable single-agent and multi-agent examples.

## Stability

`Agent`, `AgentLoop`, `Event`, `Tool`, `AgentMessage`, and `Message` are the primary stable surface. Examples and internal implementation details may evolve faster.

## License

Apache License 2.0
