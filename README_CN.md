# AgentGo

**AgentGo** 是一个极简、可组合的 Go Agent 核心库，用于构建任意 AI Agent 应用。

AgentGo 从 [AgentCore](https://github.com/voocel/agentcore) 发展而来，现在独立演进。

[English](README.md) | [中文](README_CN.md)

## 核心能力

- 与消息并列的可扩展 Artifact：通过 Run Hook、Tool 和 Middleware 管理材料，由业务决定如何呈现到模型输入。
- Message-native Agent Loop：应用始终持有 `AgentMessage`，只在模型调用边界转换为 `Message`。
- 单一 Event stream：统一承载模型输出、工具、Context 投影与压缩、重试和结束状态。
- Middleware 与 Event 共享同一个执行坐标，统一关联模型、工具与压缩动作。
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
    ─Compact + accept─▶ baseline AgentMessage
    ─Transformer─▶ request AgentMessage
    ─ToMessage─▶ model Message
    ─Model / Tool─▶ Event stream
    ─commit─▶ AgentMessage history
```

`Artifact` 表达独立于消息历史的业务材料。应用通过实现 `ID()` 和 `Kind()` 定义自己的内容类型，通过运行时提供的 `Artifacts` 能力新增、读取、列举、替换或删除材料。ID 在一个 Manager 内唯一，分类和内容含义由业务定义。

调用方仍然只需传入消息。AgentGo 创建并管理 Manager；业务在 `WithBeforeRun` 中通过 `run.Artifacts` 提取初始材料，Turn Hook 和 Model/Tool Middleware 接收相同能力。`context.Transformer` 通过 `TransformContext{Messages, Artifacts}` 决定材料如何进入每次模型请求，例如只展开一次内容、在其他位置保留引用。材料随同一个 Agent 跨 Run 保留，消息压缩不清空材料；`AgentState.Artifacts` 支持快照与 codec 恢复。提取、消息关联、呈现和持久化时机由应用负责。[离线示例](examples/artifacts) 无需 API key 即可展示完整流程：

```bash
go run ./examples/artifacts
```

应用消息可通过 `ContextItemProvider` 暴露有稳定身份的信息，而不改变模型渲染。每次模型调用前，`EventContextProjected` 会报告实际投影后的 Context 清单。`ContextItem` 与 `ContextDemand` 共享 `ContextKey`；标签含义及 Demand 提取规则由应用和 Evaluator 负责。

`AgentState` 是 Loop 自己拥有的执行状态。对于 stateful `Agent`，`AgentSnapshot` 还会聚合已经被 Agent 接受、但尚未交给 Loop 的 steering 和 follow-up 输入。两者都感知 codec，但不绑定存储或传输方式。`agentgo.NewCodec` 会注册 AgentGo 内置状态类型；应用只需用一个稳定 TypeID 注册自己的具体 `AgentMessage` 和 `Artifact` 类型。字段通过 `codec` tag 主动参与编码，特殊 wire 表示则使用自定义 Handler。同一份编码快照可用于持久化、进程交接或未来的 RPC 协议。

使用 `agentgo/context` 的压缩策略时，将 `context.CodecOptions()` 加入 codec 配置，以保留压缩表示、Raw 和 usage 失效状态。Compact 和 Transformer 均可访问 messages 与 artifacts：前者维护长期基线，后者在每次模型请求前构造视图，与预算无关。

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

`WithSnapshotLoader` 先恢复基线，再由 `BeforeRun` 初始化材料。`BeforeRun` 只返回错误，其材料集合独立于正式状态，并已包含恢复的数据。准备成功后，消息、队列与材料一起提交；加载或初始化失败则保留原有状态，后续 `Continue` 可以重试。`AfterRun` 在 Loop 之外观察完成的 Run，且先于终态 listener。恢复 adapter 可组合 SnapshotLoader 与 `AfterRun` 持久化。

Hook 参数中的 State / Snapshot 是回调入口快照。`AfterTurn` 写入的材料进入随后的 turn checkpoint；`AfterRun` 写入的材料进入终态事件。若收尾 Hook 会修改材料，需在终态 listener 中读取 `agent.Snapshot()` 保存最终状态。

`Execution` 为昂贵或对外可见的动作提供一次 Run 内稳定的身份。同一逻辑调用重试时保持 `ID` 不变并递增 `Attempt`；`ModelExecution` 与 `ToolExecution` 把该坐标贯穿 Middleware 和 Event stream。内部 summary 是 compaction 的子 Execution，宿主因此可以关联动作或复用已知结果，而 AgentGo 无需绑定 Ledger 或 Trace 的数据模型。

## 扩展点

| 需求 | 契约 |
|------|------|
| 模型 Provider | `ChatModel` |
| 应用消息 | `AgentMessage` |
| 业务材料 | `Artifact` / `ArtifactManager` |
| 工具能力 | `Tool` 及其可选接口 |
| 工具授权 | `ToolGate` |
| Context 投影与恢复 | `ContextManager` |
| 压缩策略 | `context.Compactor` |
| Stateful Agent 恢复与收尾 | `AgentSnapshot` / `WithSnapshotLoader` / `WithBeforeRun` / `WithAfterRun` |
| Turn 准备与状态观察 | `WithBeforeTurn` / `WithAfterTurn` |
| 模型执行拦截 | `WithModelMiddlewares` |
| 工具执行拦截 | `WithToolMiddlewares` |
| 终止策略 | `StopGuard` |
| UI、日志与轨迹采集 | `<-chan Event` / `Agent.Subscribe` |

生命周期事件携带源头时间与执行坐标，覆盖上下文准备、重试等待、工具调度和实际调用；Middleware 派生的 context 会传入模型与工具。失败、取消和 turn 提交的边界见[生命周期契约](docs/kernel.md#扩展与生命周期事实)。

内置包包括 `codec/` 类型化状态编码、`llm/` 模型适配、`context/` 上下文策略、`tools/` 编程工具，以及可选的 `subagent/`、`team/`、`task/`、`proxy/` 和 `permission/` 能力。

## 设计与 API

- [`docs/kernel.md`](docs/kernel.md) —— Message-native 边界与轨迹驱动的 Loop 优化。
- [Go Package 文档](https://pkg.go.dev/github.com/compforge/agentgo) —— 完整公开 API。
- [`examples/`](examples/) —— 可运行的单 Agent 与多 Agent 示例。

## 稳定性

`Agent`、`AgentLoop`、`Event`、`Tool`、`AgentMessage` 与 `Message` 是优先稳定的公开面；示例和内部实现细节可能更快演进。

## 许可证

Apache License 2.0
