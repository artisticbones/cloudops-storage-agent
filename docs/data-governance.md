# Task9 数据治理

## 数据边界与租户键

当前 RAG API 强制要求非空 `tenant` 和 `version`：
`Retriever.Search(ctx, tenant, version, query, limit)`；`MemoryRetriever` 只返回
同时匹配两者的文档。`NewAgentWithRetriever` 也拒绝空 tenant/version，Context Builder
按该边界检索并保留 Source 标识。任何持久化实现都必须把 tenant 作为授权边界，而不
能只把它当普通过滤字段。

| 数据 | 当前实现 | 治理要求 |
| --- | --- | --- |
| Session | `Session` 有独立 ID，内存 Agent 维护消息历史；Agent 持久化边界会按注册工具的 `ToolSpec` 脱敏参数和结果 | 存储键应为 `(tenant, sessionID)`；跨租户读写必须拒绝 |
| Checkpoint | `MemoryCheckpointStore` 按 run ID 进程内保存，支持深拷贝；Agent 写入前对工具消息和 `CompletedTools` 脱敏，恢复时使用安全占位内容 | 持久化键应为 `(tenant, runID, version/schema)`；恢复前校验 tenant 和授权 |
| Trace | 事件含 trace/session/run identity；敏感工具参数摘要化，敏感工具结果和敏感参数值不写入事件 | Trace 写入和查询按 tenant ACL 隔离；禁止把原始 secret/敏感参数写入日志 |
| RAG 文档 | Document 含 `TenantID`、`Version`、Source 和可选 Metadata | 入库、索引、缓存、召回、备份均带 tenant/version；Source URI 也按租户授权 |

当前 `Session`、`Checkpoint` 和 `Trace` 结构本身没有 TenantID 字段，因此内存实现
不能被误认为已经提供持久化租户隔离。生产适配器必须在存储接口外层绑定已认证租户，
或在后续兼容扩展中显式传递 tenant；禁止从用户输入中自行推断或切换租户。当前实现的
脱敏边界依赖 Agent 注册表中的 ToolSpec；直接绕过 Agent 调用通用 CheckpointStore 时
没有工具元数据，不能宣称自动识别任意业务 secret。

## 脱敏与最小化

- API key、Authorization header、cookie、密码、个人身份信息和支付数据不得进入
  Session、Checkpoint、Trace、错误消息或评估样例；
- Trace 只保留排障所需的工具名、调用 ID、状态、耗时和安全摘要；RAG 文档 Metadata
  在注入上下文前应采用 allowlist；当前 formatter 已不输出 Metadata；
- Agent 写入 Session/Checkpoint 前：`Sensitive` 工具参数和结果使用安全占位符，
  `SensitiveParameters` 的参数值和结果中同名字段使用 `[REDACTED]`；恢复后的上下文
  继续使用占位符。非敏感工具结果不做这层替换；Trace 仍使用既有摘要/替换逻辑。
- 评估使用 Fake 数据和非生产 source URI；导出前再次扫描 secret、邮箱、手机号和
  token 模式；
- 加密传输和存储由部署平台提供，密钥与业务数据分离并轮换。

## 保留、删除、备份与恢复

建议按数据类型配置可审计的 TTL，而不是永久保留：Session/Checkpoint 采用短期恢复
窗口，Trace 采用更短的排障窗口，RAG 文档按知识库版本和合同/法务要求保留。删除请求
必须同时覆盖主库、索引、缓存、导出物和备份中的可删除副本，并记录删除审计事件；
备份过期后再确认删除完成。

备份应加密、按 tenant 访问控制、定期验证恢复；恢复时先恢复到隔离环境，校验租户键、
文档 version、Checkpoint schema 和完整性，再允许流量进入。当前 MemoryCheckpointStore
是进程本地存储，没有持久化、备份或恢复能力；这些生产策略尚未在仓库中实现。当前
恢复仅验证进程内 run ID 和结构化深拷贝，不能替代生产级 tenant ACL、schema/version
校验、加密备份或删除审计。

## 发布前检查

发布前确认：检索请求 tenant/version 来自认证上下文；查询和缓存键包含二者；Trace
和 Checkpoint 的读写有租户 ACL；删除/备份有审计证据；测试与日志不含真实 Secret；
跨租户检索、恢复和导出测试均失败闭环。任何未满足项应标为发布阻塞项，而不是默认放行。
