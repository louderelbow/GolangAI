# DeepTalk (GopherAI-v2)

一个基于 Go + Vue 3 构建的 AI 对话平台，集成了聊天、图片识别、文件 RAG 检索、语音合成等功能。

## 功能特性

- **AI 智能对话**：支持文本对话和流式输出（SSE），基于 CloudWeGo Eino 框架，兼容 Ollama / OpenAI 等模型
- **图片识别**：基于 ONNX Runtime 的图片内容识别
- **文件 RAG 检索**：上传文件后自动构建向量索引，支持知识库问答（基于 Redis + Embedding）
- **语音合成（TTS）**：集成百度语音合成 API
- **MCP 协议**：支持 Model Context Protocol，可扩展工具调用
- **用户系统**：注册 / 登录，JWT 鉴权
- **消息持久化**：MySQL 存储用户、会话、消息数据
- **实时消息**：RabbitMQ 消息队列支持

## 技术栈

| 类别 | 技术 |
|------|------|
| 后端框架 | Go 1.25 + Gin |
| 前端框架 | Vue 3 + Element Plus + Axios |
| 数据库 | MySQL 8.x (GORM) |
| 缓存 | Redis 7.x (go-redis) |
| 消息队列 | RabbitMQ (AMQP) |
| AI 框架 | CloudWeGo Eino (Ollama / OpenAI 兼容) |
| RAG | Redis Vector + Eino Embedding + Retriever |
| 图片识别 | ONNX Runtime Go |
| 语音合成 | 百度语音 API |
| 认证 | JWT (golang-jwt) |
| MCP | mcp-go |

## 项目结构

```
GopherAI-v2/
├── main.go                  # 入口文件
├── config/                  # 配置模块
│   ├── config.go
│   └── config.toml.example  # 配置模板
├── router/                  # 路由定义
│   ├── router.go            # 路由入口 + 中间件
│   ├── AI.go                # AI 聊天路由
│   ├── File.go              # 文件上传路由
│   ├── Image.go             # 图片识别路由
│   └── user.go              # 用户路由
├── controller/              # 控制器层
│   ├── session/session.go   # 会话管理
│   ├── file/file.go         # 文件上传
│   ├── image/image.go       # 图片识别
│   ├── tts/tts.go           # 语音合成
│   ├── user/user.go         # 用户注册/登录
│   └── common.go            # 通用响应
├── service/                 # 业务逻辑层
│   ├── session/session.go   # 会话 + AI 对话
│   ├── file/file.go         # 文件存储 + RAG 索引
│   ├── image/image.go       # 图片识别服务
│   └── user/user.go         # 用户服务
├── dao/                     # 数据访问层
│   ├── message/message.go
│   ├── session/session.go
│   └── user/user.go
├── model/                   # 数据模型
├── common/                  # 通用组件
│   ├── aihelper/            # AI 助手管理器（Eino Agent）
│   ├── tts/                 # TTS 服务
│   ├── rag/                 # RAG 检索
│   ├── image/               # 图片识别
│   ├── mysql/               # MySQL 初始化
│   ├── redis/               # Redis 初始化
│   ├── rabbitmq/            # RabbitMQ 初始化
│   ├── email/               # 邮件服务
│   ├── code/                # 验证码
│   └── mcp/                 # MCP 客户端/服务端
├── middleware/               # 中间件
│   └── jwt/                 # JWT 认证
├── utils/                   # 工具函数
├── vue-frontend/            # Vue 3 前端
│   └── src/
│       ├── views/           # 页面组件
│       ├── router/          # 前端路由
│       └── utils/           # Axios 封装
├── gopherai.sql             # 数据库初始化脚本
├── go.mod
└── go.sum
```

## 环境要求

- **Go** >= 1.25
- **Node.js** >= 16（前端）
- **MySQL** >= 8.0
- **Redis** >= 7.0
- **RabbitMQ** >= 3.x
- **ONNX Runtime**（图片识别需要）

## 快速开始

### 1. 克隆项目

```bash
git clone <your-repo-url>
cd GopherAI-v2
```

### 2. 配置文件

```bash
cp config/config.toml.example config/config.toml
```

编辑 `config/config.toml`，填入你的配置信息：

- MySQL 连接信息
- Redis 连接信息
- RabbitMQ 连接信息
- AI 模型配置（Ollama / OpenAI 兼容接口）
- 百度语音 API Key（TTS 需要）
- JWT 密钥
- 邮箱配置（验证码发送）

### 3. 初始化数据库

```bash
mysql -u root -p < gopherai.sql
```

### 4. 启动后端

```bash
go run main.go
```

服务默认运行在 `http://localhost:9090`。

### 5. 启动前端（开发模式）

```bash
cd vue-frontend
npm install
npm run serve
```

前端运行在 `http://localhost:8080`，已配置代理将 `/api` 请求转发到后端 `9090` 端口。

### 6. 构建前端（生产模式）

```bash
cd vue-frontend
npm run build
```

## API 接口

### 用户模块

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/user/register` | 用户注册 |
| POST | `/api/v1/user/login` | 用户登录 |
| POST | `/api/v1/user/captcha` | 获取验证码 |

### AI 聊天模块（需要 JWT）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/AI/chat/sessions` | 获取用户会话列表 |
| POST | `/api/v1/AI/chat/send-new-session` | 创建新会话并发送消息 |
| POST | `/api/v1/AI/chat/send` | 发送消息到已有会话 |
| POST | `/api/v1/AI/chat/history` | 获取会话历史消息 |
| POST | `/api/v1/AI/chat/send-stream-new-session` | 创建新会话并流式发送 |
| POST | `/api/v1/AI/chat/send-stream` | 流式发送消息 |
| POST | `/api/v1/AI/chat/tts` | 创建 TTS 任务 |
| GET | `/api/v1/AI/chat/tts/query` | 查询 TTS 任务 |

### 图片识别（需要 JWT）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/image/recognize` | 图片识别 |

### 文件上传（需要 JWT）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/file/upload` | 上传文件并构建 RAG 索引 |

## MCP 模式

项目内置 MCP（Model Context Protocol）支持，可单独启动 MCP 服务器：

```bash
cd common/mcp
go run main.go --mode server --http-addr :8081
```

## 前端页面

| 路径 | 页面 |
|------|------|
| `/login` | 登录页 |
| `/register` | 注册页 |
| `/menu` | 功能菜单 |
| `/ai-chat` | AI 对话 |
| `/image-recognition` | 图片识别 |
