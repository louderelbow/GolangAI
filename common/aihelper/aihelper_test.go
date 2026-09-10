package aihelper

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/mark3labs/mcp-go/mcp"
)

// TestMain 指定示例配置文件的绝对路径：
// 让配置相关代码在测试里也能走通（不依赖测试进程的工作目录）
func TestMain(m *testing.M) {
	if _, file, _, ok := runtime.Caller(0); ok {
		root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
		_ = os.Setenv("DEEPTALK_CONFIG", filepath.Join(root, "config", "config.toml.example"))
	}
	os.Exit(m.Run())
}

// ==================== 测试替身 ====================

// stubModel 用于隔离网络的最小 AIModel 实现
type stubModel struct {
	usage *schema.TokenUsage
}

func (s *stubModel) GenerateResponse(ctx context.Context, m []*schema.Message) (*schema.Message, error) {
	msg := &schema.Message{Role: schema.Assistant, Content: "ok"}
	if s.usage != nil {
		msg.ResponseMeta = &schema.ResponseMeta{Usage: s.usage}
	}
	return msg, nil
}

func (s *stubModel) StreamResponse(ctx context.Context, m []*schema.Message, cb StreamCallback) (string, *schema.TokenUsage, error) {
	cb("ok")
	return "ok", s.usage, nil
}

func (s *stubModel) GetModelType() string { return ModelTypeDeepSeek }
func (s *stubModel) GetModelName() string { return "stub-model" }

// newTestOpenAIModel 指向本地 httptest 的 OpenAI 兼容模型（避免真实网络调用）
func newTestOpenAIModel(t *testing.T) AIModel {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"摘要内容"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("DEEPSEEK_BASE_URL", srv.URL)
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	t.Setenv("DEEPSEEK_MODEL_NAME", "test-model")

	m, err := NewOpenAIModel(context.Background())
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	return m
}

// ==================== AIHelper 并发与记忆 ====================

// AddMessage 必须并发安全：并发写入不能丢消息（配合 -race 使用）
func TestAddMessageConcurrentSafe(t *testing.T) {
	h := NewAIHelper(&stubModel{}, "session-1")

	const goroutines, per = 50, 20
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < per; j++ {
				h.AddMessage("msg", "u", true, false)
			}
		}(i)
	}
	// 同时读取，验证读写不会互相破坏
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				_ = h.GetMessages()
			}
		}()
	}
	wg.Wait()

	if got, want := len(h.GetMessages()), goroutines*per; got != want {
		t.Fatalf("concurrent AddMessage lost updates: got %d, want %d", got, want)
	}
}

// 记忆压缩：不能 panic（历史 nil ctx 的回归测试），摘要必须以 system 上下文注入而不是 assistant 消息
func TestCompressDoesNotPanicAndKeepsSummaryAsSystem(t *testing.T) {
	// 固定压缩阈值，避免依赖本地 config.toml
	oldLimit := compressTokenLimit
	compressTokenLimit = func() int { return 1000 }
	t.Cleanup(func() { compressTokenLimit = oldLimit })

	llm := newTestOpenAIModel(t)
	h := NewAIHelper(llm, "session-2")

	long := strings.Repeat("历", 1000) // 约 500 token/条
	for i := 0; i < 10; i++ {
		h.AddMessage(long, "u", i%2 == 0, false)
	}

	// 压缩前的估算 token 必须超过阈值，否则本测试没有覆盖到压缩分支
	if !NewCompressor(compressTokenLimit()).ShouldCompress(h.GetMessages()) {
		t.Fatal("test precondition failed: history should exceed the token limit")
	}

	h.compressIfNeeded(context.Background()) // 历史实现会在这里 panic

	msgs := h.GetMessages()
	if len(msgs) != 6 {
		t.Fatalf("compressed history should keep last 3 rounds (6 messages), got %d", len(msgs))
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "摘要内容") {
			t.Fatalf("summary must not be stored as a normal message, got: %q", m.Content)
		}
	}

	schemaMsgs := h.schemaMessages()
	var summarySystem int
	for _, sm := range schemaMsgs {
		if sm.Role == schema.System && strings.Contains(sm.Content, "摘要内容") {
			summarySystem++
		}
	}
	if summarySystem != 1 {
		t.Fatalf("summary should be injected as exactly one system message, got %d (msgs=%d)", summarySystem, len(schemaMsgs))
	}
}

// 压缩失败（模型报错）时必须降级：保留原历史、不 panic
func TestCompressFailureKeepsHistory(t *testing.T) {
	oldLimit := compressTokenLimit
	compressTokenLimit = func() int { return 1000 }
	t.Cleanup(func() { compressTokenLimit = oldLimit })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("DEEPSEEK_BASE_URL", srv.URL)
	t.Setenv("DEEPSEEK_API_KEY", "test-key")

	llm, err := NewOpenAIModel(context.Background())
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}

	h := NewAIHelper(llm, "session-3")
	long := strings.Repeat("历", 1000)
	for i := 0; i < 10; i++ {
		h.AddMessage(long, "u", true, false)
	}

	h.compressIfNeeded(context.Background())

	if got := len(h.GetMessages()); got != 10 {
		t.Fatalf("compression failure should keep original history, got %d messages", got)
	}
}

// ==================== 模型类型与会话绑定 ====================

// 会话模型绑定：AIHelper 必须记住创建时的模型类型
func TestAIHelperReportsBoundModelType(t *testing.T) {
	h := NewAIHelper(&stubModel{}, "session-4")
	if got := h.GetModelType(); got != ModelTypeDeepSeek {
		t.Fatalf("GetModelType()=%q, want %q", got, ModelTypeDeepSeek)
	}
}

// 模型类型校验：已注册的可通过，未注册的必须拒绝
func TestIsValidModelType(t *testing.T) {
	for _, ok := range []string{ModelTypeDeepSeek, ModelTypeRAG, ModelTypeMCP, ModelTypeOllama, ModelTypeReAct} {
		if !IsValidModelType(ok) {
			t.Errorf("model type %q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "0", "6", "abc"} {
		if IsValidModelType(bad) {
			t.Errorf("model type %q should be invalid", bad)
		}
	}
}

// 会话历史角色不能再靠下标奇偶推断（此处直接验证 model.Message 的 IsUser 被透传）
func TestMessagesKeepIsUserFlag(t *testing.T) {
	h := NewAIHelper(&stubModel{}, "session-5")
	h.AddMessage("问题", "u", true, false)
	h.AddMessage("回答", "u", false, false)

	msgs := h.GetMessages()
	if len(msgs) != 2 || !msgs[0].IsUser || msgs[1].IsUser {
		t.Fatalf("IsUser flag not preserved: %+v", msgs)
	}
}

// ==================== RAG 相关性过滤 ====================

// RAG 相关性过滤必须兼容 Redis 返回的字符串 distance
func TestFilterByRelevanceWithStringDistance(t *testing.T) {
	rag := &AliRAGModel{}
	docs := []*schema.Document{
		{ID: "a", Content: "最相关", MetaData: map[string]any{"distance": "0.10"}},
		{ID: "b", Content: "一般相关", MetaData: map[string]any{"distance": "0.35"}},
		{ID: "c", Content: "不相关", MetaData: map[string]any{"distance": "0.90"}},
		{ID: "d", Content: "没有距离字段", MetaData: map[string]any{}},
	}

	got := rag.filterByRelevance(docs)
	if len(got) != 3 {
		ids := make([]string, 0, len(got))
		for _, d := range got {
			ids = append(ids, d.ID)
		}
		t.Fatalf("string distance not handled, kept=%v, want [a b d]", ids)
	}

	// float64（部分实现会预先解析）同样要支持
	docsFloat := []*schema.Document{
		{ID: "a", MetaData: map[string]any{"distance": 0.1}},
		{ID: "c", MetaData: map[string]any{"distance": 0.9}},
	}
	if got := rag.filterByRelevance(docsFloat); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("float distance filter wrong: %+v", got)
	}
}

// ==================== 语义缓存 ====================

func TestCosine(t *testing.T) {
	a := []float64{1, 0, 0}
	b := []float64{1, 0, 0}
	if got := cosine(a, b); math.Abs(got-1) > 1e-9 {
		t.Errorf("相同向量相似度应为 1，实际 %v", got)
	}

	c := []float64{0, 1, 0}
	if got := cosine(a, c); math.Abs(got) > 1e-9 {
		t.Errorf("正交向量相似度应为 0，实际 %v", got)
	}

	d := []float64{-1, 0, 0}
	if got := cosine(a, d); math.Abs(got+1) > 1e-9 {
		t.Errorf("相反向量相似度应为 -1，实际 %v", got)
	}

	// 维度不一致 / 空向量不能 panic
	if got := cosine(a, []float64{1, 2}); got != -1 {
		t.Errorf("维度不一致应返回 -1，实际 %v", got)
	}
	if got := cosine([]float64{}, []float64{}); got != -1 {
		t.Errorf("空向量应返回 -1，实际 %v", got)
	}
}

// ==================== MCP 工具（原生 function calling） ====================

// mcpToolForTest 构造一个和真实 MCP 天气工具同构的 Tool
func mcpToolForTest() mcp.Tool {
	return mcp.Tool{
		Name:        "get_weather",
		Description: "获取指定城市的天气信息",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"city": map[string]interface{}{
					"type":        "string",
					"description": "城市名称，如 Beijing、上海",
				},
			},
			Required: []string{"city"},
		},
	}
}

// MCP 的 JSON Schema 要能转成 eino 的 ToolInfo（原生 function calling 的基础）
func TestToolInfoFromMCP(t *testing.T) {
	tool := mcpToolForTest()
	info := toolInfoFromMCP("weather__get_weather", tool)

	if info.Name != "weather__get_weather" {
		t.Errorf("name = %s", info.Name)
	}
	if info.Desc == "" {
		t.Error("desc 不应为空")
	}
	params := info.ParamsOneOf
	if params == nil {
		t.Fatal("ParamsOneOf 不应为空")
	}
	got, err := params.ToJSONSchema()
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	if got == nil || got.Properties == nil || got.Properties.Len() == 0 {
		t.Fatalf("参数 schema 未生成: %+v", got)
	}
	if _, ok := got.Properties.Get("city"); !ok {
		t.Errorf("缺少 city 参数")
	}
	if len(got.Required) != 1 || got.Required[0] != "city" {
		t.Errorf("required 解析异常: %v", got.Required)
	}
}

// ==================== 模型连接配置解析 ====================

// deepSeekSettings：环境变量优先，缺失时回落 DEEPSEEK_* → OPENAI_* → 默认值
func TestDeepSeekSettingsFromEnv(t *testing.T) {
	// 用例 1：DEEPSEEK_* 存在时优先采用
	t.Setenv("DEEPSEEK_API_KEY", "env-deepseek-key")
	t.Setenv("DEEPSEEK_MODEL_NAME", "env-model")
	t.Setenv("DEEPSEEK_BASE_URL", "http://env.example.com")
	t.Setenv("OPENAI_API_KEY", "env-openai-key")

	baseURL, modelName, key := deepSeekSettings()
	if key != "env-deepseek-key" || modelName != "env-model" || baseURL != "http://env.example.com" {
		t.Errorf("DEEPSEEK_* 环境变量未生效: base=%s model=%s key=%s", baseURL, modelName, key)
	}

	// 用例 2：缺失时回落 OPENAI_* 与默认值
	os.Unsetenv("DEEPSEEK_API_KEY")
	os.Unsetenv("DEEPSEEK_MODEL_NAME")
	os.Unsetenv("DEEPSEEK_BASE_URL")

	baseURL, modelName, key = deepSeekSettings()
	if key != "env-openai-key" {
		t.Errorf("应回落 OPENAI_API_KEY，实际 %s", key)
	}
	if modelName != "deepseek-chat" {
		t.Errorf("模型名应回落默认 deepseek-chat，实际 %s", modelName)
	}
	if baseURL != "https://api.deepseek.com" {
		t.Errorf("baseURL 应回落默认 api.deepseek.com，实际 %s", baseURL)
	}
}
