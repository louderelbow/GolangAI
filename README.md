# DeepTalk

一个基于 Go + Vue 3 构建的 AI 对话平台，采用工厂+策略模式统一调度 5 类大模型，集成 RAG 知识库检索、ReAct Agent 自主工具调用、MCP 协议、限流熔断等功能。

## 功能特性

| 模块 | 功能 |
|------|------|
| 🤖 多模型对话 | 工厂+策略模式统一调度 **DeepSeek / RAG / MCP / Ollama / ReAct Agent** 五类模型，新增模型一行注册 |
| 📚 RAG 知识库 | 智能 Markdown 分片 → 火山方舟 Embedding → Redis Stack 向量索引 → LLM 关键词增强检索 → Prompt 生成，全链路降级 |
| 🧠 ReAct Agent | Eino 原生 Agent 循环，4 个 InferTool（calculator/datetime/word_count/get_weather），MaxStep=5 自主推理 |
| 🔌 MCP 协议 | Server + Client 完整实现，StreamableHTTP 传输，支持跨模型工具调用 |
| 🛡️ 限流熔断 | Redis Token Bucket（Lua 原子性）+ 本地 sync.Map 降级，超限返回 429 |
| 💾 记忆压缩 | Token 动态估算，超 4000 token 触发 LLM 摘要压缩，保留最近 3 轮原始对话 |
| 🎤 语音合成 | 百度 TTS API，sync.Map 缓存音频，前端轮询播放 |
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
│   ├── mcp/                       # MCP Server + Client
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

### 2. 初始化数据库

```bash
mysql -u root -p < gopherai.sql
```

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
| GET | `/api/v1/AI/chat/sessions` | 获取用户会话列表 |
| POST | `/api/v1/AI/chat/send-new-session` | 创建新会话并发送 |
| POST | `/api/v1/AI/chat/send` | 发送消息（body 含 modelType: 1-5） |
| POST | `/api/v1/AI/chat/history` | 获取会话历史 |
| POST | `/api/v1/AI/chat/send-stream-new-session` | 流式创建新会话 |
| POST | `/api/v1/AI/chat/send-stream` | 流式发送 |
| POST | `/api/v1/AI/chat/tts` | 创建 TTS 任务 |
| GET | `/api/v1/AI/chat/tts/query` | 查询 TTS 结果 |

### 其他（需要 JWT）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/image/recognize` | 图片识别（表单上传） |
| POST | `/api/v1/file/upload` | 上传文件并构建 RAG 索引 |

### 模型类型说明

| modelType | 模型 | 说明 |
|-----------|------|------|
| 1 | DeepSeek | OpenAI 兼容协议，默认聊天 |
| 2 | 阿里百炼 RAG | 知识库检索增强生成 |
| 3 | 阿里百炼 MCP | MCP 协议工具调用 |
| 4 | Ollama | 本地离线模型 |
| 5 | ReAct Agent | Eino 原生 Agent + 4 工具 |

## 限流说明

所有 `/AI/chat/*` 接口受 Token Bucket 限流保护：
- 每用户容量 10 次突发，每秒补充 2 次
- 超限返回 HTTP 429 `{"status_code":4002,"status_msg":"请求过于频繁，请稍后再试"}`
- Redis 不可用时自动降级本地 sync.Map 限流

## License

MIT
