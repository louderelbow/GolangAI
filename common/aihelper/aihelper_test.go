package aihelper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"deeptalk/model"

	"github.com/cloudwego/eino/schema"
)

// stubModel 用于隔离网络的最小 AIModel 实现
type stubModel struct{}

func (s *stubModel) GenerateResponse(ctx context.Context, m []*schema.Message) (*schema.Message, error) {
	return &schema.Message{Role: schema.Assistant, Content: "ok"}, nil
}

func (s *stubModel) StreamResponse(ctx context.Context, m []*schema.Message, cb StreamCallback) (string, error) {
	return "ok", nil
}

func (s *stubModel) GetModelType() string { return ModelTypeDeepSeek }

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

	// 压缩摘要存的是 model.Message，不参与角色推断
	_ = model.Message{}
}
