package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	agentmemory "deeptalk/internal/agent/memory"
	llmpkg "deeptalk/internal/llm"

	"github.com/cloudwego/eino/schema"
)

func TestMain(m *testing.M) {
	if _, file, _, ok := runtime.Caller(0); ok {
		root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
		_ = os.Setenv("DEEPTALK_CONFIG", filepath.Join(root, "config", "config.toml.example"))
	}
	os.Exit(m.Run())
}

type stubModel struct{}

func (s *stubModel) GenerateResponse(context.Context, []*schema.Message) (*schema.Message, error) {
	return &schema.Message{Role: schema.Assistant, Content: "ok"}, nil
}
func (s *stubModel) StreamResponse(_ context.Context, _ []*schema.Message, cb llmpkg.StreamCallback) (string, *schema.TokenUsage, error) {
	cb("ok")
	return "ok", nil, nil
}
func (s *stubModel) GetModelType() string { return llmpkg.ModelTypeDeepSeek }
func (s *stubModel) GetModelName() string { return "stub-model" }

func newTestModel(t *testing.T) llmpkg.AIModel {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"摘要内容"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("DEEPSEEK_BASE_URL", server.URL)
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	t.Setenv("DEEPSEEK_MODEL_NAME", "test-model")
	modelAdapter, err := llmpkg.NewOpenAIModel(context.Background())
	if err != nil {
		t.Fatalf("NewOpenAIModel: %v", err)
	}
	return modelAdapter
}

func TestAddMessageConcurrentSafe(t *testing.T) {
	helper := NewAIHelper(&stubModel{}, "session-1")
	const goroutines, per = 50, 20
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				helper.AddMessage("msg", "u", true, false)
			}
		}()
	}
	wg.Wait()
	if got := len(helper.GetMessages()); got != goroutines*per {
		t.Fatalf("concurrent AddMessage lost updates: %d", got)
	}
}

func TestCompressKeepsSummaryAsSystem(t *testing.T) {
	oldLimit := compressTokenLimit
	compressTokenLimit = func() int { return 1000 }
	t.Cleanup(func() { compressTokenLimit = oldLimit })

	helper := NewAIHelper(newTestModel(t), "session-2")
	longText := strings.Repeat("历", 1000)
	for i := 0; i < 10; i++ {
		helper.AddMessage(longText, "u", i%2 == 0, false)
	}
	if !agentmemory.NewCompressor(compressTokenLimit()).ShouldCompress(helper.GetMessages()) {
		t.Fatal("history should exceed token limit")
	}
	helper.compressIfNeeded(context.Background())
	if got := len(helper.GetMessages()); got != 6 {
		t.Fatalf("compressed history = %d, want 6", got)
	}
	var summaries int
	for _, message := range helper.schemaMessages() {
		if message.Role == schema.System && strings.Contains(message.Content, "摘要内容") {
			summaries++
		}
	}
	if summaries != 1 {
		t.Fatalf("summary system messages = %d, want 1", summaries)
	}
}

func TestMessagesKeepRole(t *testing.T) {
	helper := NewAIHelper(&stubModel{}, "session-3")
	helper.AddMessage("问题", "u", true, false)
	helper.AddMessage("回答", "u", false, false)
	messages := helper.GetMessages()
	if len(messages) != 2 || !messages[0].IsUser || messages[1].IsUser {
		t.Fatalf("role not preserved: %+v", messages)
	}
}
