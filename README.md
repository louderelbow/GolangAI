# DeepTalk

一个基于 Go + Vue 3 的多模型推理服务。项目采用 MVC 业务分层与 `internal/` 能力分层，统一支持纯对话、确定性 RAG 和 Unified Agent 三条执行路径。

## 功能特性

| 模块 | 功能 |
|------|------|
| 🤖 多模型对话 | 统一调度 **DeepSeek / RAG / Agent** 三类模型 |
| 📚 RAG 知识库 | 智能 Markdown 分片 → 火山方舟 Embedding → Redis Stack 向量索引 → LLM 关键词增强检索 → Prompt 生成，全链路降级 |
| 🧠 Agent | 融合原 MCP 与 ReAct 路径，通过原生 function calling 使用外部工具 |
| 🔌 MCP 协议 | 集成 mcp-go StreamableHTTP/stdio 传输，支持工具白名单与多服务端 |
| 🛡️ 限流降级 | Redis Token Bucket（Lua 原子性）+ 本地 sync.Map 降级，超限返回 429 |
| 💾 记忆压缩 | Token 动态估算，超 4000 token 触发 LLM 摘要压缩，保留最近 3 轮原始对话 |
| 📝 消息持久化 | RabbitMQ 异步落库（队列持久化 + 手动 Ack），消息不丢 |
| 🔐 用户系统 | 注册/登录，bcrypt 密码哈希，JWT 鉴权中间件 |
| 🌊 SSE 流式 | 服务端 text/event-stream，前端 fetch ReadableStream 实时渲染 |

## 技术栈

| 类别 | 技术 |
|------|------|
| 后端框架 | Go + Gin |
| 前端框架 | Vue 3 + Element Plus + Axios |
| 数据库 | MySQL 8.x (GORM) |
| 缓存/向量 | Redis Stack (go-redis v9) |
| 消息队列 | RabbitMQ (AMQP, streadway/amqp) |
| AI 框架 | CloudWeGo Eino (ChatModel / Embedding / Agent / Retriever) |
| RAG | 自建分片 + Eino Embedding + Redis FT.SEARCH |
| 认证 | JWT (golang-jwt/jwt v4) |
| 限流 | Redis Lua Token Bucket + sync.Map 降级 |
| MCP | mcp-go (StreamableHTTP) |

## 项目结构

```
DeepTalk/
├── cmd/
│   ├── server/main.go             # 唯一依赖组装与服务启动入口
│   └── eval/main.go               # 离线评测 CLI
├── config/
│   ├── config.toml.example        # 配置模板（真实 config.toml 由 .gitignore 排除）
│   └── config.toml.docker         # Docker 部署用配置
├── router/                        # 路由注册与中间件挂载
├── controller/                    # HTTP 参数、JSON/SSE 编解码
│   └── session/                   # dto/query/command/stream 分文件
├── service/                       # 业务流程编排
│   ├── chat/                      # 会话运行态、生成、计量、回收
│   └── session/                   # 创建、查询、消息、流式业务
├── dao/                           # 数据访问层
├── model/                         # GORM 数据模型
├── internal/
│   ├── llm/                       # 模型契约、DeepSeek、RAG、Unified、工厂
│   ├── rag/                       # 分片、索引、混合检索、Prompt
│   ├── decision/                  # 意图规则、LLM 兜底、判定缓存
│   ├── agent/                     # core/memory/tool/skill/guard
│   ├── inference/                 # 推理调度边界
│   ├── cache/                     # 多级缓存边界与语义缓存
│   ├── ratelimit/                 # 分布式限流边界
│   └── infra/                     # config/logger/metrics/mysql/redis/MQ/MCP
├── common/code/                   # 跨层稳定错误码
├── middleware/                    # JWT、请求 ID、限流、请求大小限制
├── utils/                         # 工具函数（JWT/密码/随机数）
├── vue-frontend/                  # Vue 3 前端
└── gopherai.sql                   # 数据库初始化
```

## 快速开始

### 环境要求

- Go >= 1.21
- Node.js >= 16
- MySQL >= 8.0
- Redis Stack >= 7.x（向量索引需要）
- RabbitMQ >= 3.x（可选，不启动时自动降级）

### 1. 配置文件

```bash
cp config/config.toml.example config/config.toml
# 编辑 config.toml 填入你的 MySQL/Redis/RabbitMQ/API Key 配置
```

### API Key

| 用途 | 读取位置 | 说明 |
|------|----------|------|
| **DeepSeek**（modelType 1） | 环境变量 `DEEPSEEK_BASE_URL` / `DEEPSEEK_MODEL_NAME` / `DEEPSEEK_API_KEY`（兼容 `OPENAI_*`） | 默认 `https://api.deepseek.com` + `deepseek-chat` |
| **阿里百炼**（modelType 2/6 + Embedding） | `[ragModelConfig] apiKey` → `ALIYUN_API_KEY` → `DEEPSEEK_API_KEY` → `OPENAI_API_KEY` | 建议配置 `ragModelConfig.apiKey` |



### 2. 初始化数据库

```bash
mysql -u root -p < gopherai.sql
```

> **已有数据库升级**：会话新增了 `model_type` 字段（会话级模型绑定），需要先执行迁移：
> ```bash
> mysql -u root -p deeptalk < gopherai_migration_001.sql
> ```
> 否则会话相关接口会报 `Unknown column 'model_type'`。

### 3. 启动后端

```bash
go run ./cmd/server
# 服务运行在 http://localhost:9090
```

### 4. 启动前端

```bash
cd vue-frontend
npm install
npm run serve
# 前端运行在 http://localhost:8080
```

## API 接口

### 用户模块

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/user/register` | 用户注册 |
| POST | `/api/v1/user/login` | 用户登录 |
| POST | `/api/v1/user/captcha` | 获取邮箱验证码 |

### AI 聊天模块（需要 JWT）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/AI/chat/sessions` | 获取用户会话列表（含每个会话绑定的 `modelType`） |
| POST | `/api/v1/AI/chat/send-new-session` | 创建新会话并发送（此处 `modelType` 生效） |
| POST | `/api/v1/AI/chat/send` | 发送消息（`modelType` 被忽略，以会话绑定为准） |
| POST | `/api/v1/AI/chat/history` | 获取会话历史 |
| POST | `/api/v1/AI/chat/send-stream-new-session` | 流式创建新会话（首个事件返回 `sessionId` + `modelType`） |
| POST | `/api/v1/AI/chat/send-stream` | 流式发送（`modelType` 被忽略） |

### 其他（需要 JWT）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/file/upload` | 上传文件并构建 RAG 索引 |

### 模型类型说明

会话创建时绑定模型，**创建后不可更改**：前端在已有会话中只读展示当前模型，只有「新聊天」才允许选择。

| modelType | 模型 | 说明 |
|-----------|------|------|
| 1 | DeepSeek | OpenAI 兼容协议，默认聊天 |
| 2 | 确定性 RAG | 必然检索知识库并基于文档回答 |
| 6 | Unified Agent | 统一的 Agent + MCP 工具执行路径 |

未记录 `model_type` 的历史会话按 `2` 处理；历史 `3/5` 在执行时映射为 `6`，历史 `4` 映射为 `2`，数据库结构不变。

### 流式协议（SSE）

所有事件均为 `data: <json>`，`[DONE]` 表示结束：

```
data: {"sessionId": "xxx", "modelType": "2"}   // 新建会话时首个事件
data: {"content": "增量文本"}
data: {"error": "错误信息"}
data: [DONE]
```

### MCP 服务（modelType=6 可用）

```bash
go run ./internal/infra/mcp -http-addr :8081
# 后端默认连接 http://localhost:8081/mcp，可用 MCP_BASE_URL 覆盖
```

## 成本、可观测与缓存

### /metrics（Prometheus 文本格式，零依赖）

```
deeptalk_ai_requests_total{model,model_type,status,source}   # 请求数（status=ok/error/quota_exceeded，source=llm/semantic_cache）
deeptalk_ai_tokens_total{model,kind}                         # prompt / completion / cached(命中上游前缀缓存)
deeptalk_ai_cost_micros_total{model}                         # 费用累计（微元，1 元 = 1e6）
deeptalk_ai_request_duration_seconds{model,source}           # 延迟直方图
deeptalk_ai_cache_total{result}                              # 语义缓存 hit/miss
deeptalk_ai_active_sessions                                  # 内存中活跃会话数
```

每次请求还会打一条结构化日志：模型、用户、状态、prompt/completion/cached token、费用、延迟。

### 计费配置（`[aiPricing]`）

元 / 100 万 token。`promptPrice.<模型名>`、`completionPrice.<模型名>`，未配置走 `default*`。
命中前缀缓存的输入 token 按 0.1 系数计费。`dailyTokenQuota > 0` 时按用户每日限流（Redis 计数，Redis 不可用退回进程内）。

### Prompt 前缀缓存

消息顺序被刻意设计为**稳定前缀在前、易变内容在后**：

```
[固定 systemPrompt] → [历史摘要] → [历史消息…] → [当前时间] → [本轮提问]
```

`当前时间` 每次都变，所以必须排在后面；以前它在第 0 位，等于每次请求都改写前缀，上游缓存命中率恒为 0。
命中情况可以通过 `/metrics` 的 `kind="cached"` 直接看到。

### 语义缓存（`[semanticCache]`）

相似问题直接返回缓存答案，省一次 LLM 调用。
**只对"会话首轮提问"生效**——有历史上下文时同一问题的正确答案会不同，缓存会答错，这是刻意加的安全边界。
embedding 不可用时自动跳过（fail-open），不影响正常问答。

### 评测体系（黄金集）

```bash
go run ./cmd/eval -set eval/golden_example.json -user <你的账号> [-type 2] [-judge] [-v]
```

指标：**检索召回率**（`expectSources` 是否出现在检索结果里）、**要点覆盖率**（`expectPoints` 是否被答案覆盖）、**拒答准确率**（该拒答的有没有拒答）、**忠实度**（`-judge` 时用 LLM 打分）、平均延迟。
黄金集里的 `thresholds` 低于阈值时 CLI **退出码为 1**，可直接作为 CI / 发布前卡点。

## MCP 工具

`[mcpConfig.servers]` 声明工具来源，工具由**模型原生 function calling** 调用（不再依赖提示词里"让模型吐 JSON"）：

```toml
[[mcpConfig.servers]]
name = "weather"
url  = "http://localhost:8081/mcp"
allowedTools = ["get_weather"]     # 白名单，空数组 = 该服务端全部工具

maxStep = 5                        # Agent 最大推理步数
```

- 启动时自动 `tools/list` 拉取工具清单，按白名单过滤后包装成 eino 工具
- 多服务端时工具名自动加 `<server>__` 前缀避免重名
- 某个服务端连不上只跳过它，不影响其它工具与普通对话；一个工具都没有时退化为普通聊天
- 本项目自带的 MCP 服务：`go run ./internal/infra/mcp -http-addr :8081`



- **已登录接口**（`/AI/chat/*`）：Token Bucket，每用户容量 10 次突发、每秒补充 2 次
- **未登录接口**（`/user/register`、`/user/login`、`/user/captcha`）：按 **IP** 限流，容量 5 次、每 5 秒补 1 次（防刷验证码/暴力破解）
- 超限返回 HTTP 429 `{"status_code":4002,"status_msg":"请求过于频繁，请稍后再试"}`
- **Redis 不可用时降级本地内存限流**（仍有限流效果，而不是放行）；Redis 报错由熔断器接管（连续失败即打开，见下）

## 熔断（gobreaker）

按**依赖粒度**独立熔断，而不是全局一个开关：`llm:<模型>` / `redis:<用途>` / `mcp:<工具>` / `http:<服务>`。
三态机（closed → open → half-open → closed），跳闸规则为「连续失败 N 次」**或**「失败率超阈值且样本足够」。

只有**调用方主动取消**（`context.Canceled`）不计为下游故障，避免用户关页面把服务判死。
下游**超时算失败**——这点很关键：`context.DeadlineExceeded` 曾经也被当成"非故障"，
而 go-redis 之类的客户端在连接超时时返回的错误正好满足这个判定，结果连续失败计数永远是 0、
熔断器**永远不跳闸**，每个请求都要白等一个完整的超时（压测实测 Redis 不可达时 p50 卡在 2058ms）。
影响面不止 Redis：**任何以超时形式出现的故障都不计数**，而超时恰恰是最常见的故障形态。
回归测试见 `common/resilience/breaker_test.go` 的 `TestDeadlineExceededCountsAsFailure`。

已接入：各 LLM（按模型名）、Redis 限流与配额、MCP 工具（按工具名）与 Embedding。

配置：`[resilience]`（`failureThreshold` / `failureRatio` / `timeoutSeconds` …），`disabled = true` 可整体关闭。
指标：`deeptalk_circuit_breaker_state{name}`（0=closed 1=half-open 2=open）、`..._events_total`、`..._rejected_total`。

实测（上游全部返回 500）：前 5 次真实失败各约 1040ms，两个熔断器同时打开后，第 6 次起 **约 20ms 快速失败**，返回 5004「AI 服务暂时不可用」。

## 请求限制

| 位置 | 限制 |
|------|------|
| `/user/*` 请求体 | 16KB |
| `/AI/*` 请求体 | 64KB |
| 问题长度 | ≤ 4000 字（`question`） |
| TTS 文本 | ≤ 1000 字（超过百度 60 字/次上限时自动分片合成后拼接） |
| 文档上传 | ≤ 10MB（仅 `.md` / `.txt`） |
| 验证码 | 同一邮箱 60 秒冷却，Redis 中 2 分钟有效 |
| 会话列表 | 最多返回 200 条，按创建时间倒序 |

### 限流什么程度由配置决定

默认值与原硬编码一致：单用户**桶容量 10 / 每秒补 2 个（≈2 QPS）**，未登录接口按 IP **容量 5 / 每 5 秒补 1 个**。

```toml
[rateLimit]
enabled = true      # 压测时临时设为 false
capacity = 10
refill = 2          # 单用户平均 QPS 上限
ipCapacity = 5
ipRefill = 0.2
```

数字写整数或小数都行（`10` 和 `10.0` 等价）。

## 压测

配套一套零依赖的压测工具，重点是把**被测系统**和**上游模型**解耦：不打 mock 的话，
测出来的是上游的限流和排队，不是自己的吞吐。

```powershell
go run ./stress/mockllm -addr :8099 -mode ok -delay 200ms   # 假模型（OpenAI 兼容 + 故障注入）
go run ./stress/prep    -users 50 -modelType 2 -verify      # 造账号/JWT/会话/知识库索引
k6 run -e SCENARIO=multi -e VUS=20 -e DURATION=30s stress/k6/chat.js
```

- `stress/mockllm`：OpenAI 兼容的假模型，同时提供 chat 与 embeddings；运行时可通过 `/__admin/mode?mode=500|slow|flaky|ok` 切换故障，不用重启
- `stress/prep`：直接往库里写测试账号并用项目同一套密钥签发 JWT（绕开登录接口的 IP 限流），每个账号建好会话和知识库索引
- `stress/k6/chat.js`：7 个场景 —— `local`（纯本地接口）/ `multi`（多会话并发吞吐）/ `same`（同会话并发，测会话锁串行化）/ `repeat`（重复问题，测三层缓存）/ `long`（长历史，测记忆压缩）/ `stream`（SSE，看 TTFB）/ `mixed`
- 诊断端点：`[debug] pprofEnabled = true` 后在 **127.0.0.1:6060** 暴露 `/debug/pprof`，并开启 block/mutex 采样（找锁竞争的关键）

完整操作步骤、结果解读、常见问题见 **[stress/README.md](stress/README.md)**。

## 运行期行为

| 行为 | 说明 |
|------|------|
| 会话历史懒加载 | 会话首次进入内存时才从数据库读自己的历史；空闲 30 分钟的会话会被自动释放（日志 `evicted N idle sessions`） |
| RabbitMQ 降级 | MQ 不可用/投递失败时**同步写库**（消息不丢）；消费者自带指数退避重连；毒消息重投一次后丢弃并告警，不再无限 requeue |
| 优雅停机 | 收到 Ctrl+C / SIGTERM 后停止接收新请求，等待在途请求（含 SSE 流式回答）最多 15 秒再退出 |
| 流式取消 | 客户端断开时取消上游模型调用，不再继续消耗 token |
| 日志脱敏 | 不再打印完整 JWT 与完整模型输出，只记录长度/摘要 |

## Docker 部署（Docker + Nginx）

一条命令起全部服务（MySQL + Redis Stack + RabbitMQ + 后端 + 前端 Nginx）：

```powershell
copy .env.example .env        # 然后填上 ALIYUN_API_KEY
docker compose up -d --build
# 浏览器打开 http://localhost:8080
```

结构：

```
浏览器 → frontend 容器(Nginx) → backend 容器 → mysql / redis / rabbitmq
              │
              └─ /api/... 反向代理到 backend:9090，并补上 /v1
                 （和 vue.config.js 里 devServer 的 pathRewrite 一致）
```

**只有 frontend 对外暴露端口**，后端和中间件都只在容器内部网络上，宿主机端口绑 `127.0.0.1`。

涉及的几个文件：

| 文件 | 作用 |
|---|---|
| `Dockerfile` | 后端多阶段构建（golang:alpine 编译 → alpine 运行） |
| `Dockerfile.frontend` | 前端多阶段构建（node 构建 → nginx 托管静态文件） |
| `deploy/nginx.conf` | Nginx 配置：SPA 回退、API 反向代理、**SSE 关缓冲**、上传体积 |
| `docker-compose.yml` | 五个服务的编排、健康检查、数据卷 |
| `config/config.toml.docker` | 容器内配置（地址改成服务名、密钥走环境变量） |
| `.env.example` | 密钥与端口映射模板（复制成 `.env`，已被 gitignore） |

**三个最容易踩的坑**（详见 [deploy/README.md](deploy/README.md)）：

1. **流式对话卡住** —— Nginx 必须 `proxy_buffering off`，否则 SSE 字节被攒着不发，用户要等整段回答生成完才看到内容
2. **上传返回 413** —— Nginx 默认只允许 1MB 请求体，要显式设 `client_max_body_size 10m`
3. **Redis 必须是 Redis Stack** —— RAG 向量检索依赖 RediSearch（`FT.CREATE`/`FT.SEARCH`），普通 `redis` 镜像没有这个模块
