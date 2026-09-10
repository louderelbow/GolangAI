# DeepTalk

一个基于 Go + Vue 3 构建的 AI 对话平台，采用工厂+策略模式统一调度 5 类大模型，集成 RAG 知识库检索、ReAct Agent 自主工具调用、MCP 协议、限流降级等功能。

## 功能特性

| 模块 | 功能 |
|------|------|
| 🤖 多模型对话 | 工厂+策略模式统一调度 **DeepSeek / RAG / MCP / Ollama / ReAct Agent** 五类模型，新增模型一行注册 |
| 📚 RAG 知识库 | 智能 Markdown 分片 → 火山方舟 Embedding → Redis Stack 向量索引 → LLM 关键词增强检索 → Prompt 生成，全链路降级 |
| 🧠 ReAct Agent | Eino 原生 Agent 循环，4 个 InferTool（calculator/datetime/word_count/get_weather），MaxStep=5 自主推理 |
| 🔌 MCP 协议 | 集成 mcp-go 协议，StreamableHTTP 传输，支持跨模型工具调用 |
| 🛡️ 限流降级 | Redis Token Bucket（Lua 原子性）+ 本地 sync.Map 降级，超限返回 429 |
| 💾 记忆压缩 | Token 动态估算，超 4000 token 触发 LLM 摘要压缩，保留最近 3 轮原始对话 |
| 🎤 语音合成 | 百度 TTS API，MD5 文本缓存，单次请求直接返回音频流 |
| 🖼️ 图片识别 | 阿里云 DashScope 多模态 API（qwen-vl-plus），中文描述图片内容 |
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
| 图片识别 | 阿里云 DashScope qwen-vl-plus |
| 语音合成 | 百度语音 API |
| 认证 | JWT (golang-jwt/jwt v4) |
| 限流 | Redis Lua Token Bucket + sync.Map 降级 |
| MCP | mcp-go (StreamableHTTP) |

## 项目结构

```
DeepTalk/
├── main.go                        # 入口：初始化 DB/Redis/MQ → 加载历史 → 启动 HTTP
├── config/
│   ├── config.go                  # TOML 单例配置（启动时一次性加载，零 IO）
│   ├── config.toml.example        # 配置模板（真实 config.toml 由 .gitignore 排除）
│   └── config.toml.docker         # Docker 部署用配置
├── router/
│   ├── router.go                  # 路由入口 + JWT/RequestID/RateLimit 中间件
│   ├── AI.go                      # 聊天路由（8 个）
│   ├── File.go                    # 文件上传路由
│   ├── Image.go                   # 图片识别路由
│   └── user.go                    # 用户路由（3 个）
├── controller/                    # 控制器层（参数绑定 + 响应）
│   ├── session/session.go
│   ├── file/file.go
│   ├── image/image.go
│   ├── tts/tts.go
│   ├── user/user.go
│   └── common.go
├── service/                       # 业务逻辑层
│   ├── session/session.go
│   ├── file/file.go
│   ├── image/image.go
│   └── user/user.go
├── dao/                           # 数据访问层
│   ├── message/message.go
│   ├── session/session.go
│   └── user/user.go
├── model/                         # GORM 数据模型
├── common/                        # 通用组件
│   ├── aihelper/                  # AI 核心
│   │   ├── factory.go             #   工厂注册 5 模型
│   │   ├── manager.go             #   嵌套 map + RWMutex 管理
│   │   ├── aihelper.go            #   消息历史 + MQ 异步落库
│   │   ├── model.go               #   5 个模型实现（OpenAI/RAG/MCP/Ollama/ReAct）
│   │   ├── compressor.go          #   记忆压缩器
│   │   └── tools.go               #   ReAct Agent 4 个 InferTool
│   ├── rag/                       # RAG 检索（分片/Embedding/索引/检索）
│   ├── tts/                       # 百度 TTS
│   ├── image/                     # 阿里云 DashScope 多模态识别
│   ├── mysql/                     # MySQL 连接池
│   ├── redis/                     # Redis 连接
│   ├── rabbitmq/                  # RabbitMQ（Work Queue + 手动 Ack）
│   ├── email/                     # 邮件验证码
│   ├── logger/                    # slog 封装（requestId 链路追踪）
│   └── code/                      # 统一错误码
├── middleware/
│   ├── jwt/jwt.go                 # JWT 认证中间件
│   ├── requestid.go               # RequestID 链路追踪中间件
│   └── ratelimit.go              # Token Bucket 限流中间件
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

### API Key 放在哪（两条线，别搞混）

| 用途 | 读取位置 | 说明 |
|------|----------|------|
| **DeepSeek**（modelType 1 / 5 的对话模型） | `[deepSeekConfig]` → 环境变量 `DEEPSEEK_BASE_URL` / `DEEPSEEK_MODEL_NAME` / `DEEPSEEK_API_KEY`（也兼容 `OPENAI_*`）→ 默认 `https://api.deepseek.com` + `deepseek-chat` | 配置留空即用环境变量；两者都支持 |
| **阿里百炼**（modelType 2/3 的对话模型 + Embedding + 图片识别） | `[ragModelConfig] apiKey` → 环境变量 `ALIYUN_API_KEY` → `DEEPSEEK_API_KEY` → `OPENAI_API_KEY` | 建议只填 `ragModelConfig.apiKey` |

> 结论：**DeepSeek 的 key 不在 config.toml 里也能跑**（默认读环境变量），
> 想集中管理就往 `[deepSeekConfig] apiKey` 填；填了就以配置文件为准。

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
go run main.go
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
| POST | `/api/v1/AI/chat/tts/play` | 语音合成（文本 MD5 缓存，返回 audio/mp3 流） |

### 其他（需要 JWT）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/image/recognize` | 图片识别（表单上传） |
| POST | `/api/v1/file/upload` | 上传文件并构建 RAG 索引 |

### 模型类型说明

会话创建时绑定模型，**创建后不可更改**：前端在已有会话中只读展示当前模型，只有「新聊天」才允许选择。

| modelType | 模型 | 说明 |
|-----------|------|------|
| 1 | DeepSeek | OpenAI 兼容协议，默认聊天 |
| 2 | 阿里百炼 RAG | 知识库检索增强生成 |
| 3 | 阿里百炼 MCP | MCP 协议工具调用（需先启动 MCP 服务，见下） |
| 4 | Ollama | 本地离线模型，地址/模型名可用 `OLLAMA_BASE_URL` / `OLLAMA_MODEL_NAME` 覆盖 |
| 5 | ReAct Agent | Eino 原生 Agent + 4 工具 |

未记录 `model_type` 的历史会话按默认模型 `2`（RAG）处理。

### 流式协议（SSE）

所有事件均为 `data: <json>`，`[DONE]` 表示结束：

```
data: {"sessionId": "xxx", "modelType": "2"}   // 新建会话时首个事件
data: {"content": "增量文本"}
data: {"error": "错误信息"}
data: [DONE]
```

### MCP 服务（modelType=3 需要）

```bash
go run ./common/mcp -http-addr :8081
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

### Prompt 前缀缓存（省钱的关键）

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

## MCP 工具（给 AI 加能力，不用自己写）

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
- 本项目自带的天气 MCP 服务：`go run ./common/mcp -http-addr :8081`



- **已登录接口**（`/AI/chat/*`）：Token Bucket，每用户容量 10 次突发、每秒补充 2 次
- **未登录接口**（`/user/register`、`/user/login`、`/user/captcha`）：按 **IP** 限流，容量 5 次、每 5 秒补 1 次（防刷验证码/暴力破解）
- 超限返回 HTTP 429 `{"status_code":4002,"status_msg":"请求过于频繁，请稍后再试"}`
- **Redis 不可用时降级本地内存限流**（仍有限流效果，而不是放行）；Redis 报错后会熔断 5 秒，避免每个请求都去等连接超时

## 请求限制（生产注意）

| 位置 | 限制 |
|------|------|
| `/user/*` 请求体 | 16KB |
| `/AI/*` 请求体 | 64KB |
| 问题长度 | ≤ 4000 字（`question`） |
| TTS 文本 | ≤ 1000 字（超过百度 60 字/次上限时自动分片合成后拼接） |
| 图片识别 | ≤ 8MB（超出返回参数错误） |
| 文档上传 | ≤ 10MB（仅 `.md` / `.txt`） |
| 验证码 | 同一邮箱 60 秒冷却，Redis 中 2 分钟有效 |
| 会话列表 | 最多返回 200 条，按创建时间倒序 |

## 运行期行为（排障时看这几条）

| 行为 | 说明 |
|------|------|
| 会话历史懒加载 | 会话首次进入内存时才从数据库读自己的历史；空闲 30 分钟的会话会被自动释放（日志 `evicted N idle sessions`） |
| RabbitMQ 降级 | MQ 不可用/投递失败时**同步写库**（消息不丢）；消费者自带指数退避重连；毒消息重投一次后丢弃并告警，不再无限 requeue |
| 优雅停机 | 收到 Ctrl+C / SIGTERM 后停止接收新请求，等待在途请求（含 SSE 流式回答）最多 15 秒再退出 |
| 流式取消 | 客户端断开时取消上游模型调用，不再继续消耗 token |
| 日志脱敏 | 不再打印完整 JWT 与完整模型输出，只记录长度/摘要 |

## License

MIT
