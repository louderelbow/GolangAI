package memory

import (
	"strings"
	"testing"

	"deeptalk/model"
)

// TestMergeSummaryKeepsOldSlots 摘要多段追加，而不是单槽覆盖。
//
// 原实现是覆盖：每压缩一次上次的摘要就没了，早期信息被反复丢弃，
// 而"早期"往往包含最稳定的前提（用户是谁、在做什么项目）。
// 保留多段能把信息衰减从"断崖"变成"渐进"。
func TestMergeSummaryKeepsOldSlots(t *testing.T) {
	got := mergeSummary("第一段：用户在做一个 Go 项目", "第二段：讨论过缓存", 1000)
	if !strings.Contains(got, "第一段") || !strings.Contains(got, "第二段") {
		t.Fatalf("两段都该保留，实际 %q", got)
	}
}

// TestMergeSummaryDropsOldestFirst 超长时从**最旧**的一段开始丢。
func TestMergeSummaryDropsOldestFirst(t *testing.T) {
	old := strings.Repeat("旧", 300)
	got := mergeSummary(old, "最新的结论", 100)

	if strings.Contains(got, "旧旧旧") {
		t.Errorf("超长时最旧的段落应当被丢弃，实际 %q", got)
	}
	if !strings.Contains(got, "最新的结论") {
		t.Errorf("最新的摘要必须保留，实际 %q", got)
	}
}

func TestMergeSummaryHandlesEmpty(t *testing.T) {
	if got := mergeSummary("", "新的", 100); got != "新的" {
		t.Errorf("旧摘要为空时应直接返回新的，实际 %q", got)
	}
	if got := mergeSummary("旧的", "", 100); got != "旧的" {
		t.Errorf("新摘要为空时应保留旧的，实际 %q", got)
	}
}

// TestTruncateToTokensRespectsBudget 截断必须真的落进 token 预算。
//
// 换算系数如果和 countTokens 不一致，就会出现"截到目标长度却仍然超预算"
// 这种自相矛盾的结果 —— 所以这条断言是配套保护。
func TestTruncateToTokensRespectsBudget(t *testing.T) {
	c := NewCompressor(1000)
	text := strings.Repeat("文", 5000) + strings.Repeat("a", 5000)

	got := truncateToTokens(text, 100)
	if n := c.countTokens(got); n > 120 { // 留一点余量给省略标记
		t.Fatalf("截断后仍有 %d token，目标是 100", n)
	}
	if !strings.Contains(got, "已截断") {
		t.Error("应当附上截断标记，让模型知道内容不完整")
	}
}

// TestTrimLongMessagesCutsLongestFirst 超预算时先砍最长的那条。
//
// 一条 3000 token 的工具结果和一条 50 token 的提问，砍后者等于把问题删了。
// 从最长开始砍，用最少的"信息损失次数"换回预算。
func TestTrimLongMessagesCutsLongestFirst(t *testing.T) {
	c := NewCompressor(200)
	msgs := []*model.Message{
		{Content: "短问题", IsUser: true},
		{Content: strings.Repeat("长", 2000), IsUser: false},
	}

	got := c.trimLongMessages(msgs, 100)

	if got[0].Content != "短问题" {
		t.Errorf("短消息不该被动，实际 %q", got[0].Content)
	}
	if c.countTokens(got[1].Content) >= 2000 {
		t.Error("最长的那条应当被裁剪")
	}
}

// TestCompressTruncatesHugeSingleMessage 单条消息本身就超预算时，必须裁剪它本身。
//
// 这种场景**丢消息解决不了问题**：历史只剩两条时不能再丢，但其中一条是
// 用户粘贴的整篇文档。原实现完全没有处理这条路径 —— 它只按条数留最近 6 条，
// 压完不检查结果大小，于是用户粘一篇长文之后上下文照样爆掉，而且毫无信号。
func TestCompressTruncatesHugeSingleMessage(t *testing.T) {
	c := NewCompressor(500)

	huge := strings.Repeat("文", 20000) // 约 10000 token，远超 500
	msgs := []*model.Message{
		{Content: "短问题一", IsUser: true},
		{Content: "短回答一", IsUser: false},
		{Content: huge, IsUser: true},
		{Content: "短回答二", IsUser: false},
	}

	// 用真实的裁剪路径：直接调 trimLongMessages（Compress 会去调模型生成摘要，
	// 那种集成行为由 assistant_test 覆盖）。
	trimmed := c.trimLongMessages(msgs, 400)

	if got := c.EstimateTokens(trimmed); got > 400 {
		t.Fatalf("裁剪后仍有 %d token，超过预算 400", got)
	}
	// 短消息必须原样保留 —— 砍长的那条，而不是把问题删了
	if trimmed[0].Content != "短问题一" || trimmed[1].Content != "短回答一" {
		t.Errorf("短消息不该被动：%q / %q", trimmed[0].Content, trimmed[1].Content)
	}
	if !strings.Contains(trimmed[2].Content, "已截断") {
		t.Error("最长的那条应当被截断并标注")
	}
}

// TestCompressKeepsHistoryWhenSummaryFails 摘要失败时**不能**破坏历史。
//
// 压缩失败会退回未压缩的历史，表现为"上下文悄悄变长"，
// 而对话本身没有任何异常 —— 所以宁可长，也不能丢。
func TestCompressKeepsHistoryWhenSummaryFails(t *testing.T) {
	c := NewCompressor(100)
	msgs := []*model.Message{
		{Content: strings.Repeat("文", 1000), IsUser: true},
		{Content: "回答", IsUser: false},
		{Content: "追问", IsUser: true},
		{Content: "再回答", IsUser: false},
		{Content: "再追问", IsUser: true},
		{Content: "最后", IsUser: false},
		{Content: "更多", IsUser: true},
	}

	// 传 nil 模型 → summarize 失败
	kept, summary, err := c.Compress(nil, msgs, nil, "")
	if err == nil {
		t.Fatal("没有模型时应当报错")
	}
	if len(kept) != len(msgs) {
		t.Fatalf("摘要失败时不该改动历史：%d != %d", len(kept), len(msgs))
	}
	if summary != "" {
		t.Errorf("失败时不该产出摘要，实际 %q", summary)
	}
}
