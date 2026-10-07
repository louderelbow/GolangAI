package chat

import (
	"context"
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
func (s *stubModel) GetModelType() string { return llmpkg.ModelTypeRAG }
func (s *stubModel) GetModelName() string { return "stub-model" }

// summaryStub 专供压缩测试：任何输入都"总结"成一段固定文本。
//
// 以前这里会起一个 httptest server 冒充 DeepSeek 上游，走真实的 OpenAI 适配器
// 转一圈。DeepSeek 直连路径删掉后没必要再绕：被测的是压缩逻辑，不是协议适配，
// 用桩模型既快又不用管网络。
type summaryStub struct{ stubModel }

func (s *summaryStub) GenerateResponse(context.Context, []*schema.Message) (*schema.Message, error) {
	return &schema.Message{Role: schema.Assistant, Content: "摘要内容"}, nil
}

func newTestModel(t *testing.T) llmpkg.AIModel {
	t.Helper()
	return &summaryStub{}
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

// TestCompressKeepsSummaryAsSystem 压缩后：条数下降、摘要以 system 注入一次。
//
// 断言从"必须保留 6 条"改成了"至多 6 条" —— 因为压缩的契约变了：
// 原实现写死保留 6 条且**不检查结果大小**，用户粘一篇长文档时最近 6 条
// 本身就超预算，压缩看起来执行了、实际什么都没解决。
// 现在的契约是"最多 6 条，且必须落进预算"，所以条数是不确定的。
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

	kept := helper.GetMessages()
	if len(kept) > 6 {
		t.Fatalf("压缩后不该保留超过 6 条，实际 %d", len(kept))
	}
	if len(kept) == 0 {
		t.Fatal("不该把历史压到什么都不剩")
	}

	// 真正的契约在这里：压完必须落在预算内。
	// 1000 条长文本 + 预算 1000 token，如果只按条数留 6 条，仍然是超的。
	if got := agentmemory.NewCompressor(compressTokenLimit()).EstimateTokens(kept); got > compressTokenLimit() {
		t.Fatalf("压缩后仍有 %d token，超过预算 %d —— 压缩没有真正起作用", got, compressTokenLimit())
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

// 单条超长裁剪、摘要多段合并等纯函数行为，测在 internal/agent/memory 里
// （那里能直接测未导出函数，不必为测试开导出接口）。

func TestMessagesKeepRole(t *testing.T) {
	helper := NewAIHelper(&stubModel{}, "session-3")
	helper.AddMessage("问题", "u", true, false)
	helper.AddMessage("回答", "u", false, false)
	messages := helper.GetMessages()
	if len(messages) != 2 || !messages[0].IsUser || messages[1].IsUser {
		t.Fatalf("role not preserved: %+v", messages)
	}
}
