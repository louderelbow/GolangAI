package metrics

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"deeptalk/internal/infra/config"
)

// setupConfig 用绝对路径指定 config.toml.example，避免依赖测试进程的工作目录
func setupConfig(t *testing.T) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file)))) // internal/infra/metrics/xxx_test.go -> 模块根目录
	example := filepath.Join(root, "config", "config.toml.example")
	if _, err := os.Stat(example); err != nil {
		t.Fatalf("找不到示例配置: %v", err)
	}
	t.Setenv("DEEPTALK_CONFIG", example)
}

// TestPrometheusExposition 验证指标确实由官方 promhttp 渲染成标准格式：
// 直接打一次 /metrics，检查 TYPE 行、带标签的序列、直方图分桶与 gauge。
func TestPrometheusExposition(t *testing.T) {
	Describe("test_counter_total", "测试计数器")
	Count("test_counter_total", Labels{"model": "m1", "status": "ok"}, 3)
	Count("test_counter_total", Labels{"status": "error", "model": "m1"}, 1)
	Observe("test_duration_seconds", Labels{"model": "m1"}, 0.3)
	SetGauge("test_gauge", nil, 7)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	Handler().ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type 应为 Prometheus 文本格式，实际 %q", ct)
	}

	out := rec.Body.String()
	for _, want := range []string{
		"# TYPE test_counter_total counter",
		`test_counter_total{model="m1",status="ok"} 3`,
		`test_counter_total{model="m1",status="error"} 1`,
		"test_duration_seconds_bucket",
		`le="+Inf"`,
		"test_duration_seconds_count",
		"test_gauge 7",
		// 用默认 registry 注册，因此运行时指标应一并暴露
		"go_goroutines",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition 缺少 %q\n---\n%s", want, out)
		}
	}
}

// TestRegisterHelpIdempotent 重复调用不应 panic（启动路径与评测 CLI 都会调用）
func TestRegisterHelpIdempotent(t *testing.T) {
	RegisterHelp()
	RegisterHelp()

	CountAgentBudgetExceeded("steps")
	CountAgentToolRetry("search_docs", true)
	CountAgentDegraded("fallback_model")
	CountAgentTimeout("tool")
	RecordCacheLookup(true)

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	out := rec.Body.String()

	for _, want := range []string{
		MetricAgentBudgetExceeded,
		MetricAgentToolRetry,
		MetricAgentDegraded,
		MetricAgentTimeout,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics 缺少 %s\n---\n%s", want, out)
		}
	}
}

// 新增的配置段必须能被正确解析（写错 toml 会让线上悄悄用默认值）
func TestConfigSectionsParse(t *testing.T) {
	setupConfig(t)
	cfg := config.GetConfig()

	if cfg.AiPromptConfig.SystemPrompt == "" {
		t.Error("aiPrompt.systemPrompt 未解析")
	}
	if got := cfg.SemanticCache.Threshold; got != 0.92 {
		t.Errorf("semanticCache.threshold = %v, want 0.92", got)
	}
	if !cfg.SemanticCache.Enabled {
		t.Error("semanticCache.enabled 未解析")
	}
	if got := cfg.AiPricingConfig.PromptPrice["deepseek-chat"]; got != 2.0 {
		t.Errorf("aiPricing.promptPrice.deepseek-chat = %v, want 2.0", got)
	}
	if len(cfg.McpConfig.Servers) == 0 {
		t.Fatal("mcpConfig.servers 未解析")
	}
	if cfg.McpConfig.Servers[0].Name != "weather" || len(cfg.McpConfig.Servers[0].AllowedTools) != 1 {
		t.Errorf("第一个 MCP 服务端解析异常: %+v", cfg.McpConfig.Servers[0])
	}
	if cfg.McpConfig.MaxStep != 5 {
		t.Errorf("mcpConfig.maxStep = %d, want 5", cfg.McpConfig.MaxStep)
	}
}

// 费用换算：1M 输入 + 1M 输出，价格 2/8 元 → 10 元 = 10,000,000 微元
func TestCostMicros(t *testing.T) {
	setupConfig(t)
	config.GetConfig()

	got := CostMicros("deepseek-chat", 1_000_000, 1_000_000, 0)
	if got != 10_000_000 {
		t.Errorf("CostMicros = %d 微元, want 10000000", got)
	}

	// 全部命中前缀缓存：输入按 0.1 系数 → 0.2 元 + 8 元 = 8.2 元
	cached := CostMicros("deepseek-chat", 1_000_000, 1_000_000, 1_000_000)
	if cached >= got {
		t.Errorf("命中缓存后费用应更低: cached=%d full=%d", cached, got)
	}

	// 未配置价格的模型走默认价，不应为负或 panic
	_ = CostMicros("unknown-model", 1000, 500, 0)
}

func TestCheckQuotaUnlimitedByDefault(t *testing.T) {
	setupConfig(t)
	config.GetConfig()

	ok, _, _ := CheckQuota("u1", 12345)
	if !ok {
		t.Error("dailyTokenQuota=0 时应视为不限量")
	}
}
