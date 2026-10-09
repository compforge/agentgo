# AgentGo Kernel

## 理念与边界

AgentGo 是一个 message-native 的 Agent Loop 内核。应用持有 `AgentMessage`，消息只在模型调用边界
投影为 `Message`；Loop、ContextManager、Event 与持久化都围绕应用消息工作。内核保持克制，不内建
代码评审、资料检索等业务流程，也不替应用决定什么信息重要、什么工具调用有效。

一个 Agent Loop 的效果主要由三类可演进要素共同决定：

1. **Prompt / Context**：初始信息、运行中累积的信息及其组织、压缩和复用方式；
2. **Tool surface**：工具集合、名称、描述、参数 schema 与返回协议；
3. **Execution mechanism**：终止、重试、compaction、steering、follow-up、预算等运行策略。

AgentGo 原生支持**轨迹驱动的 Loop 优化**：Event stream 记录实际执行事实，Evaluator 在内核之外解释
轨迹，再把结论反馈给 Prompt、Tool surface 或 Execution mechanism。内核负责让事实稳定、可关联，
不负责给出具体优化答案。

## AgentMessage 与 Artifact

Agent Loop 会接收文件、工具结果等材料，并在运行中产生和更新材料。消息记录对话中的发生顺序，
`Artifact` 表达具有独立身份的应用数据，既可保存中间或最终产物，也可维护其他业务数据；它与消息是并列概念。应用可以扩展具体 Artifact 类型，一个材料可以
关联多条消息，一条消息也可以涉及多个材料，材料还可以独立于消息存在。

`ArtifactManager` 提供 CRUD，保证一个 Manager 内 ID 唯一。ID 与 Kind 的含义、材料内容及消息关联
归业务所有。AgentGo 创建并持有 Manager，通过各扩展点的 `Artifacts` 字段提供能力。扩展实现使用
本次调用提供的能力，不自行创建或缓存 Manager，因此同一个 Transformer 可以复用于不同 Agent。
内存实现支持并发访问；业务对象不深拷贝，共享内容应通过替换或由应用同步更新。

同一 Agent 的连续 Run 共用材料；turn 推进、重试、消息替换和 compaction 不会隐式删除材料。
`AgentState.Artifacts` 保存材料值，随 Snapshot 编码和恢复，具体类型由应用注册到 codec；整个
Snapshot 替换同时替换消息与材料。宿主决定存储位置及持久化时机。
裸 AgentLoop 根据 InitialState 建立独立 Manager，并通过状态事件交付材料值；新建 Agent 和子 Loop
默认隔离，即使调用方复用标准 context.Context，也不会继承父级材料管理器。

初始化输入仍是 AgentMessage 列表。stateful Agent 先通过 `SnapshotLoader` 加载基线，再向
`BeforeRun` 提供恢复后的 Snapshot、新增 Input 与独立的材料准备态。Hook 只返回错误，CRUD 始终
针对这同一份恢复基线；该 Hook 每次准备 Run 执行一次，不随模型重试或 turn 推进重复执行。
准备成功后一次性提交消息、队列与材料，运行中的扩展点共用该材料集合；失败则丢弃准备态。
准备期间 `State()` / `Snapshot()` 仍只观察已接受的状态。业务材料对象保持共享，不回滚原地修改，
需要拒绝时保留内容的业务应通过替换值更新材料。
裸 AgentLoop 调用方可通过 InitialState 提供材料值，或在 BeforeTurn 等扩展点登记初始消息中的材料。

Run / Turn Hook、Model / Tool Middleware 通过具名的 `Artifacts` 字段访问所属运行时的能力。
ContextManager 的转换、压缩和恢复入口统一接收 `TransformContext`，将材料能力传至 Transformer 和 Compactor；
标准 context.Context 保留取消、超时与内部调用链传播职责，业务无需通过隐式 key 查找 Manager。

业务 Transformer 结合输入消息和 ArtifactManager 构造请求视图。登记信息使业务能够实现内容去重、
引用、摘要等策略；AgentGo 不替业务决定哪些材料应该进入 prompt。供本次转换使用的材料必须在
Transformer 执行前登记，位于转换之后的 ModelMiddleware 更新只能影响后续转换。

材料库存与模型可见内容是不同事实：删除材料不会删除历史消息，更新材料也不会改写过去的模型请求。
去重等覆盖状态应根据每次请求的实际消息重新计算，避免 compaction 删除早先的内容后只剩悬空引用。
Transformer 基于完整消息与材料状态构造确定的请求视图，并保留 Raw 和工具调用配对。
`ContextItem` 则继续描述实际投影出的信息，由业务决定是否将 Artifact 映射为该观测协议。

## 可编码状态与执行边界

一次 stateful Agent Run 包含一次 AgentLoop 调用，Loop 包含多个 turn，一个 turn 包含一次逻辑模型调用
以及零到多个工具调用。stateful `Agent` 在 Loop 外层通过 `BeforeRun` / `AfterRun` 提供 Run 生命周期边界；
Loop 内只保留 `BeforeTurn` / `AfterTurn`，具体模型和工具调用使用 middleware。裸 `AgentLoop` 不拥有
Run hook，避免同一个名字在 wrapper 与 Loop 两层表达不同语义。

`AgentState` 是 Loop 自己拥有的状态。它在完整 turn 结束后包含消息、usage、计数以及已经决定的下一步
内部 continuation；model、tool、hook、流式 partial message 和进程内 context 不属于其可编码投影。
stateful `Agent` 在 Loop 之外还持有 steering / follow-up queue：输入一旦被 Agent 接受、尚未被 Loop 消费，
就属于 `AgentSnapshot` 的一部分。因此 `AgentSnapshot` 是 `AgentState` 与这两个 queue 在同一临界区内形成
的时点聚合，而不是新的运行时 owner，也不改变 `AgentState` 的 Loop 边界。

`codec` 包提供通用的 tagged value、稳定类型身份与 JSON 编解码；`AgentState`、`AgentSnapshot` 通过字段
tag 声明自己的 portable projection，应用再注册自定义消息和材料类型。使用 context 包的压缩策略时，
将 `context.CodecOptions()` 加入 `agentgo.NewCodec`，统一注册摘要、投影和 usage 失效包装；
当前表示、嵌套原始消息与失效状态一起恢复，避免恢复后重新展开内容或使用旧的 token 校准。

`AfterTurn` 的 State 与 `AfterRun` 的 Snapshot 都是进入回调时的观察值。Hook 可以继续维护材料，
运行时在 Hook 返回后重新捕获状态：`EventTurnEnd.State` 包含该 turn 的 Hook 写入，终态 listener
收到的 State 和此时的 `Agent.Snapshot()` 包含 AfterRun 写入。保存 Hook 入口 Snapshot 只保存入口状态；
需要保存包含收尾写入的最终状态时，应在终态 listener 中读取 Snapshot。
恢复 adapter 通过 `WithSnapshotLoader` 在初始化之前加载完整 Snapshot；装载错误会拒绝本次 Run，
后续 `Continue` 可以重试。`AfterRun` 在 Loop 完全结束、最终状态已经投影后执行，并先于终态 listener。
`SetSnapshot` 只保留为低层状态操作，不是正常恢复流程必需的用户编排步骤。进入 AgentGo 之前的 durable
inbox 仍由宿主负责，Snapshot 只承诺覆盖 Agent 已经接受的输入。

编码能力本身不决定数据保存在哪里、何时传输或者如何重新执行。宿主可以把编码结果用于持久化、进程
交接或未来 RPC，并在装载状态后重新绑定执行依赖。

模型、工具与压缩等昂贵或具有外部作用的动作共享轻量 `Execution` 坐标。`ID` 在一次 Run 内标识逻辑
执行，重试保持 `ID` 不变并递增 `Attempt`；`ParentID` 表达 summary model execution 与 compaction 等
必要的父子关系。`ModelExecution`、`ToolExecution` 是带具体输入的类型化边界，Middleware 与 Event 使用
同一坐标，避免宿主从事件顺序猜测关联。`ToolCall` 仍是模型协议中的工具请求，`Execution` 不替代它。

Execution 只表达 AgentGo 内部执行事实，不承诺跨 Run 全局唯一，也不吸收 Ledger、Trace 或工作流模型。
宿主负责把它映射到持久化身份、复用已知结果或外部 provider request ID。由 AgentLoop 发起的 threshold /
overflow compaction 会自动建立坐标；直接调用 ContextManager 的宿主则拥有该调用的执行作用域。

## 扩展与生命周期事实

Hook、Middleware 和 Event 分别承担有明确边界的工作：Run hook 属于 stateful Agent 的装载与收尾，
Turn hook 负责准备输入和观察已提交的 turn，Middleware 包裹模型或工具执行，Event 输出发生过的事实。
观测适配器组合这些公开契约；内核不另设 telemetry 层，也不依赖特定时间线或 tracing SDK。

Middleware 按注册顺序由外向内执行，可修改请求、派生 context 或短路返回已知结果；每层最多调用一次
`next`。模型重试由 Loop 管理，工具可能有外部副作用，不允许 Middleware 隐式重复执行。派生 context
会传到 provider，以及工具的校验、预览、授权与实际调用；`ExecutionFromContext` 返回当前执行坐标。
工具或 ContextManager 内的模型工作通过 `ExecuteModel` 进入相同执行路径，缺省 `ParentID` 从 context
继承；直接调用 provider 不受此入口管理。嵌套调用必须在所属操作返回前完成；并发工具也可能并发进入
模型 Middleware，调用状态应保持局部。Middleware 返回错误会影响执行，观测写入失败由适配器自行
处理，不能伪装成业务执行失败。模型、ContextManager 方法和工具执行边界将 panic 转为失败结果。

| 事实 | 边界与解释 |
|------|------------|
| `turn_start` / `turn_end` | 用 `TurnIndex` 关联。准备、模型或提交失败仍结束已开始的 turn；此时 `Err` 非空、`State` 为空，不推进已完成轮数，也不调用 `AfterTurn`。完整提交的 turn 才携带可恢复的 `State`。 |
| `model_exec_start` / `model_exec_end` | 每次物理模型尝试，包含 Middleware；重试保持逻辑 ID，递增 Attempt。内部 summary 和工具中的模型调用也使用此路径。 |
| `tool_queued` → `tool_exec_start` / `tool_exec_end` | 区分调度等待与执行管线。执行管线包含 Middleware、参数校验、预览和授权；结束时的 `Disposition` 区分实际调用、前置拒绝、Middleware 短路和跳过。 |
| `tool_invoke_start` / `tool_invoke_end` | 仅覆盖 `Tool.Execute` / `ExecuteContent`，起始事件携带授权后实参。拒绝、短路和跳过不产生调用事件。 |
| `context_prepare_start` / `context_prepare_end` | 覆盖 `ContextManager.Compact`、`Transform` 或 `RecoverOverflow`，包括不压缩、失败的情况；`ContextOperation` 区分方法。结束表示方法返回，后续提交成功与否仍由提交路径负责。 |
| `retry` 与 `retry_wait_start` / `retry_wait_end` | 前者报告重试计划；后者记录实际退避等待，包括取消提前结束。overflow recovery 本身不退避，不产生等待事件。 |

`AfterTurn` 是提交边界回调，不是 finally；它失败时已提交的 turn 仍有效，Run 随后报告错误。
`agent_end` 在 Loop 收尾和最终状态生成后记录，stateful Agent 的 `AfterRun` 在其后执行；该事件不能
用于推断外层 hook 的耗时。并发工具事件按实际发生顺序输出，消息历史仍按模型请求顺序提交。

Event 的 `Timestamp` 在源头、进入 channel 前记录，消费者不应以接收时间代替执行时间。并发事件的
到达顺序未必与时间戳排序一致；耗时必须按 Run + Execution ID + Attempt 和事件种类配对。事件区间包含
其间的流式输出与背压，不等同于 provider 服务端耗时。正常运行时 channel 使用背压；取消后维持既有的
best-effort 投递，消费者需持续 drain，不能把缺失结束事件解释成成功，也不能假定所有结束事件必达。

## ContextItem 与 ContextDemand

`ContextItem` 描述模型调用前的实际 Context 中存在哪些可识别信息；`ContextDemand` 描述轨迹暴露了
对哪些信息的需求。两者共享 `ContextKey`：

```text
ContextKey(kind, identity)
    ├─ ContextItem(representation, reason, ref)
    └─ ContextDemand(signal)
```

- `kind`、`identity`、`representation`、`reason`、`signal` 都是应用定义的开放标签；
- AgentGo 不比较 representation 的高低，也不解释 signal 的业务含义；
- 一个 `AgentMessage` 可以通过 `ContextItemProvider` 暴露零到多个 Item；
- 显式需求可以通过 `ContextDemandProvider` 暴露，从原始模型或工具事件推导需求则由 Evaluator 完成；
- Context 中存在但没有后续 Demand 的 Item 不能在单条轨迹中直接判为浪费，因为它可能已经避免了一次
  工具调用；这类收益需要通过固定语料或 A/B 对照判断。

在每次模型调用前，Loop 对 ContextManager 最终投影出的 `AgentMessage` 收集 Item，并发出
`context_projected` 事件。因此轨迹记录的是模型**实际看到的 Context 表示**，而不是压缩前的原始输入。

## 轨迹反馈流程

```text
AgentMessage / Tool / Mechanism policy
                │
                ▼
        Agent Loop execution
                │
                ▼
Event trajectory：model / tool / context / compaction / completion
                │
                ▼
Evaluator：ContextItem × ContextDemand + 其它轨迹算子
                │
                ▼
改进 Prompt、Tool surface 或 Execution mechanism
                │
                └──────── 下一批轨迹验证 ────────┘
```

例如，应用可以把文件的完整源码、outline 和路径引用表达为同一个 `ContextKey` 的不同
representation，再从读取、搜索或结论引用中推导 `ContextDemand`。Evaluator 能据此发现某类信息是否
经常在运行中被重新获取，并反向调整 Initial Context；AgentGo 只提供统一身份、投影事件和完整工具轨迹。

## 关键设计

### 轨迹是事实，不是结论

Event 只记录发生了什么：投影了哪些 Context Item、调用了什么工具、是否压缩、为何结束。诸如“重复
读取”“缺少上下文”“工具 schema 不清晰”属于 Evaluator 的判断，不能固化进 Loop。

### 领域语义留在应用层

AgentGo 可以逐步增加支持轨迹分析的稳定事件和协议，但不吸收应用的 Demand 提取器、评分阈值或
优化策略。不同应用可以共享同一套 Context 协议，同时对相同轨迹作出不同解释。

### 优化必须可验证

单条轨迹适合发现需求信号和执行异常；Prompt token、工具轮次、完成率与结果质量的整体变化，需要在
固定数据集或可比较流量上验证。轨迹驱动不是“看到一次调用就预加载所有内容”，而是让每次策略调整
都有可追溯的证据与后续验证。应追求的不是极高的 cache，而是合理的 cache；高命中率可能只是长期
携带了庞大但低价值的上下文，仍需结合单位任务成本与结果质量判断。

### 上下文 token 校准

上下文占用需要计入最新 assistant 输出（包括工具参数）及其后的消息。该次响应的 input usage
只测量生成输出前的 prompt；不能直接加 output usage，因为它可能包含不会重放的 reasoning。
压缩或重写后的模型视图不能继续使用旧 prompt 的 usage 校准：ContextEngine 在接受压缩结果时
使其失效，应用在引擎外自行重写时调用 `context.InvalidateUsage`。Raw 消息仍保留原始 usage，
供计费统计、持久化和诊断使用。这套估算衡量上下文占用，不代表累计消耗预算。

## 请求视图转换与基线压缩

Loop 持续维护已接受的 messages 与 artifacts。每个逻辑模型调用先按基线触发压缩，接受压缩结果，
再接收压缩期间到达的 steering 输入，最后执行一次 Transformer 并经 ToMessage 构造模型请求。
Transformer 拥有全局视角，可以扩大、缩小、重排或更新消息；它不承担预算判断，返回值只用于本次请求。
ToMessage 保持无参、无副作用；具体文件范围、版本与检索去重策略归应用。

Compact 负责维护长期基线。Compactor 同时接收消息和材料能力，可生成摘要、将长内容转存到宿主文件、
登记材料并用引用替换历史。默认引擎根据基线估算和阈值决定是否调用压缩策略，Chain 各阶段共享同一份
材料准备态。Loop 将候选消息与完整材料集合交给 CommitContext，接受成功后统一发布；失败丢弃候选。
成功的材料 CRUD 即使没有改写消息，也需要接受；未发生材料 CRUD 且消息未改写则不提交。
材料对象仍共享，准备态只隔离 CRUD，应通过替换值更新内容。宿主文件的失败清理由宿主负责。
直接调用 ContextEngine 的宿主自行负责准备态和接受边界，并在接受后调用 Sync。

ContextEngine 的压缩回调报告候选，Committed 为 false；Loop 只在接受后发布已提交的 context_compacted。
Sync 记录已接受的基线；Transform 和候选准备只改变视图观察，异步事件消费不反向更新引擎基线。
普通模型重试使用已接受的基线重新转换请求，不重复压缩。provider overflow 单独触发一次恢复并重试，
提交型恢复更新基线，非提交型恢复只提供重试视图；同一逻辑调用保持 Execution ID，Attempt 递增。
摘要内部模型调用经过 ExecuteModel 和模型 Middleware，使用同一材料准备态，不递归进入会话准备流程。
