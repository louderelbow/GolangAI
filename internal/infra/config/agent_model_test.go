package config

import (
	"testing"

	"github.com/BurntSushi/toml"
)

// baseConfig 造一份"RAG 走阿里、Agent 未单独配置"的配置。
func baseConfig() *Config {
	c := new(Config)
	c.RagModelConfig = RagModelConfig{
		RagChatModelName: "qwen-turbo",
		RagBaseUrl:       "https://dashscope.aliyuncs.com/compatible-mode/v1",
		RagApiKey:        "sk-aliyun",
	}
	c.LLMConfig = LLMConfig{FallbackModels: []string{"qwen-turbo"}}
	return c
}

// TestAgentModelFallsBackToRAG 整段留空时必须完全沿用 RAG：
// 这是"加了新配置段但没配"的默认路径，不能改变任何既有行为。
func TestAgentModelFallsBackToRAG(t *testing.T) {
	c := baseConfig()

	name, base, key := c.AgentModel()
	if name != "qwen-turbo" {
		t.Errorf("模型名应沿用 RAG，得到 %q", name)
	}
	if base != c.RagModelConfig.RagBaseUrl {
		t.Errorf("baseURL 应沿用 RAG，得到 %q", base)
	}
	if key != "sk-aliyun" {
		t.Errorf("key 应沿用 RAG 配置，得到 %q", key)
	}

	fb := c.AgentFallbackModels()
	if len(fb) != 1 || fb[0] != "qwen-turbo" {
		t.Errorf("同一上游时应沿用 [llm].fallbackModels，得到 %v", fb)
	}
}

// TestAgentModelUsesOwnUpstreamAndEnvKey 配了独立上游时：模型名/baseURL 用本段，
// key 从 apiKeyEnv（默认 DEEPSEEK_API_KEY）读。
func TestAgentModelUsesOwnUpstreamAndEnvKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-deepseek")
	c := baseConfig()
	c.AgentModelConfig = AgentModelConfig{
		ChatModelName: "deepseek-chat",
		BaseUrl:       "https://api.deepseek.com",
	}

	name, base, key := c.AgentModel()
	if name != "deepseek-chat" {
		t.Errorf("模型名应取本段，得到 %q", name)
	}
	if base != "https://api.deepseek.com" {
		t.Errorf("baseURL 应取本段，得到 %q", base)
	}
	if key != "sk-deepseek" {
		t.Errorf("key 应来自 DEEPSEEK_API_KEY，得到 %q", key)
	}

	// 换了上游又没写 fallbackModels：绝不能沿用 qwen-turbo 的名字，
	// 那是拿 DeepSeek 的地址请求 qwen，每次都必然失败。
	if fb := c.AgentFallbackModels(); fb != nil {
		t.Errorf("换上游且未显式配置时应不给兜底，得到 %v", fb)
	}
}

// TestAgentModelInlineKeyWins 配置里写死的 key 优先于环境变量。
func TestAgentModelInlineKeyWins(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-env")
	c := baseConfig()
	c.AgentModelConfig = AgentModelConfig{
		ChatModelName: "deepseek-chat",
		BaseUrl:       "https://api.deepseek.com",
		ApiKey:        "sk-inline",
	}

	if _, _, key := c.AgentModel(); key != "sk-inline" {
		t.Errorf("配置里写死的 key 应优先，得到 %q", key)
	}
}

// TestAgentModelCustomApiKeyEnv 自定义环境变量名要被认。
func TestAgentModelCustomApiKeyEnv(t *testing.T) {
	t.Setenv("MY_AGENT_KEY", "sk-custom")
	c := baseConfig()
	c.AgentModelConfig = AgentModelConfig{
		ChatModelName: "deepseek-chat",
		BaseUrl:       "https://api.deepseek.com",
		ApiKeyEnv:     "MY_AGENT_KEY",
	}

	if _, _, key := c.AgentModel(); key != "sk-custom" {
		t.Errorf("应读 apiKeyEnv 指定的变量，得到 %q", key)
	}
}

// TestAgentModelFallsBackToRagKeyWhenEnvMissing 环境变量为空时回落 RAG 的 key。
//
// 这是刻意保留的兜底（有人确实想让 Agent 和 RAG 共用一把 key），
// 跨上游复用时它会 401——所以 AgentModel() 里同步打了一条警告日志。
func TestAgentModelFallsBackToRagKeyWhenEnvMissing(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	c := baseConfig()
	c.AgentModelConfig = AgentModelConfig{
		ChatModelName: "deepseek-chat",
		BaseUrl:       "https://api.deepseek.com",
	}

	if _, _, key := c.AgentModel(); key != "sk-aliyun" {
		t.Errorf("环境变量为空时应回落 RAG 的 key，得到 %q", key)
	}
}

// TestAgentFallbackModelsNilVsEmpty 区分"没写"和"显式写空数组"。
//
// 没写 = 沿用 [llm].fallbackModels；写了 [] = 明确不要兜底。
// 两者都是 len==0 的切片，只能靠 nil 与否区分——所以这里把
// BurntSushi/toml 的实际解码行为钉住，免得将来换库或改字段时悄悄失效。
func TestAgentFallbackModelsNilVsEmpty(t *testing.T) {
	const head = `
[ragModelConfig]
chatModelName = "qwen-turbo"
baseUrl = "https://dashscope.aliyuncs.com/compatible-mode/v1"

[llm]
fallbackModels = ["qwen-turbo"]

[agentModel]
chatModelName = "deepseek-chat"
baseUrl = "https://api.deepseek.com"
`

	var absent Config
	if _, err := toml.Decode(head, &absent); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if absent.AgentModelConfig.FallbackModels != nil {
		t.Fatalf("没写 fallbackModels 应为 nil，实际 %#v", absent.AgentModelConfig.FallbackModels)
	}
	if fb := absent.AgentFallbackModels(); fb != nil {
		t.Errorf("换上游且没写时不该继承 [llm] 的模型名，得到 %v", fb)
	}

	var explicit Config
	if _, err := toml.Decode(head+"fallbackModels = []\n", &explicit); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if explicit.AgentModelConfig.FallbackModels == nil {
		t.Fatal("显式 fallbackModels = [] 应解成非 nil 空切片，否则无法与「没写」区分")
	}
	if fb := explicit.AgentFallbackModels(); len(fb) != 0 {
		t.Errorf("显式空数组表示不要兜底，得到 %v", fb)
	}
}

// TestRagApiKeyPrefersConfigValue RAG 的 key 仍以配置值为先，
// 环境变量只是它为空时的兜底——与改动前一致。
func TestRagApiKeyPrefersConfigValue(t *testing.T) {
	t.Setenv("ALIYUN_API_KEY", "sk-from-env")
	c := baseConfig()
	if k := c.RagApiKey(); k != "sk-aliyun" {
		t.Errorf("配置值应优先于环境变量，得到 %q", k)
	}

	c.RagModelConfig.RagApiKey = ""
	if k := c.RagApiKey(); k != "sk-from-env" {
		t.Errorf("配置为空时应回落环境变量，得到 %q", k)
	}
}
