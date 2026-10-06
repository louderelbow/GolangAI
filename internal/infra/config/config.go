package config

import (
	"fmt"
	"log"
	"os"

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
	EmailConfig        `toml:"emailConfig"`
	RedisConfig        `toml:"redisConfig"`
	MysqlConfig        `toml:"mysqlConfig"`
	JwtConfig          `toml:"jwtConfig"`
	MainConfig         `toml:"mainConfig"`
	Rabbitmq           `toml:"rabbitmqConfig"`
	RagModelConfig     `toml:"ragModelConfig"`
	AiPricingConfig    `toml:"aiPricing"`
	AiPromptConfig     `toml:"aiPrompt"`
	SemanticCache      SemanticCacheConfig `toml:"semanticCache"`
	McpConfig          `toml:"mcpConfig"`
	ResilienceConfig   `toml:"resilience"`
	IntentConfig       `toml:"intentConfig"`
	RateLimitConfig    `toml:"rateLimit"`
	DebugConfig        `toml:"debug"`
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

var config *Config

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

func GetConfig() *Config {
	if config == nil {
		config = new(Config)
		_ = InitConfig()
	}
	return config
}
