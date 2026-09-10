package aihelper

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"deeptalk/config"
)

// deepSeekSettings 的优先级：config.toml > 环境变量 > 默认值
// 示例配置里 [deepSeekConfig] 是空的，所以这里验证"环境变量 > 默认值"这一段
func TestDeepSeekSettingsPrecedence(t *testing.T) {
	// 确保配置已加载（TestMain 已指定示例配置文件）
	cfg := config.GetConfig()

	// 用例 1：只设 DEEPSEEK_*，应被采用
	t.Setenv("DEEPSEEK_API_KEY", "env-deepseek-key")
	t.Setenv("DEEPSEEK_MODEL_NAME", "env-model")
	t.Setenv("DEEPSEEK_BASE_URL", "http://env.example.com")
	t.Setenv("OPENAI_API_KEY", "env-openai-key")

	cfg.DeepSeekConfig.APIKey = ""
	cfg.DeepSeekConfig.ModelName = ""
	cfg.DeepSeekConfig.BaseURL = ""

	baseURL, modelName, key := deepSeekSettings()
	if key != "env-deepseek-key" || modelName != "env-model" || baseURL != "http://env.example.com" {
		t.Errorf("环境变量未生效: base=%s model=%s key=%s", baseURL, modelName, key)
	}

	// 用例 2：DEEPSEEK_* 缺失时退回 OPENAI_* 与默认值
	os.Unsetenv("DEEPSEEK_API_KEY")
	os.Unsetenv("DEEPSEEK_MODEL_NAME")
	os.Unsetenv("DEEPSEEK_BASE_URL")

	baseURL, modelName, key = deepSeekSettings()
	if key != "env-openai-key" {
		t.Errorf("应退回 OPENAI_API_KEY，实际 %s", key)
	}
	if modelName != "deepseek-chat" {
		t.Errorf("模型名应退回默认 deepseek-chat，实际 %s", modelName)
	}
	if baseURL != "https://api.deepseek.com" {
		t.Errorf("baseURL 应退回默认 api.deepseek.com，实际 %s", baseURL)
	}

	// 用例 3：配置文件里填了就以配置文件为准（优先级最高）
	cfg.DeepSeekConfig.APIKey = "config-key"
	cfg.DeepSeekConfig.ModelName = "config-model"
	cfg.DeepSeekConfig.BaseURL = "http://config.example.com"
	t.Cleanup(func() {
		cfg.DeepSeekConfig.APIKey = ""
		cfg.DeepSeekConfig.ModelName = ""
		cfg.DeepSeekConfig.BaseURL = ""
	})

	baseURL, modelName, key = deepSeekSettings()
	if key != "config-key" || modelName != "config-model" || baseURL != "http://config.example.com" {
		t.Errorf("配置文件优先级最高，实际 base=%s model=%s key=%s", baseURL, modelName, key)
	}
}

// 示例配置里必须留出 [deepSeekConfig] 段，便于使用者集中填写
func TestDeepSeekConfigSectionExists(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	data, err := os.ReadFile(filepath.Join(root, "config", "config.toml.example"))
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	if !containsStr(string(data), "[deepSeekConfig]") {
		t.Error("config.toml.example 缺少 [deepSeekConfig] 段")
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
