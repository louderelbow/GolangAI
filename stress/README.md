# DeepTalk 压测手册

这套工具解决一个问题：**把"被测系统"和"上游模型"解耦，测出你自己的吞吐和瓶颈在哪。**

直接压真实模型测出来的是上游的限流和排队，而且烧钱、并发一高就被限流。所以这里提供一个
OpenAI 兼容的假模型（mockllm），改一个配置就能把整条链路指向它。

## 目录

```
stress/
├── mockllm/        OpenAI 兼容的假模型服务：chat（流式/非流式）+ embeddings + 故障注入
├── prep/           预生成测试数据：测试账号 + JWT + 会话 + 知识库索引
├── k6/chat.js      压测脚本（k6，7 个场景）
└── README.md       本文件
```

配套改动：

- `config/config.toml` 新增 `[rateLimit]`（限流可配、可临时关闭）和 `[debug]`（pprof 开关）
- `common/diag/pprof.go` 新增诊断端点，`main.go` 按 `[debug] pprofEnabled` 启动
- `middleware/ratelimit.go` 的容量/速率改为读配置（默认值与原来完全一致）

---

## 一、前置条件

| 依赖 | 用途 | 没有会怎样 |
|---|---|---|
| MySQL | 测试账号 / 会话 / 消息 | prep 直接失败 |
| Redis (Redis Stack) | RAG 向量索引、限流、配额 | 降级：RAG 不检索、限流走本地桶、配额走进程内计数。**功能可用但延迟会变差** |
| RabbitMQ | 消息落库 | 降级为直连写库 |
| Go 工具链 | 跑 mockllm / prep | 必需 |
| **k6** | 压测客户端 | 必需（`stress/k6/chat.js` 就是它跑的） |

装 k6：

```powershell
winget install k6 --source winget     # 装完记得重开终端；MSI 不会自动加 PATH
k6 version                            # k6.exe v2.2.0 ...
```

> 如果 `k6 version` 报"无法识别"，说明 `C:\Program Files\k6` 不在 PATH 里
> （k6 的 MSI 确实不会自己加）。两种解法：
> 临时生效 `$env:Path += ";C:\Program Files\k6"`，或永久加到用户 PATH。

---

## 二、快速开始（5 步）

全程在仓库根目录执行。

### 步骤 0：准备一份"指向 mock"的配置

**不要直接改你的 `config/config.toml`**，复制一份出来：

```powershell
New-Item -ItemType Directory -Force -Path stress\.tmp | Out-Null
$dst = Join-Path $PWD "stress\.tmp\config.stress.toml"
$enc = New-Object System.Text.UTF8Encoding $false   # UTF-8 无 BOM

$c = [System.IO.File]::ReadAllText((Join-Path $PWD "config\config.toml"), [System.Text.Encoding]::UTF8)
$c = [regex]::Replace($c, '(?m)^(\s*baseUrl\s*=\s*).*$', '${1}"http://127.0.0.1:8099/v1"')
$c = $c.TrimEnd("`r","`n") + "`r`n`r`n[rateLimit]`r`nenabled = false`r`n`r`n[debug]`r`npprofEnabled = true`r`npprofAddr = `"127.0.0.1:6060`"`r`n"
[System.IO.File]::WriteAllText($dst, $c, $enc)
```

> ⚠️ **不要用 `Get-Content -Raw` + `Set-Content` 来复制这个文件。**
> Windows PowerShell 5.1 下它们默认按系统 ANSI 编码（中文系统是 GBK）读写，
> 而 `config.toml` 是 UTF-8 无 BOM——中文会被转坏，甚至连字符串结尾的引号都会丢，
> 启动时报 **`Near line 68 ... strings cannot contain newlines`**（看着像 TOML 语法错，
> 其实是编码错）。上面用 .NET API 显式指定 UTF-8，PS 5.1 和 7+ 都安全。

要点：
- `baseUrl` 指向 mock —— 它同时管住 **RAG 对话模型、意图识别兜底模型、embedding**（三处共用这个配置）
- `rateLimit.enabled = false` —— 压测必须关掉限流，否则单用户 2 QPS 的天花板会挡住你
- `pprofEnabled = true` —— 压完要定位瓶颈

> ⚠️ 压测结束一定要用回正常的 `config/config.toml`（不设 `DEEPTALK_CONFIG` 就是它）。

### 步骤 1：起假模型

```powershell
go run ./stress/mockllm -addr :8099 -mode ok -delay 200ms -dim 1024
```

参数说明：

| 参数 | 默认 | 说明 |
|---|---|---|
| `-delay` | 200ms | 每次请求的人为延迟，用来模拟真实推理耗时。**调成差不多等于线上 P50，压出来的数字才有参考价值** |
| `-dim` | 1024 | embedding 维度，**必须等于配置里的 `ragModelConfig.dimension`** |
| `-cachedRatio` | 0.8 | 上报的前缀缓存命中比例，用来验证 `kind="cached"` 那条指标链路 |
| `-replyRunes` | 80 | 回复长度 |
| `-toolCall` | 空 | 填工具名则总是返回一次工具调用，用于打满 ReAct 的 MaxStep |

### 步骤 2：起后端（另开一个终端）

```powershell
$env:DEEPTALK_CONFIG="$PWD\stress\.tmp\config.stress.toml"
go run .
```

如果要压 **modelType 6（Unified Agent）**，把 Agent 的上游也指到 mock（`[agentModel]` 段独立于 `[ragModelConfig]`）：

```powershell
$env:DEEPTALK_CONFIG="$PWD\stress\.tmp\config.stress.toml"
# config.stress.toml 里把 [agentModel] 的 baseUrl 改成 http://127.0.0.1:8099/v1，
# 或保持留空让它回落到 [ragModelConfig]（那边已经指向 mock）
go run .
```

启动日志里应该能看到这行，确认诊断端点开了：

```
[diag] pprof listening on http://127.0.0.1:6060/debug/pprof/ (block/mutex sampling ON)
```

### 步骤 3：造测试数据

```powershell
go run ./stress/prep -users 50 -modelType 2 -out stress/users.json -verify
```

这会做四件事：往库里写 50 个账号、用项目同一套密钥签发 JWT、每个账号建好会话、
在 `uploads/<账号>/` 下放一份样例手册并建好向量索引。`-verify` 会顺手打一次真实请求验通。

常用参数：

| 参数 | 说明 |
|---|---|
| `-users 50` | 账号数。**建议 ≥ 你要用的 VU 数**，原因见下文「三条铁律」 |
| `-sessions 1` | 每个账号建几个会话 |
| `-modelType 2` | 会话绑定的模型（2 RAG / 6 Unified Agent）。会话模型创建后不可改 |
| `-skipDoc` | 不建知识库，测 RAG 的降级路径（检索失败 → 退化成裸问） |
| `-doc <路径>` | 用自己的文档代替内置样例手册 |
| `-clean` | 先删掉同名测试账号及其会话/消息/索引，再重建 |
| `-cleanOnly` | **只清理不重建**，收尾时用 |
| `-verify` | 生成后打一次真实请求验链路 |

### 步骤 4：跑压测

```powershell
k6 run -e SCENARIO=multi -e VUS=20 -e DURATION=30s stress/k6/chat.js
```

> ⚠️ **必须用 `-e` 传参，不能用 `$env:SCENARIO = 'multi'`。**
> k6 从 1.0 起不再继承系统环境变量，脚本里的 `__ENV` 只认 `-e`。
> 用 `$env:` 不会报错，但会**静默回落到默认的 multi 场景**，很容易被误导。

输出末尾会打印一段精简摘要，同时把完整结果写到 `stress/results/`：

```
================ DeepTalk 压测摘要 ================
场景            : multi
并发 VU         : 20
请求数          : 1855
RPS             : 91.8
业务成功(1000)  : 1855
被限流(4002)    : 0
端到端 avg      : 216 ms
端到端 p95      : 231 ms
端到端 p99      : 272 ms
===================================================
```

### 步骤 5：看服务端指标 + 定位瓶颈

```powershell
# 业务指标
(Invoke-WebRequest http://127.0.0.1:9090/metrics).Content -split "`n" |
  Select-String "deeptalk_ai_|circuit_breaker|intent"

# CPU / 阻塞 / 锁竞争 profile（压测正在跑的时候采才有意义）
Invoke-WebRequest "http://127.0.0.1:6060/debug/pprof/profile?seconds=15" -OutFile cpu.prof
go tool pprof -top -cum stress\deeptalk-stress.exe cpu.prof

Invoke-WebRequest "http://127.0.0.1:6060/debug/pprof/block?seconds=5" -OutFile block.prof
go tool pprof -peek='sync\.\(\*Mutex\)\.Lock' stress\deeptalk-stress.exe block.prof
```

---

## 三、三条铁律（不遵守，测出来的数字没意义）

### 1. 必须 mock 上游

不打 mock，你测的是阿里/DeepSeek 的限流和排队，不是你的系统。而且会烧钱。

### 2. 账号数和会话数必须 ≥ 并发数

`AIHelper.turn` 是**会话级互斥锁**：同一个会话同一时刻只处理一轮对话。
如果你的 30 个 VU 只对应 5 个用户、每个用户 1 个会话，那么 6 个 VU 抢同一把锁，
测出来的是**排队延迟**，不是吞吐。

> 实测对比：同样是 30 VU，5 个用户时 p50 = 214ms、RPS = 92；
> 因为服务端每次 LLM 调用只有 ~52ms，多出来的全是等锁。
> 这不是 bug，是设计（保证同一会话的消息顺序），但你必须按 VU 数准备账号和会话。

### 3. 限流必须先关掉（或者按 QPS 反推需要多少用户）

默认单用户 2 QPS（容量 10、每秒补 2）。压测时两种做法：

- **关掉**：`[rateLimit] enabled = false`（推荐，改配置重启即可）
- **不关**：账号数 ≥ 目标 QPS ÷ 2

不处理的话你会看到大量 `4002`，一行业务代码都没跑到。

---

## 四、场景清单

用 `-e SCENARIO=` 选场景。

| 场景 | 测什么 | 建议并发 |
|---|---|---|
| `local` | 会话列表 + 历史消息。**完全不碰模型**，纯测 Gin + MySQL | 50 |
| `multi` | **主力场景**。多用户多会话并发对话，真实吞吐 | 20 |
| `same` | 所有流量压到 1 个会话，量化会话锁的排队代价 | 10 |
| `repeat` | 全程同一个问题 + 每次新建会话 → 每次都命中首轮，测**意图缓存 + 语义缓存 + 上游前缀缓存**三层叠加的收益 | 10 |
| `long` | 问题填充到 ~1500 字，快速把历史顶到压缩水位线，测**记忆压缩那次同步 LLM 调用**带来的延迟尖刺 | 10 |
| `stream` | SSE 流式接口，额外输出**首字节延迟 TTFB** | 10 |
| `mixed` | 混合流量（15% 本地 / 10% 重复问题 / 75% 正常提问），最接近真实 | 20 |

k6 用法：

```powershell
k6 run -e SCENARIO=multi -e VUS=20 -e DURATION=2m stress/k6/chat.js
```

> ⚠️ **k6 不读系统环境变量**（k6 1.0 起移除了 `--include-system-env-vars`）。
> 脚本里的 `__ENV.SCENARIO` **只认 `-e` 传参**，用 PowerShell 的 `$env:SCENARIO = 'local'`
> 是**无效的**——脚本会静默回落到默认的 `multi` 场景，不报错，很容易被误导。
> 验证方式：`k6 inspect -e SCENARIO=local stress/k6/chat.js` 看 `vus`/`duration` 有没有变。

结果会自动落到 `stress/results/<场景>-<时间>.json`。

---

## 五、故障注入

mock 支持运行时切换模式，**不用重启**：

```powershell
# 全部返回 500 —— 验证熔断是否跳闸
Invoke-RestMethod "http://127.0.0.1:8099/__admin/mode?mode=500"

# 一半概率返回 500 —— 验证失败率规则（而不是只有连续失败才跳闸）
Invoke-RestMethod "http://127.0.0.1:8099/__admin/mode?mode=flaky"

# 延迟 ×5 —— 验证超时与排队行为
Invoke-RestMethod "http://127.0.0.1:8099/__admin/mode?mode=slow"

# 恢复
Invoke-RestMethod "http://127.0.0.1:8099/__admin/mode?mode=ok"

# 看假模型侧收到了多少请求
Invoke-RestMethod "http://127.0.0.1:8099/__admin/stats"
```

标准动作：先跑一轮正常压测记录基线，切成 `500` 再跑一轮，观察：

- 服务端日志出现 `[breaker] llm:<model>: closed -> open`
- `/metrics` 里 `deeptalk_circuit_breaker_state{name="llm:..."}` 从 0 变 2
- `deeptalk_circuit_breaker_rejected_total` 开始增长
- 客户端 P95 应该**大幅下降**（快速失败），而不是继续等超时

---

## 六、怎么读结果

**看 P95/P99，不看平均值。** 平均值会被大量快请求稀释，掩盖长尾。

**区分三类"失败"，不要混成一个错误率：**

| 现象 | 含义 | 正常吗 |
|---|---|---|
| `4002` / HTTP 429 | 被限流 | 压测时不关限流就是必然，说明限流在工作 |
| `4003` | 配额用尽 | 说明配额生效，把 `aiPricing.dailyTokenQuota` 调大再压 |
| `5003` / `5004` | 模型侧失败 / 熔断快速拒绝 | 注入故障时是**预期**，正常压测出现就是问题 |

**同时看客户端和服务端**：客户端延迟包含排队和网络；服务端
`deeptalk_ai_request_duration_seconds` 只包住 LLM 调用那一段。
两者差距大，说明时间花在锁等待、DB、或序列化上。

**记住几个关键指标的含义：**

| 指标 | 说明 |
|---|---|
| `deeptalk_ai_requests_total{status,source}` | `source=semantic_cache` 的比例就是语义缓存命中率 |
| `deeptalk_ai_request_duration_seconds` | LLM 调用耗时直方图，用来算 P95 |
| `deeptalk_ai_tokens_total{kind="cached"}` | **上游前缀缓存**命中量（不是你的语义缓存） |
| `deeptalk_ai_cache_total{result}` | **你自己的语义缓存**命中/未命中 |
| `deeptalk_ai_active_sessions` | 内存中活跃会话数。**压完不降 = 会话没释放** |
| `deeptalk_circuit_breaker_state{name}` | 0=closed 1=half-open 2=open |
| `deeptalk_intent_total{intent,layer}` | 看 `layer="llm"` 占比 = LLM 兜底的真实成本 |

---

## 七、这套工具实测发现的问题（都已复现并已修）

### 1. 熔断器对"超时类故障"永远不跳闸（已修）

**现象**：Redis 不可达时，每个请求都白等完整的 2 秒连接超时，p50 一直卡在 2058ms，
熔断器状态始终是 `closed`。

**根因**：`common/resilience/breaker.go` 的 `IsSuccessful` 把
`context.DeadlineExceeded` 也当成了"非故障"：

```go
// 修复前
return err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
```

而 go-redis 在连接超时后返回的错误**正好满足** `errors.Is(err, context.DeadlineExceeded)`
（探针实测输出 `errors.Is(context.DeadlineExceeded) = true`）。
于是 `ConsecutiveFailures` 永远是 0，熔断器永远不跳闸。

这个坑覆盖面比 Redis 更广：**任何以"超时"形式表现的故障都不计数** ——
Redis 连接/读超时、HTTP 调用超时、模型超时，全都不触发熔断。
而熔断最该防的就是这类故障。

**修复**：只把"调用方主动取消"算非失败。

```go
// 修复后
return err == nil || errors.Is(err, context.Canceled)
```

**效果**（同样 Redis 不可达、5 并发 15 秒）：

| | 修复前 | 修复后 |
|---|---|---|
| 请求总数 | 35 | 540 |
| RPS | 2.3 | 35.9 |
| latency p50 | 2058 ms | 1 ms |
| latency p95 | 2175 ms | 58 ms |
| 熔断器状态 | closed（从不跳闸） | open，快速拒绝 535 次 |

回归测试见 `common/resilience/breaker_test.go` 的 `TestDeadlineExceededCountsAsFailure`。

### 2. 配置里浮点字段写成整数会导致启动失败（已修）

TOML 里 `capacity = 10`（整数）无法加载进 Go 的 `float64` 字段，
BurntSushi/toml 直接报 `cannot load TOML value of type int64 into a Go float`，
而 `GetConfig()` 是 `log.Fatal` —— 服务直接起不来，报错还很难懂。
限流的容量/速率天然就是整数写法，用户必然会踩。

**修复**：新增 `config.TolerantFloat`，`10` 和 `10.0` 都能解析。

> 这个坑是被测试套件抓到的：`common/resilience/main_test.go` 用
> `config/config.toml.example` 作为测试配置，示例里写了 `capacity = 10`，
> 一跑测试就炸。所以**配置文件示例一定要有测试覆盖**。

### 3. 日志是压测时的头号 CPU 消耗（已修）

第一轮 20 秒 profile：`os.(*File).Write` 占 **13.8%**、`log.(*Logger).output` 占 **14.3%**。
一开始以为是"代码里 `log.Printf` 太多"，改成异步写之后发现**没降反升**（25.9%），
再用 `-peek` 看调用方才定位到真正的大头：

```
os.(*File).Write  1.15s / 25.9%
  └─ 93.91% ← log.(*Logger).output     ← 不是我们的异步写入器
  └─  6.09% ← logger.(*AsyncWriter).run
```

**93.9% 来自 gorm 的 SQL 日志**：`common/mysql/mysql.go` 原本只要 `gin.Mode()=="debug"`
就把 gorm 设成 `logger.Info`，于是**每条 SQL 都同步写 stdout**（一次 RAG 问答 7~8 条 SQL）。
更隐蔽的是 gorm 用的是它自己 `log.New(os.Stdout, ...)` 建的 logger，
所以对标准库 `log` 调 `SetOutput`（异步写入器）**根本管不到它**。

**修复**（三处）：
1. `common/logger/async.go`：异步批量写入器，队列满**丢弃并计数**而不是阻塞业务，退出时 flush
2. `common/mysql/mysql.go`：gorm 日志接管到异步写入器 + 默认级别降到 `Warn`（只记慢查询/错误），
   用 `DEEPTALK_SQL_LOG=info` 才看全部 SQL
3. 顺手把 `[RAG]` 每请求 4 条检索日志合并成 1 条，去掉 JWT 中间件每请求一行的 `tokenLen`

**效果**：`os.(*File).Write` **13.79% → 3.20%**，`log.(*Logger).output` **14.32% → 0.71%**，
`WriteConsole` 完全消失。

### 4. 每个请求都重建 RAG 客户端，导致磁盘 IO + 连接无法复用（已修）

profile：`rag.NewRAGQuery` 9.02%、`os.ReadDir` 4.77%、`ark.NewEmbedder` 4.24%、
`volcengine session.loadSharedConfigIniFiles` 3.98%、`Transport.dialConn` 14.19%。

`NewRAGQuery` 每个请求都重建 embedding 客户端和 retriever，而
`arkruntime.NewClientWithApiKey` 内部会**读磁盘上的 volcengine ini 配置文件**；
每次新建 client 又意味着新的连接池，HTTP keep-alive 完全失效。

**修复**：`common/rag/client.go` 新增进程内共享的 embedder + 按用户名缓存的 `RAGQuery`；
上传新文档时调用 `rag.InvalidateUser(username)` 主动失效（索引名由文件名决定，不失效会查到旧索引）。

回归测试见 `common/rag/client_test.go`（缓存命中 / 失败不缓存 / 失效重建 / 并发只构建一次）。

### 5. 默认 HTTP 连接池只有 2 条空闲连接（已修）

即使 embedder 变成共享的，`Transport.dialConn` 仍有 14%。根因是 Go 默认 Transport 的
`MaxIdleConnsPerHost = 2`：20 个并发请求打同一个上游，只留 2 条空闲连接，其余用完即关。

**修复**（两处）：
1. `main.go` 的 `tuneDefaultHTTPTransport()`：把 `http.DefaultTransport` 调到 100/200
   （go-openai 等 SDK 的 client Transport 留空，用的就是它）
2. `common/rag/client.go` 显式给 `EmbeddingConfig.HTTPClient` 注入调好连接池的 client ——
   **不传的话 arkruntime 会自建 Transport，第 1 步的调优对它无效**

**效果**：`dialConn` 14.19% → 8.19%，`dialTCP` 11.41% → 7.47%（绝对值降 60%+）。

### 6. 三轮压测总账

同一条件（20 VU × 60s，mock 延迟 200ms）：

| 指标 | 修复前 | 全部修复后 | 变化 |
|---|---|---|---|
| CPU（20s 窗口） | 3.77s / 18.85% | **2.81s / 14.05%** | **−25%** |
| 模型侧失败 | **48** / 1584 | **0** / 1560 | 消除 |
| `os.File.Write` | 0.52s / 13.79% | 0.09s / 3.20% | **−83%** |
| `log.Logger.output` | 0.54s / 14.32% | 0.02s / 0.71% | **−96%** |
| `Transport.dialConn` | 0.63s / 14.19% | 0.23s / 8.19% | **−63%** |
| RAG 客户端构建 | 0.34s+0.29s+0.16s | ≈0 | 消除 |
| 端到端 avg / p95 | 562 / 616 ms | 577 / 613 ms | 基本不变¹ |

¹ 延迟由 mock 注入的 200ms × 3 次串行上游调用主导，不是自己算出来的，所以优化 CPU 不会改变它。
真实环境里这三次调用就是网络往返的大头，**要降延迟得减少调用次数**（见下文"关键词提取"）。

### 7. 附带确认的结论

- **会话锁是真实天花板**：30 VU 配 5 个用户时，`a.turn`（`aihelper.go:158`）
  占掉了 122 秒的阻塞时间，p50 从 ~52ms 涨到 214ms。设计如此，但压测要按 VU 数准备会话。
- **锁竞争可以忽略**：block profile 里 `sync.(*Mutex).Lock` 只占 0.026%，
  且 98% 来自日志锁（已随日志异步化一起消除）。全局 metrics 注册表那把锁没有成为瓶颈。
- **cached token 链路是通的**：mock 上报 `prompt_tokens_details.cached_tokens` 后，
  `/metrics` 里 `deeptalk_ai_tokens_total{kind="cached"}` 正常增长，
  说明 Eino → `aihelper.go:313` → `metrics.go:308` → `cost.go` 折价这条链路完好。
- **RAG 一次问答实际打 3 次上游**：1 次 query embedding + 2 次 LLM
  （关键词提取 `model.go:398` 只要问题 ≥10 字就额外同步调一次 + 答案生成）。
  关键词提取占总延迟约 1/3，**尚未优化**——它是产品取舍（可以直接删、合并进意图识别那次调用、
  或换成本地分词），不是 bug。

---

## 八、并发爬坡实测结果（找天花板）

条件：32 逻辑核同一台机器上跑 k6 + 后端 + MySQL + mock，mock 每次上游调用注入 200ms 延迟，
关闭思考时间（`SLEEP=0`），每档跑 20~30 秒。

### 8.1 业务链路（RAG 端到端）：1 → 200 并发**完全线性，零劣化**

| 并发 | RPS | avg | p95 | p99 | 失败 |
|---|---|---|---|---|---|
| 1 | 1.72 | 580ms | 611ms | 851ms | 0 |
| 5 | 8.63 | 579ms | 612ms | 1058ms | 0 |
| 10 | 17.29 | 578ms | 612ms | 1060ms | 0 |
| 20 | 34.64 | 577ms | 613ms | 1079ms | 0 |
| 50 | 86.24 | 576ms | 614ms | 837ms | 0 |
| 100 | 173.78 | 571ms | 613ms | 627ms | 0 |
| 200 | **347.74** | 570ms | 613ms | 642ms | 0 |

**RPS 严格等于 `并发 ÷ 0.575s`，延迟从头到尾平坦在 570ms / p95 613ms。**
说明这个区间内**没有任何排队**，瓶颈就是上游 LLM 的延迟（200ms × 3 次串行调用）。

> 200 VU 时 CPU 约 1.9/32 核（6%）—— 离自己的天花板还远得很。

### 8.2 纯本地路径（不碰模型）：找应用自身天花板

| 并发 | RPS | avg | p95 | p99 | 失败 |
|---|---|---|---|---|---|
| 1 | 670 | 1.20ms | 1.66ms | 1.88ms | 0 |
| 10 | **6485** | 1.24ms | 2.03ms | 3.00ms | **0** |
| 50 | 8807 | 4.87ms | 21.15ms | 36.67ms | 9395 |
| 100 | 8679 | 11.12ms | 72.11ms | 88.29ms | 26320 |
| 200 | **11216** | 17.10ms | 47.86ms | 73.78ms | 9581 |
| 400 | 8974 | 29.84ms | 28.35ms | 228.54ms | 15250 |

- 干净数据点是 **10 并发 / 6485 RPS / p95 2ms / 0 失败**
- 50 并发起出现失败并且 RPS 停在 **~9000–11000 的平台**上

### 8.3 ⚠️ 这个平台是**测试机端口耗尽**，不是应用上限

失败请求的真实报错（后端日志）：

```
dial tcp 127.0.0.1:3306: connectex: Only one usage of each socket address ...   ← MySQL
dial tcp 127.0.0.1:8099: connectex: Only one usage of each socket address ...   ← mock
```

`netsh int ipv4 show tcpstats`：

```
Active Opens:      134,330
Attempts Failed:    80,413   ← 建连失败率 58%
```

`netsh int ipv4 show dynamicportrange tcp`：**49152–65535，只有 16384 个端口**。

k6、后端、MySQL、mock 全在同一台机器上，抢同一段动态端口。高并发 + 短连接 = 端口耗尽。

**CPU 换算**：local@10 时 15 秒窗口消耗 66.51s CPU = 4.43 核（32 核的 14%），
单请求约 **0.84ms CPU** → 32 核理论天花板约 **38,000 RPS**。
实测却停在 11,000 —— **只有理论值的 29%，先崩的是端口不是 CPU**。

**怎么办**（如果要在单机压更高）：
- 扩大动态端口范围：`netsh int ipv4 set dynamicport tcp start=10000 num=55535`（需重启）
- 缩短 TIME_WAIT：`TcpTimedWaitDelay` 调小
- 关掉短连接：确保客户端和服务端都复用连接（本项目已调大 `MaxIdleConnsPerHost`）
- **最有效：把 k6 放到另一台机器上跑**，别和被测服务抢端口

### 8.4 同会话并发：会话锁的串行化证明

| 并发 | RPS | avg | p95 |
|---|---|---|---|
| 1 | 1.76 | 566ms | 613ms |
| 5 | 1.74 | 2714ms | 3652ms |
| 10 | 1.77 | 5052ms | 6090ms |
| 20 | 1.82 | 8998ms | 12156ms |
| 50 | 1.94 | 17852ms | 29435ms |
| 100 | 2.28 | 23622ms | 42969ms |

**RPS 恒定在 1.7~2.3，完全不随并发增长；延迟从 566ms 线性涨到 23.6 秒。**
这就是 `AIHelper.turn` 会话锁的效果：吞吐上限 = `1 ÷ 单请求耗时` = 1.77 RPS，**与并发数无关**。

> 这是设计选择（保证同一会话的消息顺序），不是 bug。但它决定了：
> **压测的并发数不能超过"账号数 × 每账号会话数"**，否则测的是排队而不是吞吐。

### 8.5 超过账号数时会发生什么

`multi` 场景用 200 个账号压到 300 并发时，结果是**断路器跳闸**：

```
deeptalk_circuit_breaker_events_total{name="llm:qwen-turbo",to="open"} 2
deeptalk_circuit_breaker_rejected_total{name="llm:qwen-turbo"}      173899
deeptalk_ai_requests_total{model="qwen-turbo",status="error"}       173914
```

建连失败 → embedding 调用报错 → 连续失败触发熔断 → 后续 17 万次请求被**快速拒绝**。
所以那一轮的 5720 RPS 是"快速失败的假高吞吐"，**不是有效数据**。

熔断器的行为是**正确**的（这正是它的目的），但解读压测结果时要能识别出来：
**RPS 突然飙升 + 大量错误码 5004 = 熔断在快速失败，不是性能变好。**

---

## 九、常见问题

| 现象 | 原因与处理 |
|---|---|
| 启动报 `strings cannot contain newlines` 或 `Near line N` | **编码问题，不是语法问题**。多半是用了 `Get-Content -Raw` + `Set-Content` 复制配置：Windows PowerShell 5.1 默认按 ANSI(GBK) 读写，把 UTF-8 的中文转坏了，连字符串的闭合引号都会丢。按「步骤 0」用 .NET API + 显式 UTF-8 重新生成 |
| 大量 `4002` | 限流。关掉 `[rateLimit] enabled`，或把账号数提到 目标QPS÷2 |
| 大量 `4003` | 配额用尽。调大 `aiPricing.dailyTokenQuota` 或换日期 key |
| 全是 `2006` 无效 Token | `users.json` 和当前 `config.toml` 的 `jwtConfig.key` 不匹配。重跑 prep，并确认 prep 与后端用同一个配置文件 |
| 全是 `2009` 会话不存在 | 会话被 `-clean` 删了或换了库。重跑 prep |
| 压不动 / RPS 上不去 | 先看是不是账号数不够（会话锁串行化）。其次是 k6 客户端侧：默认 `batchPerHost` 等连接设置偏保守，可加 `--batch-per-host 0` 或提高 VU 数；再不行用 pprof 看服务端 |
| 延迟高但服务端 `ai_request_duration` 很低 | 时间花在锁等待 / DB / 序列化，用 block profile 定位 |
| prep 建索引失败 | Redis 不可达。不影响压测（会走 RAG 降级路径），但想测检索必须先把 Redis 起起来 |
| 想压 RAG 但检索一直是 0 条 | `uploads/<账号>/` 下没有文件，或索引和查询用了不同的 embedding 服务（**建索引和压测必须是同一个 baseUrl**），或 mock 的 `-dim` 与配置的 `dimension` 不一致 |
| pprof 打不开 | 确认配置里 `debug.pprofEnabled = true`，且访问的是 `127.0.0.1:6060`（默认只监听本机） |

---

## 九、收尾清理

```powershell
# 1. 停掉压测用的后端（或者 Ctrl+C）
Get-Process deeptalk-stress -ErrorAction SilentlyContinue | Stop-Process -Force

# 2. 删掉测试账号、会话、消息、上传文件、向量索引（-cleanOnly 只删不重建）
go run ./stress/prep -users 50 -cleanOnly

# 3. 删掉临时文件（已被 .gitignore 忽略，删不删都行）
Remove-Item -Recurse -Force stress\.tmp, stress\users.json
```

> 注意区别：`-clean` 是**先清理再重建**（换文档内容重造数据时用），
> 收尾清理要用 `-cleanOnly`，否则会把账号又建回来。

**最后务必确认**：正常启动服务时不带 `DEEPTALK_CONFIG`，用的是你自己的 `config/config.toml`
（里面 `rateLimit` 没有 `enabled=false`，`debug.pprofEnabled` 是 false）。

---

## 十、给面试/复盘用的一段话

> 我的压测分两轮。**第一轮把上游 LLM mock 掉**，因为不打 mock 测出来的其实是上游的限流和排队，
> 不是我的系统 —— 这一轮测出的才是我自己的吞吐上限。第二轮打真实上游，只看端到端 P95，
> 不用来算吞吐。
>
> 压测前我先拆掉三个天花板：限流是单用户 2 QPS，所以必须多用户压；账号和会话数必须 ≥ 并发数，
> 否则 `AIHelper` 的会话锁会把并发变成排队；还要开 pprof，不然压完不知道瓶颈在哪。
>
> 这套工具跑第一轮就抓到一个真问题：**Redis 不可达时熔断器永远不跳闸**。
> 因为 `IsSuccessful` 把 `context.DeadlineExceeded` 当成了非故障，而 go-redis 的连接超时
> 返回的错误正好满足这个判定，导致连续失败计数永远是 0。后果是每个请求都白等 2 秒，
> p50 卡在 2058ms。修掉之后同样场景 RPS 从 2.3 涨到 35.9，p50 从 2058ms 降到 1ms。
> 这个坑的影响面比 Redis 大 —— **任何以超时形式出现的故障都不会触发熔断**，
> 而超时恰恰是最常见的故障形态。
