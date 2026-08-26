# AGENTS.md — AgentGo

## 项目定位与边界

AgentGo 是一个极简、可组合、message-native 的 Go Agent Loop 内核。它提供 Model、Tool、Context、
Event、调度与多 Agent 的通用执行能力，但不内建具体业务流程、权限结论、完成标准或轨迹评分。

应用使用 `AgentMessage` 表达领域消息，只有模型调用边界使用 `Message`。Event stream 是运行事实的
统一出口；轨迹评估可以据此演进 Prompt、Tool surface 与 Execution mechanism，但解释和优化策略归
应用或 Evaluator 所有。

## 代码地图与核心模块

```text
agentgo/
├── *.go          Agent / AgentLoop、AgentMessage / Message、Tool、Event、Context 契约及调度入口
├── codec/        通用 tagged value、类型注册与 JSON 编解码
├── context/      默认 ContextEngine、可替换 Compactor、投影、压缩、summary 与 overflow recovery
├── llm/          模型 Provider 适配；核心包不依赖具体 LLM SDK
├── proxy/        远端模型代理适配
├── tools/        内置编程工具
├── permission/   可选权限引擎；最终通过 ToolGate 注入
├── task/         后台任务运行时
├── subagent/     工具式 SubAgent
├── team/         长期多 Agent 协作能力
└── examples/     最短使用示例，不属于稳定 API
```

## 关键约定

1. **Message-native 边界不可反转**：Loop、Context、Event 和持久化使用 `AgentMessage`；
   `ToMessage` 是唯一模型协议转换边界，compaction 必须保留 `Raw`。
2. **Kernel 记录事实，不解释业务**：ContextItem / ContextDemand 共享身份协议，但 kind、representation、
   signal、提取器和评分含义归应用；权限、终止和 Context 策略均通过扩展点注入。
3. **Event stream 是统一观测面**：模型、工具、Context 投影/压缩及结束状态都从 Event 输出；新增运行
   能力时优先补完整事实事件，而不是让 UI、日志或 Harness 猜内部状态。
4. **Context 投影与提交分离**：单次模型调用的 transient projection 不应悄悄覆盖运行基线；只有显式
   commit/recovery 契约可以替换历史，并报告可观察的 compaction 事实。
5. **公开 API 保持克制**：新增接口前先确认是否能作为现有 AgentMessage、Event、Tool 或 Context
   可选能力表达；Go 改动提交前运行 `go test ./...` 与 `go build ./...`。
6. **编码机制不绑定存储或传输策略**：`codec` 提供通用 tagged value、类型注册和编解码机制；
   `AgentState` 只声明自己的可编码字段，持久化、进程交接、RPC 与调用结果对账均由宿主组合。
7. **版本随公开契约演进**：`VERSION` 表达仓库当前发布版本；公开 API、可观察行为或依赖基线变化时，
   在同一 PR 中按语义版本同步升级，避免代码能力与可识别版本脱节。
8. **Execution 是运行坐标而非外部账本模型**：逻辑执行在一次 Run 内保持稳定 `ID`，物理重试递增
   `Attempt`；Middleware 与 Event 共享该坐标，Ledger、Trace 等宿主适配负责赋予跨 Run 语义。
9. **输入接纳与 Loop continuation 分层记账**：外部请求是否已被平台持久接纳属于宿主事实；调用
   `Steer` / `FollowUp` 后，消息进入 `AgentState` 的语义队列，交给 Loop 后再进入
   `RunProgress.PendingMessages`，提交成功后才进入消息历史。所有权迁移不得丢失或重复消息。

## References

- `README.md` / `README_CN.md` —— 产品定位、能力与最短使用路径
- `docs/kernel.md` —— Message-native 内核、ContextItem / ContextDemand 与轨迹驱动优化
- `doc.go` —— Go Package 总览
- `examples/` —— 可运行示例
