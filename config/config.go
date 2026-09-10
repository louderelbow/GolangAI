package config

import (
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
}

type VoiceServiceConfig struct {
	VoiceServiceApiKey    string `toml:"voiceServiceApiKey"`
	VoiceServiceSecretKey string `toml:"voiceServiceSecretKey"`
}

// DeepSeekConfig 大模型（OpenAI 兼容协议）连接配置
// 取值优先级：这里填了就用这里的 > 环境变量 > 代码默认值
//   DEEPSEEK_BASE_URL / OPENAI_BASE_URL    默认 https://api.deepseek.com
//   DEEPSEEK_MODEL_NAME / OPENAI_MODEL_NAME 默认 deepseek-chat
//   DEEPSEEK_API_KEY / OPENAI_API_KEY      默认空
// 这样既能集中写在配置文件里，也兼容"只设环境变量"的部署方式。
type DeepSeekConfig struct {
	BaseURL   string `toml:"baseUrl"`
	ModelName string `toml:"modelName"`
	APIKey    string `toml:"apiKey"`
}

// AiPricingConfig 模型计费（单位：元 / 100 万 token）
// 用于把每个请求的 token 换算成费用并累计，不配置则费用按 0 计（token 仍然统计）
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

type Config struct {
	EmailConfig        `toml:"emailConfig"`
	RedisConfig        `toml:"redisConfig"`
	MysqlConfig        `toml:"mysqlConfig"`
	JwtConfig          `toml:"jwtConfig"`
	MainConfig         `toml:"mainConfig"`
	Rabbitmq           `toml:"rabbitmqConfig"`
	RagModelConfig     `toml:"ragModelConfig"`
	VoiceServiceConfig `toml:"voiceServiceConfig"`
	AiPricingConfig    `toml:"aiPricing"`
	AiPromptConfig     `toml:"aiPrompt"`
	DeepSeekConfig     `toml:"deepSeekConfig"`
	SemanticCache      SemanticCacheConfig `toml:"semanticCache"`
	McpConfig          `toml:"mcpConfig"`
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
