# AgentGo

**AgentGo** 是一个极简、可组合的 Go Agent 核心库，用于构建任意 AI Agent 应用。

AgentGo 从 [AgentCore](https://github.com/voocel/agentcore) 发展而来，现在独立演进。

[English](README.md) | [中文](README_CN.md)

## 核心能力

- Message-native Agent Loop：应用始终持有 `AgentMessage`，只在模型调用边界转换为 `Message`。
- 单一 Event stream：统一承载模型输出、工具、Context 投影与压缩、重试和结束状态。
- Model、Tool、ContextManager、Compactor、StopGuard、Turn Hook 和权限 Gate 均可替换。
- 同一执行内核同时提供有状态 `Agent` 与无状态 `AgentLoop` 两种入口。
- 支持 steering、follow-up、后台任务、SubAgent 与多 Agent Team。
- 面向轨迹的 Context 协议：`ContextItem` 记录投影后的 Context 拥有什么，`ContextDemand` 提供与之配对且不带业务解释的需求形状。

内核保持策略克制：信息含义、工具权限、完成条件和轨迹评价均由应用决定。

## 安装

```bash
go get github.com/compforge/agentgo
```

## 快速开始

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
        agentgo.WithSystemPrompt("你是一个编程助手。"),
        agentgo.WithTools(tools.NewRead(".", tools.NewFileReadState())),
    )

    agent.Subscribe(func(event agentgo.Event) {
        if event.Type == agentgo.EventMessageEnd {
            if message, ok := event.Message.(agentgo.Message); ok && message.Role == agentgo.RoleAssistant {
                fmt.Println(message.TextContent())
            }
        }
    })

    agent.Prompt("总结这个代码仓。")
    agent.WaitForIdle()
}
```

## 核心流程

```text
AgentMessage
    ─ContextManager / Compactor─▶ projected AgentMessage
    ─ToMessage─▶ model Message
    ─Model / Tool─▶ Event stream
    ─commit─▶ AgentMessage history
```

应用消息可通过 `ContextItemProvider` 暴露有稳定身份的信息，而不改变模型渲染。每次模型调用前，`EventContextProjected` 会报告实际投影后的 Context 清单。`ContextItem` 与 `ContextDemand` 共享 `ContextKey`；标签含义及 Demand 提取规则由应用和 Evaluator 负责。

## 扩展点

| 需求 | 契约 |
|------|------|
| 模型 Provider | `ChatModel` |
| 应用消息 | `AgentMessage` |
| 工具能力 | `Tool` 及其可选接口 |
| 工具授权 | `ToolGate` |
| Context 投影与恢复 | `ContextManager` |
| 压缩策略 | `context.Compactor` |
| Turn 准备与观察 | `WithBeforeTurn` / `WithAfterTurn` |
| 终止策略 | `StopGuard` |
| UI、日志与轨迹采集 | `<-chan Event` / `Agent.Subscribe` |

内置包包括 `llm/` 模型适配、`context/` 上下文策略、`tools/` 编程工具，以及可选的 `subagent/`、`team/`、`task/`、`proxy/` 和 `permission/` 能力。

## 设计与 API

- [`docs/kernel.md`](docs/kernel.md) —— Message-native 边界与轨迹驱动的 Loop 优化。
- [Go Package 文档](https://pkg.go.dev/github.com/compforge/agentgo) —— 完整公开 API。
- [`examples/`](examples/) —— 可运行的单 Agent 与多 Agent 示例。

## 稳定性

`Agent`、`AgentLoop`、`Event`、`Tool`、`AgentMessage` 与 `Message` 是优先稳定的公开面；示例和内部实现细节可能更快演进。

## 许可证

Apache License 2.0
