# Task9 评估

## 目的与边界

评估集用于阻止 Agent Loop、工具策略、RAG 引用和模型预算的回归。当前
`EvalRunner` 使用 Fake Model/Tool，不调用网络服务；它验证可重复的运行时契约，
不替代真实模型的质量评审。真实模型评估应另行记录模型、提示词、文档版本和成本。

## 当前可执行覆盖

| 维度 | 当前证据 | 通过条件 |
| --- | --- | --- |
| Tool 选择与参数 | `eval_test.go` 的 calculator case；`agent_test.go` 的表达式校验 | 选中预期工具；非法语义参数产生 `InvalidArgument` |
| 任务成功/恢复 | `eval_test.go` 的成功、tool recovery、zero recovery case | 返回精确答案；错误恢复次数受限 |
| 引用 | `TestEvalRunnerCoversCitationAndHallucinationGuardrails`、`TestEvalRunnerRejectsCitationWhenSourceIsNotInModelRequest`、`TestEvalRunnerRequiresAllCitationEvidenceConfiguration` | 引用型 `EvalCase` 必须同时提供 `Retriever`、`RetrievalTenant`、`RetrievalVersion` 和非空 `ExpectedSources`（Source ID/URI 等引用标识）；缺任一项，`EvalRunner` 立即返回 `InvalidArgument` 失败。Runner 自动检查实际模型请求的 `Retrieved context` 系统消息包含每个 `ExpectedSources`，不能只靠 `ModelResults` 硬编码引用通过。`ExpectedRequestContains` 仍可用于额外请求文本断言 |
| 幻觉 | 同一 guardrail case 的 `ExpectedAnswer` 与 `ForbiddenAnswerContains` | 无证据时返回“不确定/无法确认”类答案，且不出现禁止事实 |
| 安全拒绝 | `eval_test.go` 的 high-risk tool case；`tool_policy_test.go` | Tool handler `CallCount == 0`；Trace 有 `tool_policy` 拒绝/待审批事件 |
| 成本/预算 | `TestEvalRunnerEnforcesExecutableBudgets` 与 `model_router_test.go` | EvalRunner 可执行 MaxCalls、MaxOutputTokens、MaxCost case，并返回 `BudgetError` 与 `BudgetExceeded`；Router regression 另外验证路由级预留、fallback、计费快照和并发预算 |

`ExpectedAnswerContains` 和 `ForbiddenAnswerContains` 仍只是确定性字符串断言；引用 case
必须用 `ExpectedSources` 声明期望的 Source/引用标识。Runner 会在真实的 Retriever 注入
之后，检查这些标识是否进入模型请求的 `Retrieved context`，因此模型直接返回硬编码引用
不能通过。`ExpectedRequestContains` 只是额外断言，不是引用证据的唯一防线。上述检查不等价
于真实模型的引用正确性评审；需要语义/事实评审时，增加人工或离线 judge，并保留输入文档
的 tenant、version 和 source 标识。

## 运行方式

```sh
go test ./... -run 'EvalRunner|MemoryRetriever|ModelRouter' -count=1
go test ./... -count=1
```

每个 case 应至少固定：输入、Fake Model 序列或基于请求的 `ModelResponder`、工具集合、
预期答案/错误、预期工具调用数和 Trace 原因。涉及 RAG 时必须固定 Retriever、tenant、
version 和 `ExpectedSources`；可按需增加 `ExpectedRequestContains`。新增 case 不应使用真实
API key 或真实租户数据。

`EvalRunner` 的边界是一次确定性 Agent Loop 的可执行行为契约：检索上下文是否进入模型
请求、答案/拒绝、handler 调用数和预算错误。`ModelRouter` regression 的边界是模型端点
选择、fallback/retry、调用/输入/输出/成本/时长预算账本；Router 测试不替代 EvalRunner
的 RAG 或工具策略 case，EvalRunner 也不声称覆盖真实 provider 的质量和网络行为。

## 后续评估集（未实现）

- 真实模型的引用精确率、拒答率和幻觉率离线评测；
- 按模型/租户的 token、调用次数、成本和 p95 延迟基线；
- 预算阈值的历史回归报告和 CI 产物归档；
- 对脱敏后的 Trace 做安全回放。

这些是后续工作，不应在文档或发布记录中描述为当前已具备的能力。
