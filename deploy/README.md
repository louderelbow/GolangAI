# DeepTalk Docker + Nginx 部署说明

## 一、整体结构

```
浏览器
  │  http://localhost:8080
  ▼
┌─────────────────────────┐
│ frontend 容器            │   Nginx：托管 Vue 静态文件
│ (nginx:alpine)          │         把 /api/... 反向代理到后端
└───────────┬─────────────┘
            │ 容器内部网络，backend:9090
            ▼
┌─────────────────────────┐
│ backend 容器             │   Go 编译出来的单个二进制
│ (alpine)                │   uploads/ 挂 volume 持久化
└──┬────────┬────────┬────┘
   │        │        │
   ▼        ▼        ▼
 mysql    redis    rabbitmq          （都挂 volume）
```

**只有 frontend 容器对外暴露端口**，后端和三个中间件都只在容器内部网络上，
宿主机端口绑在 `127.0.0.1`（只本机可访问，不对局域网开放）。

---

## 二、前置准备

1. **装 Docker Desktop**（Windows）：https://www.docker.com/products/docker-desktop/
   装完确认能跑：
   ```powershell
   docker version
   docker compose version
   ```
   > Windows 上 Docker Desktop 依赖 WSL2，第一次启动会提示装 WSL2 内核，按提示做就行。

2. **准备配置文件**：
   ```powershell
   copy .env.example .env
   ```
   然后编辑 `.env`，把 `ALIYUN_API_KEY` 填成你自己的百炼 Key。

---

## 三、启动

```powershell
# 构建并后台启动全部服务
docker compose up -d --build
```

第一次会比较慢（要下镜像 + 编译 Go + 装 npm 依赖 + 构建前端，通常 5~15 分钟）。

看状态：
```powershell
docker compose ps
```

全部变成 `healthy` / `running` 之后，浏览器打开：

```
http://localhost:8080
```

各服务的对外端口（都在 `.env` 里可改）：

| 服务 | 地址 | 说明 |
|---|---|---|
| 前端 | http://localhost:8080 | 正常使用这个 |
| RabbitMQ 管理台 | http://localhost:15673 | root / 123456 |
| RedisInsight | http://localhost:8001 | Redis 可视化 |
| MySQL | 127.0.0.1:3307 | 用 DataGrip 连的话注意端口是 3307 |

---

## 四、常用操作

```powershell
docker compose logs -f backend        # 跟后端日志（Ctrl+C 退出，不影响容器）
docker compose logs -f frontend       # 跟 Nginx 日志
docker compose restart backend        # 改完配置重启后端
docker compose up -d --build backend  # 改完 Go 代码重建后端
docker compose up -d --build frontend # 改完前端代码重建前端
docker compose down                   # 停止（数据保留）
docker compose down -v                # 停止并**删除数据**，慎用
```

**改配置不用重建镜像**：`config/config.toml.docker` 是挂载进容器的，
改完执行 `docker compose restart backend` 就生效。

---

## 五、几个必须知道的坑

### 1. 流式对话没反应（最常踩）

现象：点发送后一直转圈，等模型把整段回答写完才一次性蹦出来。

原因：**中间任何一层做了响应缓冲**，SSE 的字节被攒着不发。

- Nginx 侧：`deploy/nginx.conf` 里已经配了 `proxy_buffering off` 和
  `proxy_set_header X-Accel-Buffering no`。**这两行删了流式就废了。**
- 如果你在前面又加了一层（Cloudflare、公司网关、Nginx 套 Nginx），
  那层也要关缓冲，不然一样会卡。

### 2. 上传文档返回 413

Nginx 默认只允许 1MB 请求体，而文档上传接口允许 10MB。
`deploy/nginx.conf` 里已经设了 `client_max_body_size 10m`。
如果你改了后端的 `MaxUploadBodyBytes`，这里要同步改。

### 3. Redis 必须是 Redis Stack，不能换成普通 redis

RAG 的向量检索用的是 RediSearch（`FT.CREATE` / `FT.SEARCH`）。
普通 `redis` 镜像没有这个模块，建索引会直接报 `unknown command`。
compose 里用的是 `redis/redis-stack:latest`，**不要图省事换成 `redis:7`**。

### 4. 容器里不能用 localhost

容器里的 `localhost` 指的是容器自己，不是宿主机。
所以 `config/config.toml.docker` 里所有地址都写的是 compose 服务名
（`mysql` / `redis` / `rabbitmq`）。

如果你想在容器里连宿主机的服务，要用 `host.docker.internal`。

### 5. MCP 的 stdio 工具在容器里跑不了

原来配置里的 `command = "npx.cmd"` / `uvx` 依赖宿主机的 Node/Python，
而后端镜像基于 alpine，里面没有。所以 `config.toml.docker` 里默认把
`[[mcpConfig.servers]]` 注释掉了。要用的正确做法是**把 MCP 服务也做成一个
compose 服务**，然后填它的服务名：

```toml
[[mcpConfig.servers]]
name = "weather"
url = "http://mcp-weather:8081/mcp"
allowedTools = ["get_weather"]
```

### 6. 数据库表不会自动建吗

会。`gopherai.sql` 挂到了 MySQL 的 `docker-entrypoint-initdb.d`，
**第一次启动且数据卷为空时**自动执行。之后重启不会再跑。

程序启动时还会跑一次 GORM 的 `AutoMigrate` 做兜底，所以表结构不会缺。

如果要重新初始化数据库（**会丢数据**）：
```powershell
docker compose down
docker volume rm deeptalk_mysql_data
docker compose up -d
```

### 7. Nginx 启动失败：host not found in upstream "backend"

`nginx.conf` 里的 `upstream deeptalk_backend` 是在 **Nginx 启动时**解析域名的。
如果那一刻 backend 容器的 DNS 记录还不存在，Nginx 会直接退出。

正常情况下不会遇到——compose 里 `frontend` 有 `depends_on: [backend]`，
Docker 会先创建 backend 容器（名字一注册，DNS 就有了），再启动 Nginx。

真遇到了也不用慌：`frontend` 配了 `restart: unless-stopped`，
容器退出后会自动重启，那时候 backend 已经在跑了。
如果反复重启，用 `docker compose logs frontend` 看具体报错。

### 8. 前端构建失败：ERR_OSSL_EVP_UNSUPPORTED

Vue CLI 老项目在 Node 17+ 上偶尔会报这个（webpack 的 md4 哈希问题）。
本项目用的是 Vue CLI 5（webpack 5），一般不会遇到。
真遇到了就在 `Dockerfile.frontend` 的构建阶段加一行：

```dockerfile
ENV NODE_OPTIONS=--openssl-legacy-provider
```

---

## 六、上线前必须做的事

| 项 | 位置 | 说明 |
|---|---|---|
| **轮换已泄露的密钥** | `config/config.toml.docker` | 这个文件被 git 跟踪，里面的邮箱授权码和语音服务 Key 是明文。**密钥进过 git 历史就等于已泄露，必须去服务商后台重新生成** |
| **换掉 JWT 密钥** | `config.toml.docker` 的 `jwtConfig.key` | 现在是 `GolangAIwpl`，任何人都能用它伪造 token |
| **改数据库密码** | `.env` + `config.toml.docker` | 两处要一致 |
| **关掉 pprof** | `config.toml.docker` 的 `[debug]` | 已经是 `false`，别改成 true 就上线 |
| **限制上传大小** | `deploy/nginx.conf` | 现在是 10m，按需要调 |
| **配 HTTPS** | 见下 | 现在只有 HTTP，登录密码和 token 是明文传输 |
| **给容器挂数据备份** | `mysql_data` volume | 生产要定时备份，容器删了数据就没了 |

### 加 HTTPS（用 Certbot 最简单）

```bash
# 在宿主机上（不是容器里）
docker run --rm -v $PWD/deploy/certs:/etc/letsencrypt \
  -v $PWD/deploy/www:/var/www/certbot \
  certbot/certbot certonly --webroot \
  -w /var/www/certbot -d your-domain.com
```

然后在 `deploy/nginx.conf` 里加一个 443 的 server 块，并把 80 的流量重定向到 443。
证书目录挂到 frontend 容器里：`- ./deploy/certs:/etc/letsencrypt:ro`。

---

## 七、本机调试的两种模式

**模式 A：全部在容器里**（推荐，环境一致）

```powershell
docker compose up -d --build
```

**模式 B：中间件在容器里，后端在 GoLand/终端里跑**（改代码不用重建镜像，开发快）

```powershell
# 只起中间件
docker compose up -d mysql redis rabbitmq
```

然后后端用**你自己原来的 `config/config.toml`**（里面的 host 还是 `127.0.0.1`），
但端口要对上你在 `.env` 里设的宿主机映射：

```toml
[mysqlConfig]
host = "127.0.0.1"
port = 3307        # 注意不是 3306，因为容器映射到了 3307

[redisConfig]
host = "127.0.0.1"
port = 6380        # 注意不是 6379

[rabbitmqConfig]
host = "127.0.0.1"
port = 5673        # 注意不是 5672
```

前端本地起 `npm run serve`（8080），它自带的 devServer 代理会转发到 `localhost:9090`。

> 如果你的本机 3306/6379/5672 本来就是空的（没装过这些服务），
> 也可以把 `.env` 里的 `MYSQL_HOST_PORT` 等改回标准端口，省得改配置文件。
