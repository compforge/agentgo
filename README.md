# AgentGo

**AgentGo** is a minimal, composable Go library for building AI agent applications.

AgentGo evolved from [AgentCore](https://github.com/voocel/agentcore) and now develops independently.

[English](README.md) | [中文](README_CN.md)

## What it provides

- A message-native Agent Loop: applications keep `AgentMessage`; model-level `Message` exists only at the call boundary.
- A single event stream for model output, tools, context projection and compaction, retries, and completion.
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

## Extension points

| Need | Contract |
|------|----------|
| Model provider | `ChatModel` |
| Application message | `AgentMessage` |
| Tool capability | `Tool` and optional tool interfaces |
| Tool authorization | `ToolGate` |
| Context projection and recovery | `ContextManager` |
| Compaction policy | `context.Compactor` |
| Turn preparation and observation | `WithBeforeTurn` / `WithAfterTurn` |
| Model-call options and validation | `WithBeforeModelCall` / `WithAfterModelCall` |
| Stop policy | `StopGuard` |
| UI, logging, and trajectory capture | `<-chan Event` / `Agent.Subscribe` |

Built-in packages include model adapters under `llm/`, context strategies under `context/`, coding tools under `tools/`, and optional `subagent/`, `team/`, `task/`, `proxy/`, and `permission/` capabilities.

## Design and API

- [`docs/kernel.md`](docs/kernel.md) — message-native boundaries and trajectory-driven loop optimization.
- [Go package documentation](https://pkg.go.dev/github.com/compforge/agentgo) — complete public API.
- [`examples/`](examples/) — runnable single-agent and multi-agent examples.

## Stability

`Agent`, `AgentLoop`, `Event`, `Tool`, `AgentMessage`, and `Message` are the primary stable surface. Examples and internal implementation details may evolve faster.

## License

Apache License 2.0
