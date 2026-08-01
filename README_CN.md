# AgentGo

**AgentGo** 是一个极简、可组合的 Go Agent 核心库，用于构建任意 AI Agent 应用。

AgentGo 基于 AgentCore 改造并独立演进。

[English](README.md) | [中文](README_CN.md)

## 安装

```bash
go get github.com/compforge/agentgo
```

## 设计哲学

克制的内核，开放的扩展，往往比面面俱到的一体化更可靠。越少的内置，越多的可能。

## 稳定性

- 优先稳定 `Agent`、`AgentLoop`、`Event`、`Tool`、`Message` 这些核心接口
- `examples/` 与内部实现细节不视为稳定 API

## 架构

```
agentgo/            Agent 核心（类型、循环、Agent、事件）
agentgo/llm/        LLM 适配层（OpenAI, Anthropic, Gemini，基于 litellm）
agentgo/tools/      内置工具：read, write, edit, bash
agentgo/context/    上下文运行时 —— 投影、重写、溢出恢复
agentgo/task/       后台任务注册中心（Runtime / Entry），bash + subagent 共用
agentgo/subagent/   SubAgent 工具 —— 通过工具调用实现多 Agent
agentgo/proxy/      ChatModel 适配器，把 LLM 调用转发到远程代理
agentgo/permission/ 可选权限引擎，自行适配为 ToolGate
```

核心设计：

- **无状态循环 + 有状态 Agent** —— `loop.go` 是 free function，所有输入通过参数注入；`agent.go` 作为循环事件的唯一消费者，更新内部状态后分发给外部监听者。双层循环：内层处理工具调用 + steering，外层处理 follow-up
- **事件流** —— 单一 `<-chan Event` 输出，驱动任何 UI（TUI、Web、Slack、日志）
- **上下文层** —— `ContextManager`（接口）+ `agentgo/context`（默认引擎）共同负责 prompt 投影、溢出恢复，并自动接入消息转换与 token 估算
- **SubAgent 工具**（`subagent/`）—— 通过工具调用实现多 Agent，四种模式：single、parallel、chain、background

## 快速开始

### 单 Agent

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
    model, err := llm.NewModel("openai", "gpt-5-mini", llm.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
    if err != nil {
        panic(err)
    }

    // 共享的 FileReadState，让 Write/Edit 能够强制 read-before-write。
    fileState := tools.NewFileReadState()
    agent := agentgo.NewAgent(
        agentgo.WithModel(model),
        agentgo.WithSystemPrompt("你是一个编程助手。"),
        agentgo.WithTools(
            tools.NewRead(".", fileState),
            tools.NewWrite(".", fileState),
            tools.NewEdit(".", fileState),
            tools.NewBash("."),
        ),
    )

    agent.Subscribe(func(ev agentgo.Event) {
        if ev.Type == agentgo.EventMessageEnd {
            if msg, ok := ev.Message.(agentgo.Message); ok && msg.Role == agentgo.RoleAssistant {
                fmt.Println(msg.Content)
            }
        }
    })

    agent.Prompt("列出当前目录下的文件。")
    agent.WaitForIdle()
}
```

如果需要工具调用拦截，注册一个 `ToolGate` ——参数校验后、工具执行前调用一次的钩子。核心本身不做任何权限判定，策略由用户提供。

```go
gate := func(ctx context.Context, req agentgo.GateRequest) (*agentgo.GateDecision, error) {
    if req.Call.Name == "bash" {
        return &agentgo.GateDecision{Allowed: false, Reason: "禁止执行 bash"}, nil
    }
    return &agentgo.GateDecision{Allowed: true}, nil
}

agent := agentgo.NewAgent(
    // ... model, tools 等
    agentgo.WithToolGate(gate),
)
```

可选的 `agentgo/permission` 子包提供更完整的决策引擎（模式、规则、文件系统根、审计）。几行 wrapper 即可适配为 `ToolGate`。

### Provider 级配置

`llm.WithExtra` 会并入每次请求的 body；需要设置 HTTP 头、`User-Agent` 或 provider 客户端选项时，使用 `llm.WithProviderExtra`：

```go
model, err := llm.NewModel("anthropic", "claude-sonnet-4",
    llm.WithAPIKey(apiKey),
    llm.WithBaseURL(baseURL),
    llm.WithProviderExtra(map[string]any{
        "user_agent": "my-client/1.0",
        "anthropic_beta": "beta-name",
        "headers": map[string]string{
            "X-Custom-Client": "my-client",
        },
    }),
)
```

### 多 Agent（SubAgent 工具）

子 Agent 作为普通工具被调用，各自拥有隔离的上下文。需要 import `agentgo/subagent` 子包：

```go
import (
    "github.com/compforge/agentgo"
    "github.com/compforge/agentgo/llm"
    "github.com/compforge/agentgo/subagent"
    "github.com/compforge/agentgo/tools"
)

model, _ := llm.NewModel("openai", "gpt-5-mini", llm.WithAPIKey(apiKey))

// 每个子 Agent 独立的 FileReadState — 各自有独立的 read 历史。
scoutState := tools.NewFileReadState()
workerState := tools.NewFileReadState()

scout := subagent.Config{
    Name:         "scout",
    Description:  "快速代码侦察",
    Model:        model,
    SystemPrompt: "快速探索代码库并汇报发现。简洁明了。",
    Tools:        []agentgo.Tool{tools.NewRead(".", scoutState), tools.NewBash(".")},
    MaxTurns:     5,
}

worker := subagent.Config{
    Name:         "worker",
    Description:  "通用执行者",
    Model:        model,
    SystemPrompt: "执行分配给你的任务。",
    Tools:        []agentgo.Tool{tools.NewRead(".", workerState), tools.NewWrite(".", workerState), tools.NewEdit(".", workerState), tools.NewBash(".")},
}

runner := subagent.NewRunner(scout, worker)
subagentTool := runner.AsTool()
agent := agentgo.NewAgent(
    agentgo.WithModel(model),
    agentgo.WithTools(subagentTool),
)
```

如果流程由宿主代码调度，可以绕过 JSON 工具协议直接调用：

```go
result, err := runner.Run(ctx, "worker", "实现指定修改")
```

要启用 background 模式（异步 subagent + 完成后通知主 Agent），需要再接一个共享任务运行时：

```go
import "github.com/compforge/agentgo/task"

rt := task.NewRuntime()
subagentTool.SetTaskRuntime(rt)
subagentTool.SetNotifyFn(agent.FollowUp) // 完成后把通知作为 follow-up 送回父 Agent
```

LLM 通过工具调用触发四种执行模式：

```jsonc
// Single：单个 agent 执行单个任务
{"agent": "scout", "task": "找到所有 API 端点"}

// Parallel：多个 agent 并发执行
{"tasks": [{"agent": "scout", "task": "查找认证代码"}, {"agent": "scout", "task": "查找数据库 schema"}]}

// Chain：顺序执行，{previous} 传递上一步输出
{"chain": [{"agent": "scout", "task": "查找认证代码"}, {"agent": "worker", "task": "基于以下内容重构: {previous}"}]}

// Background：后台异步执行，立即返回，完成后通知
{"agent": "worker", "task": "运行完整测试套件", "background": true, "description": "正在执行测试"}
```

### Steering 与注入

`Inject(ctx, msg)` 根据 Agent 当前状态自动派发消息——当调用方意图是「尽快送达」、不想自己判断 running / idle 时使用：

```go
result, _ := agent.Inject(ctx, agentgo.UserMsg("结束前先重新检查未完成任务。"))
fmt.Println(result.Disposition)
```

三种结果：

- `steered_current_run` —— 当前正在运行，消息进入本轮 steering 路径
- `resumed_idle_run` —— 当前空闲且会话尾部是 assistant，消息入队后立即触发 `Continue()`
- `queued` —— 消息已入队，但没有立即启动新 run

需要更精确控制时直接使用低层 API：

```go
agent.Steer(agentgo.UserMsg("停下来，改为专注于测试。")) // 中断当前工具序列
agent.FollowUp(agentgo.UserMsg("现在运行测试。"))       // 排到当前 run 结束之后
agent.Abort()                                            // 立即取消
```

如果消息必须并入「下一次显式用户输入」（而不是 Agent 队列），应继续放在应用层处理。

### 事件流

所有生命周期事件通过单一通道输出 —— 订阅即可驱动任何 UI：

```go
agent.Subscribe(func(ev agentgo.Event) {
    switch ev.Type {
    case agentgo.EventMessageStart:    // assistant 开始流式输出
    case agentgo.EventMessageUpdate:   // 流式 token 增量
    case agentgo.EventMessageEnd:      // 消息完成
    case agentgo.EventToolExecStart:   // 工具开始执行
    case agentgo.EventToolExecEnd:     // 工具执行完毕
    case agentgo.EventError:           // 发生错误
    }
})
```

### 结构化工具进度

长耗时工具现在可以发结构化进度，而不是依赖各项目自己约定 JSON：

```go
agentgo.ReportToolProgress(ctx, agentgo.ProgressPayload{
    Kind:    agentgo.ProgressSummary,
    Agent:   "worker",
    Tool:    "bash",
    Summary: "worker → bash",
})
```

订阅方应直接读取 `ev.Progress` 作为工具进度更新：

```go
agent.Subscribe(func(ev agentgo.Event) {
    if ev.Type == agentgo.EventToolExecUpdate && ev.Progress != nil {
        fmt.Printf("[%s] %s\n", ev.Progress.Kind, ev.Progress.Summary)
    }
})
```

### 可热切换模型

如果需要运行时换模型，可以用 `SwappableModel` 包一层。切换会在下一次调用生效。`subagent.Config.Model` 会在每次子 Agent 运行开始时重新解引用，所以同一个包装器对主 Agent 和子 Agent 都生效。

```go
defaultModel, _ := llm.NewModel("openai", "gpt-5-mini", llm.WithAPIKey(apiKey))
sw := agentgo.NewSwappableModel(defaultModel)

agent := agentgo.NewAgent(agentgo.WithModel(sw))

nextModel, _ := llm.NewModel("openai", "gpt-5", llm.WithAPIKey(apiKey))
sw.Swap(nextModel) // 下一轮开始使用新模型
```

### 自定义 LLM 适配器

要替换 LLM 调用为代理、Mock 或自定义实现，实现 `ChatModel` 接口并通过
`WithModel` 传入即可。`SwappableModel` 与 `agentgo/proxy` 子包都基于这一接口构建，可作参考。

### 上下文压缩

对话历史接近上下文窗口上限时自动摘要压缩。现在推荐直接使用内置 `ContextManager`：

```go
import (
    "github.com/compforge/agentgo"
    agentctx "github.com/compforge/agentgo/context"
)

engine := agentctx.NewDefaultEngine(model, 128000)

agent := agentgo.NewAgent(
    agentgo.WithModel(model),
    agentgo.WithContextManager(engine),
)
```

当 `ContextManager` 实现了相关可选能力接口时，`NewAgent` 会自动接入消息转换、token 估算和 context window，无需再手动配置。

当使用量超出 `ContextWindow - ReserveTokens`（默认 16384）时，压缩会：

1. 保留最近消息（默认 20000 tokens）
2. 通过 LLM 将旧消息摘要为结构化检查点（Goal / Progress / Key Decisions / Next Steps）
3. 跨压缩消息追踪文件操作（read/write/edit 路径）
4. 支持增量更新 —— 后续压缩基于已有摘要更新，而非重新总结

## 内置工具

| 工具 | 说明 |
|------|------|
| `read` | 读取文件内容，head 截断（2000 行 / 50KB） |
| `write` | 写入文件，自动创建目录 |
| `edit` | 精确文本替换，支持模糊匹配、BOM/行ending 归一化、unified diff 输出 |
| `bash` | 执行 shell 命令，tail 截断（2000 行 / 50KB） |

`bash` 工具要求 PATH 上有 POSIX shell（`bash` 或 `sh`）——Windows 上即 Git Bash。不提供 cmd.exe/PowerShell 回退：LLM 生成的命令假定 POSIX 语法。

## API 参考

### Agent

| 方法 | 说明 |
|------|------|
| `NewAgent(opts...)` | 创建 Agent |
| `Prompt(input)` | 发起新对话轮次 |
| `PromptMessages(msgs...)` | 用任意 AgentMessage 发起对话 |
| `Continue()` | 从当前上下文继续 |
| `Inject(ctx, msg)` | 根据当前状态自动选择 steer / idle 续跑 / 排队 |
| `Steer(msg)` | 中断注入 steering 消息 |
| `FollowUp(msg)` | 排队 follow-up 消息 |
| `Abort()` | 取消当前执行 |
| `AbortSilent()` | 静默取消（不发 abort 标记） |
| `HoldRuns()` | 静默排空当前 run 并拒绝新启动（`ErrRunsHeld`），直到 release——用于原子性的状态手术 |
| `Reset()` | 经 HoldRuns 排空后清空全部状态与队列 |
| `WaitForIdle()` | 阻塞等待完成 |
| `Subscribe(fn)` | 注册事件监听 |
| `State()` | 获取当前状态快照 |
| `ExportMessages()` | 导出消息用于序列化 |
| `ImportMessages(msgs)` | 导入反序列化的消息 |
| `BuildLLMMessages()` | 物化下一次 LLM 调用的提示（system → 投影后的历史） |

## 许可证

Apache License 2.0
