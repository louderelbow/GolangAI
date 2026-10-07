package rag

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// fakeRewriteLLM 可控的假模型：能指定回复、报错、延迟。
type fakeRewriteLLM struct {
	reply string
	err   error
	delay time.Duration

	calls  int32
	lastIn []*schema.Message
}

func (f *fakeRewriteLLM) Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastIn = input
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &schema.Message{Role: schema.Assistant, Content: f.reply}, nil
}

func history(contents ...string) []*schema.Message {
	out := make([]*schema.Message, 0, len(contents))
	for i, c := range contents {
		role := schema.User
		if i%2 == 1 {
			role = schema.Assistant
		}
		out = append(out, &schema.Message{Role: role, Content: c})
	}
	return out
}

// ==================== 触发条件 ====================

func TestShouldRewriteTriggersOnPronoun(t *testing.T) {
	cases := []string{"那它呢", "这个怎么算", "上面那条适用于谁", "刚才说的那个呢"}
	for _, q := range cases {
		ok, reason := ShouldRewrite(q, 3)
		if !ok {
			t.Errorf("%q 含指代词，应触发改写（原因=%s）", q, reason)
		}
		if !strings.HasPrefix(reason, "pronoun:") {
			t.Errorf("%q 的触发原因应为 pronoun:，实际 %s", q, reason)
		}
	}
}

func TestShouldRewriteTriggersOnShortQueryWithHistory(t *testing.T) {
	ok, reason := ShouldRewrite("多少钱", 2)
	if !ok {
		t.Fatal("短问题 + 有历史应触发改写")
	}
	if reason != "short_with_history" {
		t.Fatalf("原因应为 short_with_history，实际 %s", reason)
	}
}

func TestShouldRewriteSkipsSelfContainedQuestion(t *testing.T) {
	cases := []struct {
		query   string
		history int
	}{
		{"年假制度是怎么规定的", 3},   // 自包含且够长
		{"公司年假制度是怎么规定的", 0}, // 自包含
		{"多少钱", 0},          // 短，但没历史（首轮无物可消解）
		{"请说明病假与年假的区别", 5},  // 自包含
	}
	for _, c := range cases {
		if ok, reason := ShouldRewrite(c.query, c.history); ok {
			t.Errorf("%q（历史=%d）不该触发改写，实际触发了：%s", c.query, c.history, reason)
		}
	}
}

// TestShouldRewriteShortQueryNeedsHistory "短问题"这一条必须配合历史才成立：
// 没有历史时无物可消解，触发改写纯属浪费一次模型调用。
func TestShouldRewriteShortQueryNeedsHistory(t *testing.T) {
	if ok, _ := ShouldRewrite("多少钱", 0); ok {
		t.Error("无历史时短问题不该触发改写")
	}
	if ok, _ := ShouldRewrite("多少钱", 1); !ok {
		t.Error("有历史时短问题应触发改写")
	}
}

// ==================== 改写成功 ====================

// TestRewriteResolvesPronoun 验收"rewrite 生效"：
// 多轮对话里问"那它呢"，应改写成带实体的自包含语句。
func TestRewriteResolvesPronoun(t *testing.T) {
	llm := &fakeRewriteLLM{reply: "年假多少天"}
	r := NewRewriter(llm)

	msgs := history("公司的年假制度", "年假按工龄计算…", "那它呢")
	res := r.Rewrite(context.Background(), "那它呢", msgs)

	if !res.Rewritten {
		t.Fatalf("应改写成实体化语句，实际未改写（原因=%s）", res.Reason)
	}
	if res.Query != "年假多少天" {
		t.Fatalf("改写结果不对: %q", res.Query)
	}
	if res.Reason != RewriteOK {
		t.Fatalf("原因应为 %s，实际 %s", RewriteOK, res.Reason)
	}
	if atomic.LoadInt32(&llm.calls) != 1 {
		t.Fatalf("应只调用一次模型，实际 %d 次", llm.calls)
	}
}

// TestRewritePromptCarriesHistoryAndQuery 提示词必须带上文与当前提问，
// 否则模型无从知道"它"指什么。
func TestRewritePromptCarriesHistoryAndQuery(t *testing.T) {
	llm := &fakeRewriteLLM{reply: "x"}
	r := NewRewriter(llm)
	r.Rewrite(context.Background(), "那它呢", history("公司的年假制度", "按工龄算", "那它呢"))

	if len(llm.lastIn) != 1 {
		t.Fatalf("应只发一条消息，实际 %d 条", len(llm.lastIn))
	}
	prompt := llm.lastIn[0].Content
	for _, want := range []string{"公司的年假制度", "按工龄算", "那它呢", "改写"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("提示词缺少 %q\n---\n%s", want, prompt)
		}
	}
	// 当前提问必须排在历史之后
	if strings.Index(prompt, "那它呢") < strings.Index(prompt, "公司的年假制度") {
		t.Error("当前提问应排在历史之后")
	}
}

// ==================== 降级 ====================

// TestRewriteFallsBackOnTimeout 验收"rewrite 失败降级"：
// 超时后必须回退原句，且不把错误抛给调用方。
func TestRewriteFallsBackOnTimeout(t *testing.T) {
	llm := &fakeRewriteLLM{reply: "不该用到的结果", delay: 300 * time.Millisecond}
	r := NewRewriter(llm)
	r.timeout = 30 * time.Millisecond // 压短超时，避免测试变慢

	start := time.Now()
	res := r.Rewrite(context.Background(), "那它呢", history("年假制度", "答案", "那它呢"))
	elapsed := time.Since(start)

	if res.Rewritten {
		t.Fatal("超时不应算改写成功")
	}
	if res.Query != "那它呢" {
		t.Fatalf("超时应回退原句，实际 %q", res.Query)
	}
	if res.Reason != RewriteFallbackTime {
		t.Fatalf("原因应为 %s，实际 %s", RewriteFallbackTime, res.Reason)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("超时后应立即返回，实际耗时 %s", elapsed)
	}
}

func TestRewriteFallsBackOnError(t *testing.T) {
	llm := &fakeRewriteLLM{err: errors.New("upstream down")}
	res := NewRewriter(llm).Rewrite(context.Background(), "那它呢", history("a", "b", "那它呢"))

	if res.Rewritten || res.Query != "那它呢" {
		t.Fatalf("调用失败应回退原句，实际 %+v", res)
	}
	if res.Reason != RewriteFallbackError {
		t.Fatalf("原因应为 %s，实际 %s", RewriteFallbackError, res.Reason)
	}
}

func TestRewriteFallsBackOnEmptyOutput(t *testing.T) {
	for _, reply := range []string{"", "   ", "\n", "\"\"", "``"} {
		res := NewRewriter(&fakeRewriteLLM{reply: reply}).Rewrite(
			context.Background(), "那它呢", history("a", "b", "那它呢"))
		if res.Rewritten {
			t.Errorf("模型输出 %q 时不应算改写成功", reply)
		}
		if res.Query != "那它呢" {
			t.Errorf("模型输出 %q 时应回退原句，实际 %q", reply, res.Query)
		}
		if res.Reason != RewriteFallbackEmpty {
			t.Errorf("模型输出 %q 时原因应为 %s，实际 %s", reply, RewriteFallbackEmpty, res.Reason)
		}
	}
}

// TestRewriteWithoutLLMIsNoop 关闭改写时行为必须是"透明"的。
func TestRewriteWithoutLLMIsNoop(t *testing.T) {
	var r *Rewriter
	res := r.Rewrite(context.Background(), "那它呢", nil)
	if res.Rewritten || res.Query != "那它呢" {
		t.Fatalf("未配置改写真应原样返回，实际 %+v", res)
	}
	if res.Reason != RewriteSkipped {
		t.Fatalf("原因应为 %s，实际 %s", RewriteSkipped, res.Reason)
	}
}

// ==================== 输出清洗 ====================

func TestCleanRewriteOutput(t *testing.T) {
	cases := map[string]string{
		"年假多少天":         "年假多少天",
		"  \"年假多少天\"  ": "年假多少天",
		"`年假多少天`":       "年假多少天",
		"年假多少天\n\n解释：因为上面提到了年假": "年假多少天", // 多行只取第一行
		"{\"query\":\"年假多少天\"}": "年假多少天", // 兼容模型吐 JSON
	}
	for in, want := range cases {
		if got := cleanRewriteOutput(&schema.Message{Content: in}); got != want {
			t.Errorf("cleanRewriteOutput(%q) = %q，期望 %q", in, got, want)
		}
	}
	if got := cleanRewriteOutput(nil); got != "" {
		t.Errorf("nil 消息应返回空串，实际 %q", got)
	}
}

// TestRenderHistoryKeepsRecentAndTruncatesLong 历史只保留最近几轮，
// 单条过长要截断——改写只需要知道"在说什么"，不需要全文。
func TestRenderHistoryKeepsRecentAndTruncatesLong(t *testing.T) {
	long := strings.Repeat("很长的历史内容", 100)
	msgs := []*schema.Message{
		{Role: schema.User, Content: "最早的一条"},
		{Role: schema.Assistant, Content: "第二条"},
		{Role: schema.User, Content: "第三条"},
		{Role: schema.Assistant, Content: "第四条"},
		{Role: schema.User, Content: "第五条"},
		{Role: schema.Assistant, Content: long}, // 超长，应被截断
		{Role: schema.User, Content: "最近的"},
		{Role: schema.User, Content: "当前提问"},
	}

	out := renderHistory(msgs)
	if strings.Contains(out, "最早的一条") {
		t.Error("超出最近 6 条的历史应被丢弃")
	}
	if strings.Contains(out, "当前提问") {
		t.Error("当前提问不该出现在历史里（它单独成段）")
	}
	if !strings.Contains(out, "最近的") {
		t.Error("最近的历史应保留")
	}
	if len([]rune(out)) > 800 {
		t.Errorf("单条过长应被截断，实际渲染出 %d 字", len([]rune(out)))
	}
}

func TestRenderHistorySingleMessageIsEmpty(t *testing.T) {
	if got := renderHistory([]*schema.Message{{Role: schema.User, Content: "只有当前提问"}}); got != "" {
		t.Fatalf("只有一条时历史应为空，实际 %q", got)
	}
}
