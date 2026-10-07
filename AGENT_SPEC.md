DeepTalk 高并发推理服务改造规格书
文档性质：AI Coding Agent 可执行需求规格（Feature Spec）
项目定位：高并发多模型推理服务 + Agent 编排引擎
执行方式：按 PHASE 0 → 9 顺序执行，每个 PHASE 独立可用、可验证、可回滚
配套文档：ARCHITECTURE.md 为 PHASE-0 的执行细则
文档版本：v2.0

0. 元信息与全局约束
0.1 环境
text
仓库根目录：C:\Users\32519\Desktop\GoProject\DeepTalk
module：deeptalk
Go：1.25.0
构建缓存：$env:GOCACHE = Join-Path $env:TEMP "gocache"
配置文件：$env:DEEPTALK_CONFIG = <repo>/config/config.toml
0.2 全局验收命令（每个 PHASE 结束都必须全绿）
powershell
$env:GOCACHE = Join-Path $env:TEMP "gocache"
$env:DEEPTALK_CONFIG = "$PWD\config\config.toml"
go build ./...
go vet ./...
go test ./...
go test -race ./...
0.3 硬性约束
ID	约束
C1	不得破坏已有 HTTP 契约：路由路径、请求/响应 JSON 字段名、TOML key、Redis key 前缀、MQ 队列名、deeptalk_* 指标名
C2	每个 PHASE 结束必须 go build && go test 全绿，且服务可启动、可完成一次完整对话
C3	每个 PHASE 必须可回滚：完成后打 git tag phase-N-done，回滚 = git revert
C4	依赖单向：见 3.2 节依赖图，CI 用 go list -deps 校验
C5	包注释必须写（// Package xxx ...），中文，一句话
C6	不新增第三方依赖除非本规格明确指定；新增必须在 go.mod 注释说明理由
C7	所有新增能力必须有对应的验收命令或评测用例，禁止“实现了但无法验证”
C8	涉及 DB 的改动必须提供迁移 SQL，且不得破坏已有数据
C9	每个 PHASE 完成后按第 7 章格式输出报告
C10	性能相关 PHASE 必须产出压测数据：QPS / P50 / P95 / P99 / 错误率 / 资源占用
1. 目标与范围
1.1 一句话目标
把当前“5 种互斥模型 + 硬编码检索管线”的应用，改造成高并发、低延迟、可观测、可评测的多模型推理服务：推理请求可调度、多级缓存可命中、限流配额可分布式、Agent 决策可评测。

1.2 能力清单（现状 → 目标）
#	能力	现状	目标 PHASE
1	多轮对话 + 流式	✅	保留
2	原生 function calling	✅	保留
3	ReAct 循环	✅	PHASE-2 升级（加预算/超时）
4	MCP 工具生态	✅	PHASE-1 融合进统一 Agent
5	RAG 全链路	✅	PHASE-4 改造为工具
6	记忆压缩	✅	保留 + PHASE-9 融入预算分配
7	熔断 + 限流	⚠️ 单机	PHASE-6 升级为分布式
8	指标 + pprof + 成本计量	✅	保留 + 扩展
9	RAG 效果评测	✅	保留 + PHASE-7 扩展轨迹评测
10	推理请求调度	❌	PHASE-3
11	多级缓存体系	⚠️ 仅语义缓存	PHASE-5
12	分布式限流与配额	❌	PHASE-6
13	超时控制（单步/整轮）	⚠️ 仅单工具 15s	PHASE-2
14	重试（工具失败/改参）	❌	PHASE-2
15	预算上限（token/步数/时间）	⚠️ 仅 MaxStep	PHASE-2
16	错误兜底链	⚠️ 仅熔断	PHASE-2
17	轨迹记录	❌	PHASE-7
18	流式步骤可视化	❌	PHASE-7
19	检索即工具	❌	PHASE-4
20	统一 Agent 模型（type 6）	❌	PHASE-4
21	Query Rewrite（指代消解）	❌	PHASE-4
22	Skill 一等公民	❌	PHASE-8
23	上下文预算动态分配	⚠️ 固定值	PHASE-9
24	多 Agent 协作	❌	可选探索，不做主线
1.3 明确不做（防止范围蔓延）
不做	原因
长期记忆（跨会话事实抽取）	需要实体抽取 + 冲突消解，2-3 周内做不扎实
语义记忆（历史对话向量检索）	与 RAG 索引复用会互相污染，需独立设计
Rerank 精排	需要引入 rerank 模型，成本与依赖都超出本轮范围
人在环审批（HITL）	需要前端交互设计，与多 Agent 并行做会失控
引用可验证性	依赖 Rerank 与抽取，放在下一轮
DDD / 聚合根 / 领域事件	过度设计
DI 框架（wire/fx）	手工注入足够
多 Agent 主线	工具数 < 20 时收益低、复杂度高，改为可选探索
1.4 明确要删除的功能
删除	影响面
modelType 4 Ollama 及 OllamaModel	常量、工厂注册、前端选项
tools.go 的计算器/时间/字数/天气工具	RegisterAllTools()、ReAct 模型注册
图片识别模块	router/Image.go、controller/image/、service/image/、common/image/、前端 ImageRecognition.vue 及路由
TTS 模块	/chat/tts/play 路由、controller/tts/、common/tts/、前端调用点
注：图片识别与 TTS 的删除涉及前端，若本轮不动前端，则后端保留路由但标记 Deprecated。

2. 模型类型收敛方案
2.1 收敛结果
新 modelType	名称	职责	来源
1	DeepSeek	纯对话（对照组）	保留
2	RAG	确定性检索管线（强制检索）	保留
6	Unified Agent	Agent + MCP + Skill + 检索工具	新增（融合原 3 与 5）
~~3~~	~~MCP~~	—	融合进 6
~~4~~	~~Ollama~~	—	删除
~~5~~	~~ReAct~~	—	融合进 6
2.2 兼容要求
要求	说明
旧会话数据	sessions.model_type 已有 3/4/5 的历史数据
映射规则	读取时 3→6、5→6、4→2
映射位置	service/session 内统一的 normalizeModelType()
前端	MODEL_OPTIONS 改为 3 项；已有会话仍显示原绑定值（只读）
数据库	不改 schema，靠读取时映射
2.3 保留 2（RAG）独立的原因
text
RAG 独立存在不是历史包袱，而是「确定性保证」这条路径：
  企业内部制度问答要求「必然检索、基于文档回答」，
  不能让模型自主决定「要不要查文档」——凭记忆编制度条款是生产事故。
Unified Agent 提供灵活性（可组合工具），RAG 提供确定性（必然检索）。
评测要对比两条路径的召回率、工具调用准确率、token 成本。
3. 目标架构
3.1 目录结构
text
cmd/
  server/main.go                  唯一依赖组装点
  eval/main.go                    评测 CLI

internal/
  agent/                          ★ Agent 核心
    core/
      agent.go                    Agent 接口、Request/Response
      loop.go                     ReAct 循环（含预算/超时/重试编排）
      context.go                  上下文组装
      trace.go                    轨迹记录器
    memory/
      short.go                    短期记忆
      compressor.go               记忆压缩
      budget.go                   ★ 上下文预算动态分配
    tool/
      spec.go                     ToolSpec
      registry.go                 工具注册表
      retriever_tool.go           ★ search_docs
      cache.go                    工具结果缓存
      retry.go                    重试策略
    skill/                        ★ Skill 一等公民
      skill.go
      registry.go
      loader.go
      builtin/
    guard/                        ★ 可靠性
      timeout.go
      budget.go
      fallback.go

  decision/                       意图识别（规则层 + 主模型兜底）
    intent.go                     规则层：加权词表 / 整词匹配 / 指代与元问题守卫
    llm_intent.go                 LLM 兜底：function calling 结构化输出
    cache.go                      判定结果 TTL 缓存

  llm/                            ★ 模型接入
    llmcore/
      iface.go
    deepseek.go
    rag.go
    unified.go
    factory.go

  rag/                            检索
    chunk.go
    retrieve.go
    fusion.go
    relevance.go
    rewrite.go

  inference/                      ★★ 高并发推理核心（新增）
    scheduler.go                  请求调度器
    pool.go                       模型实例池
    queue.go                      优先级队列
    balancer.go                   负载均衡
    batcher.go                    动态批处理
    breaker.go                    熔断器（增强版）
    metrics.go                    推理指标

  cache/                          ★★ 多级缓存（新增）
    exact.go                      精确缓存（LRU + TTL）
    semantic.go                   语义缓存
    prefix.go                     前缀缓存
    penetration.go                穿透/击穿/雪崩防护
    coordinator.go                多级缓存协调

  ratelimit/                      ★★ 分布式限流（新增）
    token_bucket.go               Redis 令牌桶
    sliding_window.go             滑动窗口
    quota.go                      用户/模型配额
    fallback.go                   本地降级

  infra/                          基础设施
    config/  logger/  metrics/  resilience/  diag/
    mysql/  redis/  rabbitmq/  mcp/  email/

  eval/                           评测
    golden/
    runner.go                     RAG 效果评测
    trace_eval.go                 ★ 轨迹评测
    perf_eval.go                  ★★ 性能评测
    metrics.go

controller/  service/  dao/  model/  middleware/  router/    （保留现有分层）
3.2 依赖方向（CI 用 go list -deps 校验）
text
llmcore      → 无内部依赖
decision     → llmcore(可选), infra/config, infra/metrics
rag          → infra/redis, infra/config
cache        → infra/redis, infra/config
ratelimit    → infra/redis, infra/config
inference    → llmcore, infra/config, infra/metrics, infra/resilience
agent/tool   → llmcore, rag, cache
agent/memory → llmcore, infra/config
agent/core   → llmcore, agent/tool, agent/memory, agent/guard, decision(意图预判，可选), inference
agent/skill  → agent/tool
llm          → llmcore, agent/core, rag, inference, cache, ratelimit
service      → llm, dao
禁止：任何反向依赖、循环依赖。

3.3 核心接口定义
3.3.1 Agent（internal/agent/core/agent.go）
go
type Agent interface {
    Run(ctx context.Context, req Request) (*Response, error)
    Stream(ctx context.Context, req Request, cb StreamCallback) (*Response, error)
}

type Request struct {
    SessionID string
    UserName  string
    Query     string
    History   []Message
    Skills    []string
    Priority  Priority   // ★ 新增：请求优先级
}

type Priority int

const (
    PriorityLow Priority = iota
    PriorityNormal
    PriorityHigh
)

type Response struct {
    Content       string
    TraceID       string
    Steps         int
    Usage         schema.TokenUsage
    Degraded      bool
    DegradeReason string
    CacheHit      CacheLevel  // ★ 新增：命中哪级缓存
    QueueWaitMs   int64       // ★ 新增：排队等待时间
}

type CacheLevel int

const (
    CacheNone CacheLevel = iota
    CacheExact
    CacheSemantic
    CachePrefix
)
3.3.2 Scheduler（internal/inference/scheduler.go）★ 新增
go
// Scheduler 负责推理请求的排队、调度、分发。
// 目标：高优先级请求不被低优先级阻塞，慢模型不拖垮快模型。
type Scheduler interface {
    Submit(ctx context.Context, req *InferenceRequest) (*InferenceResult, error)
    Stats() SchedulerStats
}

type InferenceRequest struct {
    ModelName string
    Prompt    string
    Priority  Priority
    Timeout   time.Duration
    Metadata  map[string]string
}

type InferenceResult struct {
    Output     string
    Usage      schema.TokenUsage
    LatencyMs  int64
    QueueMs    int64
    ModelName  string
}

type SchedulerStats struct {
    QueueDepth     map[string]int
    InFlight       map[string]int
    AvgQueueMs     map[string]int64
    AvgLatencyMs   map[string]int64
    RejectedTotal  int64
}
3.3.3 ModelPool（internal/inference/pool.go）★ 新增
go
// ModelPool 管理某个模型的多个实例（或并发槽位）。
// 支持：最大并发、排队上限、超时拒绝、健康检查。
type ModelPool interface {
    Acquire(ctx context.Context, priority Priority) (*Slot, error)
    Release(slot *Slot)
    Health() PoolHealth
}

type Slot struct {
    ModelName string
    AcquiredAt time.Time
}

type PoolHealth struct {
    MaxConcurrent int
    InUse         int
    Waiting       int
    Healthy       bool
}
3.3.4 MultiLevelCache（internal/cache/coordinator.go）★ 新增
go
// MultiLevelCache 协调三级缓存：精确 → 语义 → 前缀。
// 查询顺序：L1 精确 → L2 语义 → L3 前缀 → 回源。
type MultiLevelCache interface {
    Get(ctx context.Context, req *CacheRequest) (*CacheResult, error)
    Set(ctx context.Context, req *CacheRequest, resp *CacheResponse) error
    Stats() CacheStats
}

type CacheRequest struct {
    UserName  string
    ModelName string
    Prompt    string
    Embedding []float32  // 语义缓存用
}

type CacheResult struct {
    Hit    bool
    Level  CacheLevel
    Output string
    Usage  schema.TokenUsage
}

type CacheStats struct {
    ExactHitRate    float64
    SemanticHitRate float64
    PrefixHitRate   float64
    TotalSavedTokens int64
}
3.3.5 RateLimiter（internal/ratelimit/token_bucket.go）★ 新增
go
// RateLimiter 分布式限流，基于 Redis + Lua 保证原子性。
// 支持：用户级、模型级、全局级三层配额。
type RateLimiter interface {
    Allow(ctx context.Context, key string, cost int) (bool, error)
    Quota(ctx context.Context, key string) (*QuotaInfo, error)
}

type QuotaInfo struct {
    Limit     int
    Remaining int
    ResetAt   time.Time
}
3.3.6 ToolSpec（internal/agent/tool/spec.go）
go
type ToolSpec struct {
    Name        string
    Description string
    Info        *schema.ToolInfo
    Handler     func(ctx context.Context, args string) (string, error)

    Timeout     time.Duration
    RetryPolicy RetryPolicy
    Idempotent  bool
    SkillName   string
    CacheTTL    time.Duration
}

type RetryPolicy struct {
    MaxAttempts  int
    BackoffBase  time.Duration
    AllowReplan  bool
}
4. PHASE 清单
每个 PHASE 的“验收”必须可自动执行，不允许“人工看一眼觉得对”。

PHASE-0：清理 + 结构调整（地基）
前置：无
周期：2 天
可停：是

目标
删除 1.4 列出的功能

完成 ARCHITECTURE.md 第 0~6 章的包拆分

建立 internal/ 目录骨架

改动清单
动作	内容
删	OllamaModel、工厂 creators["4"]、ModelTypeOllama、前端选项
删	tools.go 的计算器/时间/字数/天气工具
删/标记	图片识别、TTS
拆	按 ARCHITECTURE.md 拆 common/aihelper
迁	common/{config,logger,metrics,...} → internal/infra/
建	internal/agent/ internal/decision/ internal/llm/ internal/rag/ internal/inference/ internal/cache/ internal/ratelimit/ 空目录骨架
验收
powershell
go build ./... ; go vet ./... ; go test ./... ; go test -race ./...

Test-Path internal/infra/config, internal/agent, internal/decision, internal/llm, internal/rag, internal/inference, internal/cache, internal/ratelimit   # 全部 True
Test-Path common/aihelper/model.go   # False
PHASE-1：统一 Agent 骨架 + MCP 融合
前置：PHASE-0
周期：3 天
可停：是

目标
把原 MCP + ReAct 融合成统一 Agent 骨架，为后续 PHASE 提供基础。

新增文件
text
internal/agent/core/agent.go
internal/agent/core/loop.go
internal/agent/tool/spec.go
internal/agent/tool/registry.go
internal/llm/unified.go
验收
验收项	期望
统一 Agent 可跑通	问一个问题，走 ReAct 循环返回答案
工具注册可用	注册一个假工具，Agent 能调用
契约一致	路由、JSON 字段、TOML key 不变
PHASE-2：可靠性三件套（超时 / 重试 / 预算）+ 错误兜底链
前置：PHASE-1
周期：3 天
可停：是

目标
Agent 循环具备“不会跑飞、不会挂死、挂了有兜底”的保证。

新增文件
text
internal/agent/guard/timeout.go
internal/agent/guard/budget.go
internal/agent/guard/fallback.go
internal/agent/tool/retry.go
行为规格
能力	规格
单步超时	工具 15s、模型 60s，超时作为 observation 回填
整轮超时	默认 120s，超时返回部分内容 + 提示
重试	仅幂等工具自动重试，MaxAttempts 默认 2，指数退避
改参重试	AllowReplan = true 时允许模型换参数重试一次
预算上限	MaxSteps（8）、MaxTokens（20000）、MaxWallClock（120s）
错误兜底链	主模型失败 → 重试 → 备用模型 → 纯对话 → 错误码
新增配置
toml
[agent]
maxSteps = 8
maxTokens = 20000
maxWallClockSeconds = 120
toolTimeoutSeconds = 15
modelTimeoutSeconds = 60
toolMaxAttempts = 2

[llm]
fallbackModels = ["qwen-turbo", "deepseek-chat"]
新增指标
text
deeptalk_agent_budget_exceeded_total{reason="steps|tokens|wallclock"}
deeptalk_agent_tool_retry_total{tool="...",result="success|fail"}
deeptalk_agent_degraded_total{level="retry|fallback_model|plain_chat|error"}
deeptalk_agent_timeout_total{kind="tool|model|turn"}
验收
用例	构造方式	期望
工具超时不影响整轮	注入 sleep 超时假工具	返回成功，observation 含超时信息
非幂等工具不重试	恒失败 Idempotent=false	调用次数 == 1
幂等工具重试成功	前 1 次失败第 2 次成功	调用次数 == 2
步数超限停止	MaxSteps=2	停止返回部分结果
token 超限停止	MaxTokens=100	同上
主模型失败切备用	恒失败主模型 + 可用备用	degraded_total{level="fallback_model"} +1
全部失败返回错误	主备都失败	返回错误码，不 panic
PHASE-3：推理请求调度 ★★ 核心新增
前置：PHASE-2
周期：5 天
可停：是

目标
把“每个请求直接调用模型”改造成“经过调度器排队、分发、限流”，支撑高并发。

新增文件
text
internal/inference/scheduler.go
internal/inference/pool.go
internal/inference/queue.go
internal/inference/balancer.go
internal/inference/breaker.go
internal/inference/metrics.go
行为规格
能力	规格
模型实例池	每个模型维护 maxConcurrent 个槽位，超出排队
优先级队列	三级优先级：High / Normal / Low，High 优先出队
排队上限	每模型排队上限，超出直接拒绝（快速失败）
排队超时	请求排队超过 queueTimeout 直接拒绝
负载均衡	多实例时按加权轮询 / 最少连接分发
熔断	连续失败 N 次打开熔断，半开探测恢复
优雅关闭	收到 SIGTERM 后停止接收新请求，等待在途请求完成
新增配置
toml
[inference]
enabled = true
queueTimeoutMs = 3000
maxQueueDepth = 200

[inference.pools.deepseek]
maxConcurrent = 20
weight = 10

[inference.pools.qwen]
maxConcurrent = 30
weight = 10

[inference.breaker]
failureThreshold = 5
openDurationMs = 10000
halfOpenProbes = 3
新增指标
text
deeptalk_inference_queue_depth{model="..."}
deeptalk_inference_inflight{model="..."}
deeptalk_inference_queue_wait_ms{model="...",quantile="0.5|0.95|0.99"}
deeptalk_inference_latency_ms{model="...",quantile="0.5|0.95|0.99"}
deeptalk_inference_rejected_total{model="...",reason="queue_full|queue_timeout|breaker_open"}
deeptalk_inference_breaker_state{model="...",state="closed|open|half_open"}
验收
验收项	期望
排队生效	并发 100 请求 / maxConcurrent 10，队列深度上升后回落
优先级生效	High 请求平均排队时间 < Low 请求
排队超时拒绝	队列打满后新请求返回 queue_timeout
熔断生效	注入恒失败模型，5 次后熔断打开，后续请求快速失败
半开恢复	熔断打开 10s 后，探测成功则关闭
优雅关闭	SIGTERM 后正在处理的请求完成，新请求拒绝
压测数据	并发 100 / 持续 60s，P99 延迟、QPS、错误率有数据
powershell
go test ./internal/inference/... -v
# 压测
k6 run scripts/k6/inference_scheduler.js
PHASE-4：检索即工具 + 统一 Agent（type 6）+ Query Rewrite
前置：PHASE-3
周期：3 天
可停：是

目标
RAG 检索封装成工具 search_docs，Agent 自主决定是否调用

新增 modelType 6 = Unified Agent

补上“识别了指代但不消解”的链路断层

新增文件
text
internal/agent/tool/retriever_tool.go
internal/rag/rewrite.go
internal/llm/unified.go
工具定义
工具名	入参	返回	幂等	缓存 TTL
search_docs	{query, top_k}	命中分片 + 来源 + 相似度	✅	60s
list_documents	{}	已索引文档列表	✅	300s
get_document	{doc_id, chunk_range}	指定分片原文	✅	300s
Query Rewrite 规格
text
触发条件：满足任一
  - 意图层标记「含指代词」
  - 问题长度 < 8 字符且存在历史
动作：一次 LLM 调用，把指代替换为实体
产出：rewriteQuery
降级：调用失败 / 超时 800ms / 结果为空 → 使用原始 query
记录：rewrite 前后对比写入轨迹
验收
验收项	期望
Agent 能自主调用 search_docs	问制度类问题，轨迹中出现 tool_name = search_docs
Agent 能自主不调用	问“你好”，轨迹中无 search_docs
rewrite 生效	多轮对话中问“那它呢”，轨迹记录 rewrite 前后
rewrite 失败降级	注入 rewrite 超时，检索仍用原 query
RAG（type 2）不受影响	行为与 PHASE-2 前完全一致
PHASE-5：多级缓存体系 ★★ 核心新增
前置：PHASE-4
周期：5 天
可停：是

目标
把“每次请求都打模型”改造成“精确缓存 → 语义缓存 → 前缀缓存 → 回源”的多级缓存体系，降低成本、降低延迟。

新增文件
text
internal/cache/exact.go
internal/cache/semantic.go
internal/cache/prefix.go
internal/cache/penetration.go
internal/cache/coordinator.go
三级缓存规格
级别	命中条件	存储	TTL	预期命中率
L1 精确	prompt 完全一致（hash）	本地 LRU	5min	15-25%
L2 语义	embedding 相似度 ≥ 0.95	Redis 向量	30min	10-20%
L3 前缀	prompt 前缀一致（≥ 50 字符）	Redis	10min	5-15%
防护机制
问题	方案
缓存穿透	空结果也缓存（短 TTL 30s）+ 布隆过滤器
缓存击穿	热点 key 加互斥锁，只让一个请求回源
缓存雪崩	TTL 加随机抖动 ±20%
缓存一致性	写操作后删除精确缓存 + 语义缓存标记失效
新增配置
toml
[cache]
enabled = true
exactTTLSeconds = 300
semanticTTLSeconds = 1800
prefixTTLSeconds = 600
semanticThreshold = 0.95
exactMaxEntries = 10000
jitterRatio = 0.2

[cache.penetration]
bloomFilterEnabled = true
emptyResultTTLSeconds = 30
新增指标
text
deeptalk_cache_hit_total{level="exact|semantic|prefix"}
deeptalk_cache_miss_total{level="exact|semantic|prefix"}
deeptalk_cache_hit_rate{level="exact|semantic|prefix"}
deeptalk_cache_saved_tokens_total
deeptalk_cache_saved_cost_total
deeptalk_cache_penetration_blocked_total
deeptalk_cache_breakdown_lock_wait_ms
验收
验收项	期望
精确缓存命中	同一 prompt 连续请求 3 次，第 2/3 次命中 L1
语义缓存命中	相似问题（如“年假几天”/“年假有几天”）命中 L2
前缀缓存命中	长 system prompt 相同、用户问题不同，命中 L3
穿透防护	查询不存在的 key，布隆过滤器拦截，不打模型
击穿防护	热点 key 失效瞬间，并发 100 请求只有 1 个回源
雪崩防护	TTL 有随机抖动，缓存不会同时失效
成本下降	压测 1000 请求，token 成本下降 ≥ 30%
powershell
go test ./internal/cache/... -v
k6 run scripts/k6/cache_hit.js
PHASE-6：分布式限流与配额 ★★ 核心新增
前置：PHASE-5
周期：4 天
可停：是

目标
把单机限流升级为分布式限流 + 三层配额，支撑多实例部署。

新增文件
text
internal/ratelimit/token_bucket.go
internal/ratelimit/sliding_window.go
internal/ratelimit/quota.go
internal/ratelimit/fallback.go
三层配额规格
层级	key	默认配额	说明
用户级	user:{name}	100 req/min	防止单用户刷
模型级	model:{name}	1000 req/min	防止单模型被打爆
全局级	global	5000 req/min	保护整个服务
限流算法
算法	使用场景	实现
令牌桶	允许突发流量	Redis + Lua 原子操作
滑动窗口	精确限流	Redis ZSET + Lua
降级策略
text
Redis 不可用 → 本地令牌桶（单机限流，精度下降但不阻塞）
Redis 超时 → 快速失败，走本地
配额超限 → 返回 429 + Retry-After
新增配置
toml
[ratelimit]
enabled = true
algorithm = "token_bucket"

[ratelimit.user]
limit = 100
windowSeconds = 60

[ratelimit.model]
limit = 1000
windowSeconds = 60

[ratelimit.global]
limit = 5000
windowSeconds = 60

[ratelimit.fallback]
localLimit = 50
localWindowSeconds = 60
新增指标
text
deeptalk_ratelimit_allowed_total{level="user|model|global"}
deeptalk_ratelimit_rejected_total{level="user|model|global"}
deeptalk_ratelimit_redis_fallback_total
deeptalk_ratelimit_redis_latency_ms{quantile="0.5|0.95|0.99"}
验收
验收项	期望
用户级限流生效	单用户 1 分钟 200 请求，第 101 个返回 429
模型级限流生效	单模型 1 分钟 1500 请求，第 1001 个返回 429
Redis 降级	停掉 Redis，请求仍可用本地限流通过
多实例共享配额	起 2 个服务实例，总配额仍是配置值
限流精度	并发 1000 请求，实际通过数 ≈ 配额数（误差 < 5%）
压测数据	限流下 QPS、P99、拒绝率有数据
powershell
go test ./internal/ratelimit/... -v
k6 run scripts/k6/ratelimit.js
PHASE-7：轨迹记录 + 流式步骤可视化 + 轨迹评测
前置：PHASE-4
周期：4 天
可停：是

目标
Agent 每一步落库 + 可推送前端 + 可评测，产出可量化指标。

新增 DB 表
sql
-- migrations/002_agent_trace.sql
CREATE TABLE IF NOT EXISTS agent_trace (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  trace_id      VARCHAR(64)  NOT NULL,
  session_id    VARCHAR(36)  NOT NULL,
  user_name     VARCHAR(50)  NOT NULL,
  turn          INT          NOT NULL,
  step          INT          NOT NULL,
  step_type     VARCHAR(16)  NOT NULL COMMENT 'llm|tool|decision|context',
  thought       TEXT         NULL,
  tool_name     VARCHAR(64)  NULL,
  tool_args     TEXT         NULL,
  observation   TEXT         NULL,
  error         TEXT         NULL,
  latency_ms    INT          NOT NULL DEFAULT 0,
  prompt_tokens INT          NOT NULL DEFAULT 0,
  output_tokens INT          NOT NULL DEFAULT 0,
  created_at    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_trace (trace_id),
  KEY idx_session_turn (session_id, turn),
  KEY idx_tool (tool_name, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
流式事件协议
text
data: {"step": 1, "stepType": "decision", "detail": "选中工具 search_docs"}
data: {"step": 2, "stepType": "tool", "tool": "search_docs", "status": "start"}
data: {"step": 2, "stepType": "tool", "tool": "search_docs", "status": "end", "latencyMs": 320}
data: {"step": 3, "stepType": "llm", "status": "start"}
评测指标
text
工具选择准确率 = Σ(实际调用工具 ∩ 期望工具) / Σ(期望工具)
工具误用率     = Σ(实际调用工具 ∩ 禁止工具) / 用例数
平均步数       = Σ步骤数 / 成功用例数
任务成功率     = 成功用例数 / 总用例数
单任务 token   = Σ(prompt+completion) / 用例数
CLI
powershell
go run ./cmd/eval -agent
go run ./cmd/eval -agent -compare
go run ./cmd/eval -agent -json
验收
验收项	期望
轨迹完整落库	SELECT * FROM agent_trace WHERE trace_id='xxx' ORDER BY step 返回完整链
步骤顺序正确	step 单调递增无断号
超长结果截断	注入 10KB 假工具，observation ≤ 4096
流式事件不破坏旧前端	旧前端正常显示
五个指标可产出	CLI 输出表格
三种模型可对比	-compare 输出对比表
阈值卡 CI	阈值调高，退出码 == 1
PHASE-8：Skill 一等公民
前置：PHASE-7
周期：3 天
可停：是

目标
新增一个领域能力只需加配置 + 注册工具，不改 agent 核心代码。

新增文件
text
internal/agent/skill/skill.go
internal/agent/skill/registry.go
internal/agent/skill/loader.go
internal/agent/skill/builtin/doc_qa.go
config/skills/doc_qa/skill.toml
config/skills/doc_qa/prompt.md
验收
验收项	期望
新增 skill 不改核心代码	复制 doc_qa 为 demo，改配置重启即生效
单个 skill 配置错误不影响启动	写错一个 skill.toml，服务正常启动
工具名拼错有明确日志	日志出现 skill=demo missing tools=[xxx]
禁用生效	enabled=false 后不出现在可用列表
PHASE-9：上下文预算动态分配
前置：PHASE-2
周期：2 天
可停：是（收尾）

目标
上下文各部分按比例动态分配 token，而非固定值。

规格
toml
[agent.context]
totalBudget = 8000
ratioSystem = 0.10
ratioMemory = 0.15
ratioRetrieval = 0.45
ratioHistory = 0.30
minHistoryTurns = 3
overflowPolicy = "shrink_retrieval"
验收
验收项	期望
长对话不爆预算	20 轮对话，实际 token ≤ totalBudget
检索结果优先裁剪	检索超长时，实际检索 token ≤ quota
system 不被裁	任何情况下 system 完整保留
分配结果可观测	轨迹表出现 step_type=context 行
5. 全局禁止事项
禁止	原因
跳过 PHASE-3 直接做 PHASE-5/6	没有调度器，缓存和限流无意义
跳过 PHASE-7 直接做多 Agent	没有轨迹评测无法证明有效性
破坏已有 HTTP/JSON/TOML/指标契约	违反 C1
让单个 skill 装载失败 / 意图判定失败导致启动失败或对话失败	违反可用性要求
引入 DI 框架、DDD 全套	过度设计
为每个类型生成 interface	只对边界抽接口
把新增能力写成“实现了但无法验证”	违反 C7
修改 deploy/、vue-frontend/ 构建配置	与本次无关
6. 交付物清单（Definition of Done）
每个 PHASE 都要满足
□ go build ./... && go vet ./... && go test ./... && go test -race ./... 全绿
□ 服务可启动，能完成一次完整对话（含流式）
□ 该 PHASE 的验收表逐项通过（附命令与输出）
□ 打 tag phase-N-done
□ 按第 7 章格式输出报告
全部 PHASE 完成后
□ modelType 收敛为 1 / 2 / 6
□ 删除清单中的功能全部移除或标记 Deprecated
□ internal/ 目录结构与第 3.1 节一致
□ 轨迹表 agent_trace 有真实数据
□ cmd/eval 支持 -agent / -compare / -json
□ 产出 modelType 1 / 2 / 6 的对比数据表
□ 产出 推理调度压测数据（QPS / P50 / P95 / P99 / 错误率）
□ 产出 缓存命中率与成本下降数据
□ 产出 分布式限流压测数据
□ config/skills/ 下至少 1 个可用 skill
□ 意图识别在配置缺失 / 模型不可用时降级为默认意图，不阻断对话
□ README.md 补充：Agent 能力、Skill 扩展、意图识别配置、评测指标、性能数据
□ config/config.toml.example 补充全部新增配置项并加注释
7. 输出要求
每个 PHASE 完成后输出以下结构，不得包含多余解释：

text
PHASE-N 完成
- 目标达成：<逐条对照>
- 新增文件：<列表>
- 修改文件：<列表，含改动要点>
- 删除文件：<列表>
- 新增配置：<key = value，列表>
- 新增指标：<指标名，列表>
- 验收结果：
    go build=<pass/fail> vet=<pass/fail> test=<pass/fail> race=<pass/fail>
    该 PHASE 验收表：<逐项 pass/fail + 实际输出摘要>
- 性能数据：<若有，给出 QPS / P50 / P95 / P99 / 错误率 / 资源占用>
- 数据产出：<对比数据表格>
- 契约一致性：路由=<一致/变更> JSON字段=<一致/变更> TOML key=<新增/变更> 指标名=<新增/变更>
- 遗留问题：<无 / 具体描述>
- 回滚方式：<git tag 名 / revert 命令>
附录 A：PHASE 依赖图
text
PHASE-0 清理+结构
   └─ PHASE-1 统一Agent骨架
        └─ PHASE-2 可靠性三件套
             └─ PHASE-3 推理请求调度 ★★
                  └─ PHASE-4 检索即工具+统一Agent+Rewrite
                       ├─ PHASE-5 多级缓存 ★★
                       │    └─ PHASE-6 分布式限流 ★★
                       ├─ PHASE-7 轨迹+可视化+评测
                       │    └─ PHASE-8 Skill
                       └─ PHASE-9 上下文预算分配
关键路径：0 → 1 → 2 → 3 → 4 → 5 → 6 → 7（约 25 天）
可并行：PHASE-9 可在 PHASE-4 后任意时间做；PHASE-8 可在 PHASE-7 后做