# Task 5 观测说明

运行时观测分为三层：`MemoryMetrics` 保存无敏感参数的数量和延迟快照；`Trace` 输出兼容现有 JSONL 事件，并为每个事件补齐 `traceId`、`sessionId`、`runId`、`toolCallId` 和单调 `sequence`；标准库 pprof 仅在显式配置 `AGENT_PPROF_ADDR` 时启动。

pprof 入口重点使用：

- `/debug/pprof/heap` 观察长消息、Tool Result 处理后的堆分配和存活对象；
- `/debug/pprof/goroutine` 观察运行中及取消后的 Goroutine 数量，确认阻塞操作能随 context 退出；
- `/debug/pprof/allocs` 和 `/debug/pprof/profile` 辅助定位分配与 CPU 热点；
- Go 的 GC 行为可结合 heap 的 in-use/alloc 指标和 `GODEBUG=gctrace=1` 日志观察。

Task 5 不引入外部指标依赖，也不实现 Task 7 的队列；`MemoryMetrics.RecordQueue` 只为未来队列适配器提供安全的显式记录边界。
