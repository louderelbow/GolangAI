package config

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

type MainConfig struct {
	Port    int    `toml:"port"`
	AppName string `toml:"appName"`
	Host    string `toml:"host"`
}

type EmailConfig struct {
	Authcode string `toml:"authcode"`
	Email    string `toml:"email" `
}

type RedisConfig struct {
	RedisPort     int    `toml:"port"`
	RedisDb       int    `toml:"db"`
	RedisHost     string `toml:"host"`
	RedisPassword string `toml:"password"`
}

type MysqlConfig struct {
	MysqlPort         int    `toml:"port"`
	MysqlHost         string `toml:"host"`
	MysqlUser         string `toml:"user"`
	MysqlPassword     string `toml:"password"`
	MysqlDatabaseName string `toml:"databaseName"`
	MysqlCharset      string `toml:"charset"`
}

type JwtConfig struct {
	ExpireDuration int    `toml:"expire_duration"`
	Issuer         string `toml:"issuer"`
	Subject        string `toml:"subject"`
	Key            string `toml:"key"`
}

type Rabbitmq struct {
	RabbitmqPort     int    `toml:"port"`
	RabbitmqHost     string `toml:"host"`
	RabbitmqUsername string `toml:"username"`
	RabbitmqPassword string `toml:"password"`
	RabbitmqVhost    string `toml:"vhost"`
}

type RagModelConfig struct {
	RagEmbeddingModel string `toml:"embeddingModel"`
	RagChatModelName  string `toml:"chatModelName"`
	RagDocDir         string `toml:"docDir"`
	RagBaseUrl        string `toml:"baseUrl"`
	RagDimension      int    `toml:"dimension"`
	RagApiKey         string `toml:"apiKey"`
	MaxContextTokens  int    `toml:"maxContextTokens"`

	// MaxDistance 相关性阈值（余弦距离，越小越相关）。默认 0.5；文档短、语料口语化时可放宽到 0.7
	RagMaxDistance float64 `toml:"maxDistance"`
	// FallbackTopN 当所有分片都没过阈值时，至少保留几个最相关的分片（默认 3）
	// 以前这里硬编码为 1，会把召回率压到最低
	RagFallbackTopN int `toml:"fallbackTopN"`
}

type AiPricingConfig struct {
	DefaultPromptPrice     float64            `toml:"defaultPromptPrice"`
	DefaultCompletionPrice float64            `toml:"defaultCompletionPrice"`
	PromptPrice            map[string]float64 `toml:"promptPrice"`     // 模型名 -> 元/1M
	CompletionPrice        map[string]float64 `toml:"completionPrice"` // 模型名 -> 元/1M

	// 每用户每日 token 配额（prompt+completion 合计），0 表示不限
	DailyTokenQuota int `toml:"dailyTokenQuota"`
}

// AiPromptConfig 提示词配置
type AiPromptConfig struct {
	// SystemPrompt 稳定的系统提示（放在消息最前面，有利于命中上游前缀缓存）
	SystemPrompt string `toml:"systemPrompt"`
}

// SemanticCacheConfig 语义缓存：相似问题直接返回缓存答案，省 token / 降延迟
// 注意：只对"没有历史上下文"的首轮提问生效（有历史时同样的问题答案不同，缓存会答错）
type SemanticCacheConfig struct {
	Enabled    bool    `toml:"enabled"`
	Threshold  float64 `toml:"threshold"`  // 余弦相似度阈值，建议 0.92 以上
	MaxEntries int     `toml:"maxEntries"` // 最多缓存多少条
	TTLSeconds int     `toml:"ttlSeconds"` // 缓存有效期
}

// McpServerConfig 一个 MCP 服务端（工具来源）
// 二选一：
//   - HTTP：填 url（StreamableHTTP，如 http://localhost:8081/mcp）
//   - stdio：填 command + args（本地子进程，如 npx -y @modelcontextprotocol/server-filesystem /data）
type McpServerConfig struct {
	Name         string   `toml:"name"`
	URL          string   `toml:"url"`          // StreamableHTTP 地址
	Command      string   `toml:"command"`      // stdio 启动命令，如 npx / uvx
	Args         []string `toml:"args"`         // stdio 命令参数
	Env          []string `toml:"env"`          // stdio 额外环境变量，格式 KEY=VALUE
	AllowedTools []string `toml:"allowedTools"` // 工具白名单，为空表示该服务端全部工具可用
}

// McpConfig MCP 工具注册表配置
type McpConfig struct {
	Servers []McpServerConfig `toml:"servers"`
	MaxStep int               `toml:"maxStep"` // Agent 最大推理步数
}

// AgentConfig Agent 循环的可靠性与预算约束（[agent] 段）。
//
// 这些约束的作用是让 Agent "不会跑飞、不会挂死"：
// 每一步都受超时保护，整轮受 token / 步数 / 墙钟三重预算限制。
type AgentConfig struct {
	MaxSteps            int `toml:"maxSteps"`            // 单轮最大推理步数（默认 8）
	MaxTokens           int `toml:"maxTokens"`           // 单轮 token 上限（默认 20000）
	MaxWallClockSeconds int `toml:"maxWallClockSeconds"` // 单轮墙钟上限，秒（默认 120）
	ToolTimeoutSeconds  int `toml:"toolTimeoutSeconds"`  // 单次工具调用超时，秒（默认 15）
	ModelTimeoutSeconds int `toml:"modelTimeoutSeconds"` // 单次模型调用超时，秒（默认 60）
	ToolMaxAttempts     int `toml:"toolMaxAttempts"`     // 幂等工具最大尝试次数（默认 2）

	// LoopGuardThreshold 同一个 (工具 + 参数) 成功调用多少次后开始拦截。
	//
	// 默认 3；设成负数可以关掉（排查"某个工具确实需要重复调用"时用）。
	// 详见 internal/agent/tool/loopguard.go 的三条边界。
	LoopGuardThreshold int `toml:"loopGuardThreshold"`

	// ==================== 调试开关（生产必须留空） ====================
	//
	// ForceClarifyQuestion 非空时，**会话首轮**无论模型有没有调用 ask_user，
	// 都会抛出这个澄清提问；模型自己抛的会被它覆盖（保证演示可复现）。
	//
	// 存在的理由：模型要不要调工具是概率事件（qwen-turbo 实测约一半一半），
	// 而"前端弹窗长什么样""选择有没有进上下文"这两段必须能被确定性地验收，
	// 不该赌模型的心情。
	//
	// 只在首轮生效是刻意的：若每轮都弹，用户永远走不到"选完 → 给出答案"，
	// 这条链路也就测不完整了。
	ForceClarifyQuestion string   `toml:"forceClarifyQuestion"`
	ForceClarifyOptions  []string `toml:"forceClarifyOptions"`
}

func (c AgentConfig) withDefaults() AgentConfig {
	if c.MaxSteps <= 0 {
		c.MaxSteps = 8
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 20000
	}
	if c.MaxWallClockSeconds <= 0 {
		c.MaxWallClockSeconds = 120
	}
	if c.ToolTimeoutSeconds <= 0 {
		c.ToolTimeoutSeconds = 15
	}
	if c.ModelTimeoutSeconds <= 0 {
		c.ModelTimeoutSeconds = 60
	}
	if c.ToolMaxAttempts <= 0 {
		c.ToolMaxAttempts = 2
	}
	return c
}

// GetAgent 返回补全默认值后的 Agent 约束。
func (c *Config) GetAgent() AgentConfig { return c.AgentConfig.withDefaults() }

// ==================== Unified Agent 专用上游 ====================

// AgentModelConfig [agentModel] 段：Unified Agent 用的模型，与 RAG 分开配置。
//
// 为什么不复用 [ragModelConfig]：两者对模型的要求不同。
// RAG 是"检索 + 一次直答"，便宜够用即可；Unified Agent 要在 ReAct 循环里
// 反复决定"调不调工具、参数怎么给"，必须支持可靠的 function calling。
// 共用一个模型名时只能迁就一边——要么 RAG 白花钱，要么 Agent 经常不调工具。
//
// 三段（模型名 / baseURL / key）各自独立回落：整段留空时 Unified Agent
// 与 RAG 共用 [ragModelConfig]，行为与改动前完全一致。
type AgentModelConfig struct {
	ChatModelName string `toml:"chatModelName"`
	BaseUrl       string `toml:"baseUrl"`
	ApiKey        string `toml:"apiKey"`
	// ApiKeyEnv 指定从哪个环境变量读 key。
	// 本段配了独立上游却没写 apiKey 时，默认读 DEEPSEEK_API_KEY。
	ApiKeyEnv string `toml:"apiKeyEnv"`
	// FallbackModels 本段独立上游的备用模型（同一 baseURL / key）。
	// 不写则见 AgentFallbackModels 的回落规则。
	FallbackModels []string `toml:"fallbackModels"`
}

// RagApiKey 取 [ragModelConfig] 上游的 key：配置值优先，其次常见环境变量。
//
// 抽出来是因为同一段兜底顺序原先在 model_rag、model_unified、
// decision.NewIntentLLM 各抄了一遍——改一处顺序要同步三处，迟早漏一个。
func (c *Config) RagApiKey() string {
	if k := strings.TrimSpace(c.RagModelConfig.RagApiKey); k != "" {
		return k
	}
	for _, env := range []string{"ALIYUN_API_KEY", "DEEPSEEK_API_KEY", "OPENAI_API_KEY"} {
		if k := os.Getenv(env); k != "" {
			return k
		}
	}
	return ""
}

// AgentModel 返回 Unified Agent 实际使用的 模型名 / baseURL / API Key。
//
// key 的解析顺序：
//  1. [agentModel].apiKey（写死在配置里）
//  2. [agentModel].apiKeyEnv 指定的环境变量（默认 DEEPSEEK_API_KEY）
//  3. 与 RAG 同一套兜底（RagApiKey）
func (c *Config) AgentModel() (name, baseURL, apiKey string) {
	am := c.AgentModelConfig

	name = strings.TrimSpace(am.ChatModelName)
	if name == "" {
		name = c.RagModelConfig.RagChatModelName
	}

	baseURL = strings.TrimSpace(am.BaseUrl)
	if baseURL == "" {
		baseURL = c.RagModelConfig.RagBaseUrl
	}

	if k := strings.TrimSpace(am.ApiKey); k != "" {
		return name, baseURL, k
	}

	// 配了独立上游却只写了 baseUrl 时，默认去读 DEEPSEEK_API_KEY。
	// 刻意不默认读 ALIYUN_API_KEY：两个变量常常同时存在（.env 里就都有），
	// 拿错一把的结果是一个看不懂的 401，而不是一句能照着改的提示。
	envName := strings.TrimSpace(am.ApiKeyEnv)
	if envName == "" && (am.ChatModelName != "" || am.BaseUrl != "") {
		envName = "DEEPSEEK_API_KEY"
	}
	if envName != "" {
		if k := os.Getenv(envName); k != "" {
			return name, baseURL, k
		}
		log.Printf("[config] ⚠️ [agentModel] 配了独立上游，但环境变量 %s 为空；"+
			"将回落到 RAG 的 key（对 DeepSeek 多半会 401）", envName)
	}

	return name, baseURL, c.RagApiKey()
}

// AgentFallbackModels 返回 Unified Agent 的备用模型名。
func (c *Config) AgentFallbackModels() []string {
	if c.AgentModelConfig.FallbackModels != nil {
		return c.AgentModelConfig.FallbackModels
	}
	// 换了上游还沿用 [llm] 里的模型名，等于拿 DeepSeek 的地址去请求 qwen-turbo，
	// 每次兜底都必然失败——这种"假兜底"比没有兜底更糟（多一次失败计数，
	// 还可能把熔断器推向 open）。所以换上游时默认不给兜底。
	base := strings.TrimSpace(c.AgentModelConfig.BaseUrl)
	if base != "" && base != c.RagModelConfig.RagBaseUrl {
		return nil
	}
	return c.LLMConfig.FallbackModels
}

// LocalAgentConfig [localAgent] 段：允许模型查看**用户自己电脑**上的工作区。
//
// 默认关闭。这是一个"模型可以读用户硬盘"的能力，必须由部署者显式打开，
// 而不是装完就有。打开后服务端会注册 list_files / read_file 两个工具；
// 用户还没在自己电脑上启动 deeptalk-agent 时，这两个工具会回一句
// "未连接"（而不是报错），模型据此如实告知用户。
//
// 真正的安全边界不在这里，而在本地 agent 那端：服务端只能请求，
// 路径是否越界由用户机器上的进程判定。
type LocalAgentConfig struct {
	Enabled bool `toml:"enabled"`
}

// GetLocalAgent 返回本地工作区能力配置。
func (c *Config) GetLocalAgent() LocalAgentConfig { return c.LocalAgentConfig }

// LLMConfig 模型层的兜底配置（[llm] 段）。
//
// FallbackModels 是主模型不可用时的备用模型名（同一 baseURL / apiKey），
// 按顺序尝试；全部失败才返回错误码。
type LLMConfig struct {
	FallbackModels []string `toml:"fallbackModels"`
}

// CachePenetrationConfig 缓存穿透防护配置
type CachePenetrationConfig struct {
	// BloomFilterEnabled 保留字段，本项目未实现布隆过滤器。
	// 原因见 internal/cache/penetration.go：这里的 key 是用户问题，
	// 几乎每条都是新的，布隆过滤器会把所有首次提问判成"一定不存在"，
	// 反而让缓存彻底失效。有界的是文档而不是问题。
	BloomFilterEnabled    bool `toml:"bloomFilterEnabled"`
	EmptyResultTTLSeconds int  `toml:"emptyResultTTLSeconds"` // 空结果（拒答）缓存时长，默认 30s
}

// CacheConfig 多级答案缓存配置（[cache] 段）。
//
// 分层是为了**成本**而不是命中率：
//
//	L1 精确命中 = 0 次模型调用 + 0 次 embedding 调用
//	L2 语义命中 = 0 次模型调用 + 1 次 embedding 调用
//
// 语义缓存得先把问题向量化，这一步本身就是外部调用；L1 用 hash 绕过了它。
//
// 注意 L2 自身的开关/阈值/容量/TTL 仍由已有的 [semanticCache] 段配置——
// 那一段是历史契约，不在这里重复定义，避免同一个缓存出现两个配置源。
type CacheConfig struct {
	// Enabled 用指针区分"没配置"（默认开启）与"显式关闭"。
	// 用普通 bool 的话，配置文件里漏了 [cache] 段就等于把缓存关掉了——
	// 这种"静默失效"比配错更难发现。
	Enabled         *bool                  `toml:"enabled"`
	ExactTTLSeconds int                    `toml:"exactTTLSeconds"` // L1 有效期，默认 300
	ExactMaxEntries int                    `toml:"exactMaxEntries"` // L1 容量上限，默认 10000
	JitterRatio     float64                `toml:"jitterRatio"`     // TTL 抖动比例（防雪崩），默认 0.2
	Penetration     CachePenetrationConfig `toml:"penetration"`
}

// IsEnabled 是否启用多级缓存。
func (c CacheConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

func (c CacheConfig) withDefaults() CacheConfig {
	if c.Enabled == nil {
		enabled := true
		c.Enabled = &enabled
	}
	if c.ExactTTLSeconds <= 0 {
		c.ExactTTLSeconds = 300
	}
	if c.ExactMaxEntries <= 0 {
		c.ExactMaxEntries = 10000
	}
	// 抖动不支持关闭：它是防雪崩的安全机制，配成 0 会让一批 key 同时过期。
	// 因此 <=0 一律按默认值处理。
	if c.JitterRatio <= 0 {
		c.JitterRatio = 0.2
	}
	if c.JitterRatio > 1 {
		c.JitterRatio = 1
	}
	if c.JitterRatio < 0 || c.JitterRatio > 1 {
		c.JitterRatio = 0.2
	}
	if c.Penetration.EmptyResultTTLSeconds <= 0 {
		c.Penetration.EmptyResultTTLSeconds = 30
	}
	return c
}

// GetCache 返回补全默认值后的缓存配置。
func (c *Config) GetCache() CacheConfig { return c.CacheConfig.withDefaults() }

// WithDefaults 导出给测试与外部装配使用，避免零值导致缓存"立即过期 / 容量为 0"。
func (c CacheConfig) WithDefaults() CacheConfig { return c.withDefaults() }

// RagRewriteConfig 指代消解（Query Rewrite）配置（[ragRewrite] 段）。
//
// 意图层能识别"含指代词、依赖上下文"，但检索仍拿原句去查——
// 用户问"那它呢"，等于用"那它呢"做向量检索。这一段负责把指代替换成实体。
type RagRewriteConfig struct {
	// Enabled 用指针区分"没配置"（默认开启）与"显式关闭"（便于 A/B 对照）
	Enabled   *bool `toml:"enabled"`
	TimeoutMs int   `toml:"timeoutMs"` // 改写超时（默认 800ms），超时即回退原句
}

func (c RagRewriteConfig) withDefaults() RagRewriteConfig {
	if c.Enabled == nil {
		enabled := true
		c.Enabled = &enabled
	}
	if c.TimeoutMs <= 0 {
		c.TimeoutMs = 800
	}
	return c
}

// IsEnabled 是否开启指代消解。
func (c RagRewriteConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// GetRagRewrite 返回补全默认值后的指代消解配置。
func (c *Config) GetRagRewrite() RagRewriteConfig { return c.RagRewriteConfig.withDefaults() }

// InferencePoolConfig 单个模型的推理池配置
type InferencePoolConfig struct {
	MaxConcurrent int `toml:"maxConcurrent"` // 同时进行的请求数上限，超出排队
	Weight        int `toml:"weight"`        // 多实例时的加权轮询权重
}

// InferenceBreakerConfig 调度层的准入熔断配置
type InferenceBreakerConfig struct {
	FailureThreshold int `toml:"failureThreshold"` // 连续失败多少次打开（默认 5）
	OpenDurationMs   int `toml:"openDurationMs"`   // open 持续多久后允许半开探测（默认 10000）
	HalfOpenProbes   int `toml:"halfOpenProbes"`   // 半开时允许的探测请求数（默认 3）
}

// InferenceConfig 推理请求调度配置（[inference] 段）
//
// 作用：把"每个请求直接调用模型"改成"先排队、再按槽位分发"，
// 使上游并发可控、过载时快速失败而不是全部超时。
type InferenceConfig struct {
	Enabled        bool                           `toml:"enabled"`
	QueueTimeoutMs int                            `toml:"queueTimeoutMs"` // 排队超过此时长直接拒绝（默认 3000）
	MaxQueueDepth  int                            `toml:"maxQueueDepth"`  // 队列长度上限，超出直接拒绝（默认 200）
	Pools          map[string]InferencePoolConfig `toml:"pools"`          // 按模型名配置
	Breaker        InferenceBreakerConfig         `toml:"breaker"`
}

func (c InferenceConfig) withDefaults() InferenceConfig {
	if c.QueueTimeoutMs <= 0 {
		c.QueueTimeoutMs = 3000
	}
	if c.MaxQueueDepth <= 0 {
		c.MaxQueueDepth = 200
	}
	if c.Breaker.FailureThreshold <= 0 {
		c.Breaker.FailureThreshold = 5
	}
	if c.Breaker.OpenDurationMs <= 0 {
		c.Breaker.OpenDurationMs = 10000
	}
	if c.Breaker.HalfOpenProbes <= 0 {
		c.Breaker.HalfOpenProbes = 3
	}
	// 未配置的模型走一套保守默认值：有并发上限总比无限并发好
	if c.Pools == nil {
		c.Pools = map[string]InferencePoolConfig{}
	}
	return c
}

// GetInference 返回补全默认值后的调度配置。
func (c *Config) GetInference() InferenceConfig { return c.InferenceConfig.withDefaults() }

// ResilienceConfig 熔断器配置（基于 sony/gobreaker）
// 默认开启；Disabled=true 时所有调用直连（只保留原有超时/降级逻辑）
type ResilienceConfig struct {
	Disabled bool `toml:"disabled"`

	FailureThreshold    int     `toml:"failureThreshold"`    // 连续失败多少次跳闸（默认 5）
	FailureRatio        float64 `toml:"failureRatio"`        // 失败率阈值，配合 MinRequests 生效（默认 0.6）
	MinRequests         int     `toml:"minRequests"`         // 统计失败率所需最小请求数（默认 10）
	TimeoutSeconds      int     `toml:"timeoutSeconds"`      // open 持续多久后进入 half-open（默认 30）
	MaxRequestsHalfOpen int     `toml:"maxRequestsHalfOpen"` // half-open 允许的试探请求数（默认 3）
	IntervalSeconds     int     `toml:"intervalSeconds"`     // 关闭态计数窗口滚动周期（默认 60）
}

func (c ResilienceConfig) withDefaults() ResilienceConfig {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.FailureRatio <= 0 {
		c.FailureRatio = 0.6
	}
	if c.MinRequests <= 0 {
		c.MinRequests = 10
	}
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = 30
	}
	if c.MaxRequestsHalfOpen <= 0 {
		c.MaxRequestsHalfOpen = 3
	}
	if c.IntervalSeconds <= 0 {
		c.IntervalSeconds = 60
	}
	return c
}

type Config struct {
	EmailConfig      `toml:"emailConfig"`
	RedisConfig      `toml:"redisConfig"`
	MysqlConfig      `toml:"mysqlConfig"`
	JwtConfig        `toml:"jwtConfig"`
	MainConfig       `toml:"mainConfig"`
	Rabbitmq         `toml:"rabbitmqConfig"`
	RagModelConfig   `toml:"ragModelConfig"`
	AgentModelConfig `toml:"agentModel"`
	LocalAgentConfig `toml:"localAgent"`
	AiPricingConfig  `toml:"aiPricing"`
	AiPromptConfig   `toml:"aiPrompt"`
	SemanticCache    SemanticCacheConfig `toml:"semanticCache"`
	McpConfig        `toml:"mcpConfig"`
	ResilienceConfig `toml:"resilience"`
	IntentConfig     `toml:"intentConfig"`
	RateLimitConfig  `toml:"rateLimit"`
	DebugConfig      `toml:"debug"`
	AgentConfig      `toml:"agent"`
	LLMConfig        `toml:"llm"`
	InferenceConfig  `toml:"inference"`
	RagRewriteConfig `toml:"ragRewrite"`
	CacheConfig      `toml:"cache"`
}

// TolerantFloat 兼容 TOML 里把浮点字段写成整数的写法。
//
// BurntSushi/toml 不会把 `capacity = 10` 隐式转成 float64，会直接报
// "cannot load TOML value of type int64 into a Go float" 然后 log.Fatal，
// 而限流的容量/速率天然就是整数写法，用户几乎必然会踩。
// 用这个类型让 10 和 10.0 都能解析。
type TolerantFloat float64

func (t *TolerantFloat) UnmarshalTOML(v any) error {
	switch n := v.(type) {
	case int64:
		*t = TolerantFloat(n)
	case float64:
		*t = TolerantFloat(n)
	default:
		return fmt.Errorf("期望数字，实际是 %T", v)
	}
	return nil
}

// RateLimitConfig 限流配置（按用户 / 按 IP 的令牌桶）
// 默认值与历史硬编码值完全一致，不配置时行为不变。
// 压测前把 enabled 设成 false（或把 refill 调大），压完务必改回来。
type RateLimitConfig struct {
	// Enabled 用指针是为了区分"没配置"（默认开启）和"显式配置成 false"（关闭）
	Enabled    *bool         `toml:"enabled"`
	Capacity   TolerantFloat `toml:"capacity"`   // 单用户桶容量
	Refill     TolerantFloat `toml:"refill"`     // 单用户每秒补充 token 数（= 平均 QPS 上限）
	IPCapacity TolerantFloat `toml:"ipCapacity"` // 未登录接口按 IP：桶容量
	IPRefill   TolerantFloat `toml:"ipRefill"`   // 未登录接口按 IP：每秒补充
}

func (c RateLimitConfig) withDefaults() RateLimitConfig {
	if c.Enabled == nil {
		enabled := true
		c.Enabled = &enabled
	}
	if c.Capacity <= 0 {
		c.Capacity = 10
	}
	if c.Refill <= 0 {
		c.Refill = 2
	}
	if c.IPCapacity <= 0 {
		c.IPCapacity = 5
	}
	if c.IPRefill <= 0 {
		c.IPRefill = 0.2
	}
	return c
}

// IsEnabled 限流是否开启（未配置时默认开启）
func (c RateLimitConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// GetRateLimit 返回填好默认值的限流配置
func (c *Config) GetRateLimit() RateLimitConfig { return c.RateLimitConfig.withDefaults() }

// DebugConfig 诊断端点配置
// PprofEnabled 打开后会暴露 /debug/pprof，并开启 CPU/阻塞/互斥锁采样——
// 采样本身有开销，所以默认关闭；生产环境只在排查问题时临时打开。
type DebugConfig struct {
	PprofEnabled bool   `toml:"pprofEnabled"`
	PprofAddr    string `toml:"pprofAddr"` // 默认 127.0.0.1:6060（只监听本机）
}

func (c DebugConfig) withDefaults() DebugConfig {
	if c.PprofAddr == "" {
		c.PprofAddr = "127.0.0.1:6060"
	}
	return c
}

// GetDebug 返回填好默认值的诊断配置
func (c *Config) GetDebug() DebugConfig { return c.DebugConfig.withDefaults() }

// IntentConfig 意图识别配置
// 规则层（加权词表 + 整词匹配 + 指代词守卫）永远生效且零成本；
// LLMFallback 控制"规则层不确定时是否调用 LLM 做结构化兜底"（会产生额外调用与费用）
type IntentConfig struct {
	LLMFallback     bool `toml:"llmFallback"`
	CacheTTLSeconds int  `toml:"cacheTTLSeconds"`
	MaxCacheEntries int  `toml:"maxCacheEntries"`
}

// GetResilience 返回填好默认值的熔断配置
func (c *Config) GetResilience() ResilienceConfig {
	return c.ResilienceConfig.withDefaults()
}

type RedisKeyConfig struct {
	CaptchaPrefix         string
	CaptchaCooldownPrefix string
	IndexName             string
	IndexNamePrefix       string
}

var DefaultRedisKeyConfig = RedisKeyConfig{
	CaptchaPrefix:         "captcha:%s",
	CaptchaCooldownPrefix: "captcha:cooldown:%s",
	IndexName:             "rag_docs:%s:idx",
	IndexNamePrefix:       "rag_docs:%s:",
}

var (
	config     *Config
	configOnce sync.Once
)

// ConfigFile 返回实际使用的配置文件路径
// 优先级：环境变量 DEEPTALK_CONFIG > config/config.toml > config/config.toml.example
func ConfigFile() string {
	if p := os.Getenv("DEEPTALK_CONFIG"); p != "" {
		return p
	}

	configFile := "config/config.toml"
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		configFile = "config/config.toml.example"
		log.Println("[WARNING] config.toml not found, using config.toml.example (please fill in your real config)")
	}
	return configFile
}

func InitConfig() error {
	configFile := ConfigFile()

	if _, err := toml.DecodeFile(configFile, config); err != nil {
		log.Fatal(err.Error())
		return err
	}
	return nil
}

// GetConfig 返回全局配置（首次调用时加载，线程安全）。
//
// 原实现是 `if config == nil { config = new(Config); InitConfig() }` 这种
// 检查-再赋值：并发下两个 goroutine 会同时进入初始化，其中一个正在
// DecodeFile 往 config 里写，另一个已经在读它——既可能读到半成品配置，
// 也会被 race detector 抓到。
//
// 每个请求都会走到这里（限流、缓存、模型配置都要读），所以这个竞争
// 是常态而不是边缘情况。用 sync.Once 与同项目其他单例（MCP 注册表、
// 推理调度器）保持一致。
func GetConfig() *Config {
	configOnce.Do(func() {
		config = new(Config)
		if err := InitConfig(); err != nil {
			log.Printf("[config] 加载失败，将使用默认值: %v", err)
		}
	})
	return config
}
