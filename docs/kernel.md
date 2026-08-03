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
都有可追溯的证据与后续验证。
