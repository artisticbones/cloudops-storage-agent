# cloudops-storage-agent

生产级别的智能云上数据治理 Agent。

cloudops-storage-agent 是一个使用 Go 实现的 Agent Runtime 学习与实践项目，目标是把自然语言请求转换为可追踪、可控、可恢复的云上数据治理操作。项目覆盖 Agent Loop、模型适配、工具调用、上下文管理、限流、可观测性和安全边界，并逐步向生产级架构演进。

> 当前仓库的 Go module 仍为 github.com/artisticbones/hello-agent。这不影响本地运行；如果发布到 GitHub，建议在确定仓库地址后再统一修改 go.mod 和内部 import path。

## 项目定位

- 使用 Agent Loop 管理模型请求、工具调用、错误恢复和停止条件。
- 使用 ModelClient 隔离不同模型 SDK，当前默认兼容 OpenAI 风格 API，可连接 Ollama。
- 使用 Tool Schema 描述工具能力，使用 Tool 负责执行、校验参数和返回结构化结果。
- 使用 Context 和 Runtime 控制超时、取消、上下文预算和多轮会话。
- 使用 Trace、Metrics、pprof 和评估数据定位错误、性能问题和资源泄漏。
- 面向云上数据治理场景扩展 RAG、项目状态查询、操作审批、审计和风险控制能力。

## 核心架构

~~~mermaid
flowchart LR
    U[用户 / CLI / HTTP] --> S[Agent Service]
    S --> R[Agent Runtime]
    R --> C[Context Builder]
    C --> M[Model Router / ModelClient]
    M --> P[Tool Policy]
    P --> T[Tool Executor]
    T --> H[Tool Handler]
    R --> G[RAG Retriever]
    G --> D[Document Store]
    R --> K[Session / Checkpoint]
    R --> O[Trace / Metrics / Eval]
~~~

一次典型请求：

1. 接收用户问题并创建本次 Agent Run。
2. Runtime 构造模型上下文，并附加当前可用的工具定义。
3. ModelClient 请求模型。
4. 模型返回普通文本时结束本轮；返回 Tool Call 时进入工具策略和工具执行阶段。
5. 工具结果以 role=tool 消息追加到上下文，再请求模型生成下一步。
6. 达到最大步骤、上下文预算、超时或不可恢复错误时，Runtime 安全停止并记录原因。

## 环境要求

- Go 1.25 或更高版本
- 本地模型运行需要安装 Ollama
- 使用远程 OpenAI-compatible 模型时，需要网络访问和有效 API Key

## 快速开始：本地 Ollama

### 1. 拉取模型

~~~bash
ollama pull qwen3:1.7b
~~~

项目默认配置已经指向本地 Ollama：

~~~text
AGENT_BASE_URL=http://localhost:11434/v1/
AGENT_MODEL=qwen3:1.7b
~~~

本地 Ollama 通常不需要 API Key，AGENT_API_KEY 可以保持为空。

### 2. 直接运行源码

~~~bash
go run .
~~~

运行后输入问题，例如：

~~~text
请输入问题（输入 exit 退出）：
> 计算 123 + 23 / 23
~~~

输入 exit 或 quit 退出程序。

### 3. 编译后运行

~~~bash
mkdir -p bin
go build -o bin/cloudops-storage-agent .
./bin/cloudops-storage-agent
~~~

也可以使用项目根目录下的输出文件：

~~~bash
go build -o cloudops-storage-agent .
./cloudops-storage-agent
~~~

### 4. 安装到 Go 的可执行文件目录

~~~bash
go install .
cloudops-storage-agent
~~~

如果命令找不到，请确认 $(go env GOPATH)/bin 已加入 PATH：

~~~bash
export PATH="$(go env GOPATH)/bin:$PATH"
~~~

发布到 GitHub 并修改 module path 后，可以使用：

~~~bash
go install github.com/<your-account>/cloudops-storage-agent@latest
cloudops-storage-agent
~~~

### 5. 交叉编译

例如在 macOS 上构建 Linux amd64 二进制：

~~~bash
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" \
  -o bin/cloudops-storage-agent-linux-amd64 .
~~~

其他常用目标：

~~~bash
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o bin/cloudops-storage-agent-darwin-arm64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/cloudops-storage-agent-windows-amd64.exe .
~~~

## 连接远程模型

项目使用 OpenAI-compatible API 形式连接模型服务。不要把真实 API Key 写入代码或提交到 Git。

~~~bash
export AGENT_API_KEY="your-api-key"
export AGENT_BASE_URL="https://api.example.com/v1/"
export AGENT_MODEL="your-model-name"
go run .
~~~

也可以运行编译后的程序：

~~~bash
AGENT_API_KEY="your-api-key" \
AGENT_BASE_URL="https://api.example.com/v1/" \
AGENT_MODEL="your-model-name" \
./bin/cloudops-storage-agent
~~~

## 配置项

配置通过环境变量加载，未设置时使用适合本地学习的默认值。

| 环境变量 | 默认值 | 作用 |
| --- | --- | --- |
| AGENT_API_KEY | 空 | 模型服务 API Key；本地 Ollama 通常为空 |
| AGENT_MODEL | qwen3:1.7b | 模型名称 |
| AGENT_BASE_URL | http://localhost:11434/v1/ | OpenAI-compatible API 地址 |
| AGENT_MAX_STEPS | 5 | 一次 Agent Loop 允许的最大步骤数 |
| AGENT_MAX_RETRIES | 2 | 模型请求失败时的最大重试次数 |
| AGENT_REQUEST_TIMEOUT | 3m | 单次 Loop 请求超时，不是整个多轮会话生命周期 |
| AGENT_CONTEXT_TOKEN_BUDGET | 4096 | 一次模型请求的上下文预算 |
| AGENT_MAX_TOOL_RECOVERIES | 2 | 工具参数错误允许模型修正的最大次数 |
| AGENT_PPROF_ADDR | 空 | pprof 地址，例如 127.0.0.1:6060 |
| AGENT_SYSTEM_PROMPT_FILE | prompts/system.md | 系统指令文件路径 |

示例：

~~~bash
AGENT_MODEL=qwen3:1.7b \
AGENT_MAX_STEPS=8 \
AGENT_CONTEXT_TOKEN_BUDGET=8192 \
go run .
~~~

## 系统指令

默认系统指令文件位于 prompts/system.md，也可以通过环境变量指定：

~~~bash
AGENT_SYSTEM_PROMPT_FILE=prompts/production.md go run .
~~~

当前 CLI 支持通过自然语言维护系统指令。系统指令属于持久化配置，修改后新的模型请求会使用最新内容；生产环境应对系统指令变更做版本管理、审计和回滚。

## pprof 性能诊断

启动 pprof：

~~~bash
AGENT_PPROF_ADDR=127.0.0.1:6060 go run .
~~~

查看 goroutine：

~~~bash
curl 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1'
~~~

采集 Heap 和 CPU profile：

~~~bash
go tool pprof 'http://127.0.0.1:6060/debug/pprof/heap'
go tool pprof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
~~~

进入 pprof 交互界面后可使用：

~~~text
top
list <function-name>
web
~~~

生产环境不要把 pprof 端口直接暴露到公网，应限制监听地址、增加访问控制，或仅在受控诊断环境临时开启。

## 测试与质量检查

提交代码前建议执行：

~~~bash
go test ./...
go test -race ./...
go vet ./...
git diff --check
~~~

只运行某个包或测试：

~~~bash
go test ./config -run TestLoadConfig -v
go test ./... -run TestAgent -v
~~~

## 目录结构

~~~text
.
├── agent/          # Agent Loop、Runtime 和依赖注入
├── adapter/        # ModelClient 及模型适配器
├── config/         # 环境变量和运行时配置
├── contextx/       # 上下文构造、预算和压缩
├── tools/          # Tool、Schema、Registry 和工具执行
├── memory/         # 会话、checkpoint 和 RAG 记忆
├── routing/        # 模型路由、fallback、重试和预算
├── ratelimit/      # 并发控制、队列和限流
├── observability/  # Trace、Metrics、pprof 和脱敏
├── prompts/        # 系统指令
├── docs/           # 部署、数据治理、评估和可观测性文档
└── main.go         # CLI 入口
~~~

部分目录和能力仍处于持续演进中，实际可用入口以源码和测试为准。

## 相关文档

- [部署与运行](docs/deployment.md)
- [可观测性](docs/observability.md)
- [评估方法](docs/evaluation.md)
- [数据治理设计](docs/data-governance.md)

## 发布到 GitHub

建议先在 GitHub 创建名为 cloudops-storage-agent 的空仓库，然后在本地执行：

~~~bash
git remote add origin git@github.com:<your-account>/cloudops-storage-agent.git
git branch -M main
git add .
git commit -m "docs: add project README"
git push -u origin main
~~~

如果使用 HTTPS：

~~~bash
git remote add origin https://github.com/<your-account>/cloudops-storage-agent.git
git branch -M main
git add .
git commit -m "docs: add project README"
git push -u origin main
~~~

推送前请检查：

- API Key、.env、模型输出原始敏感信息没有被提交。
- pprof profile、trace 和本地运行日志没有被提交。
- README 中的命令与当前源码入口一致。
- go test ./...、go test -race ./... 和 go vet ./... 均通过。

## 当前边界

项目已经具备生产化 Agent Runtime 的核心学习骨架，但生产落地仍需要结合真实云厂商和业务继续补齐，例如：

- 多租户身份、权限、审批和审计。
- 云资源真实 API 的幂等、补偿和回滚。
- 持久化数据库、消息队列和分布式锁。
- RAG 文档生命周期、权限过滤、版本隔离和引用追踪。
- 高并发压测、容量评估、灰度发布和灾备演练。

因此，README 中的“生产级”表示目标架构和工程约束，具体云上执行能力仍应在接入真实依赖后通过集成测试和演练验证。

