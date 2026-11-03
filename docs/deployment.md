# Task9 部署

## 当前状态

当前程序是单进程 CLI：配置由环境变量加载，默认模型是本地兼容 endpoint，
`AGENT_API_KEY` 为空时不会自动生成 key。pprof 只有在显式设置
`AGENT_PPROF_ADDR` 后才监听独立地址；`startPprofServer` 会在 context 取消时关闭
监听器。服务层已有 `AgentService.Close`、队列关闭和运行取消能力。

当前没有提交 Dockerfile、CI workflow、生产健康端点或 CLI 的 OS signal handler。
以下容器、探针和 CI 内容是部署建议，不是已实现功能。

## 分环境配置

建议为 development、staging、production 分别维护不可提交的环境配置：

| 配置 | development | staging/production |
| --- | --- | --- |
| `AGENT_BASE_URL` | 本机 Ollama/测试 endpoint | 内网或受控 OpenAI-compatible endpoint |
| `AGENT_MODEL` | 低成本开发模型 | 经评估的固定模型版本 |
| `AGENT_API_KEY` | 本地 shell secret | Secret manager 注入 |
| `AGENT_MAX_STEPS`/`AGENT_MAX_RETRIES` | 较小、便于调试 | 按 SLO 和预算固定 |
| `AGENT_CONTEXT_TOKEN_BUDGET` | 低于生产预算 | 与模型上下文和成本预算一起评审 |
| `AGENT_PPROF_ADDR` | `127.0.0.1:6060` 可选 | 默认空；仅临时内网绑定 |

不要把 `.env`、API key、token 或生产 URL 中的凭据提交到仓库。生产 Secret 应通过
平台 Secret 注入为进程环境变量；日志、Trace、core dump 和进程列表不得回显它们。

## 启动、检查与停止

启动前执行配置校验（`config.LoadConfig` 会调用 `Config.Validate`），并将失败视为
不可启动。现有程序没有 `/healthz` 或 `/readyz`，因此当前只能用进程存活、启动日志
和上游模型连通性检查；不要把 pprof HTTP 200 当作业务就绪信号。

后续若增加 HTTP 探针，应保持现有构造函数兼容，并区分：

- liveness：进程和事件循环仍在运行；
- readiness：配置有效、模型 endpoint 可用、队列未关闭且仍可接收新 Run。

停止时应先停止接收新 Run，再取消排队/运行中的 context，等待在途 Run 到达终态，
最后关闭 queue、limiter、服务和 pprof，并设置有限的 shutdown deadline。当前已实现的
`AgentService.Close` 和 pprof context shutdown 可作为这条流程的组件；CLI 级 signal
编排仍待实现。

## pprof 安全

只在临时诊断时设置 `AGENT_PPROF_ADDR=127.0.0.1:6060` 或绑定受控管理网卡；不要暴露
到公网，不要与业务端口复用。访问后及时关闭，必要时在网络层加 ACL/VPN。pprof 可能
暴露请求规模、堆内容和执行信息，采集文件按 Trace 同等级别保护。

## Docker 与最小 CI 建议（后续）

建议的最小 Docker 流程是多阶段构建、非 root 运行、只复制二进制和 prompt 文件、
使用只读根文件系统，并通过运行时 Secret 注入配置。仓库当前没有 Dockerfile，不能
声称该流程已验证。

最小 CI 应执行：`gofmt -l` 检查、`go vet ./...`、普通测试、`go test -race ./...`、
`git diff --check`，再构建并用无真实 Secret 的启动 smoke test。CI 当前未提交，以上
是建议的门禁清单。
