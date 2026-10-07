package memory

import (
	"context"
	"deeptalk/internal/infra/config"
	"deeptalk/internal/llm/llmcore"
	"deeptalk/model"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
)

// Compressor 对话记忆压缩器
type Compressor struct {
	maxTokens int
}

// NewCompressor 创建压缩器
func NewCompressor(maxTokens int) *Compressor {
	if maxTokens <= 0 {
		maxTokens = 4000
	}
	return &Compressor{maxTokens: maxTokens}
}

// ShouldCompress 检查是否需要压缩
func (c *Compressor) ShouldCompress(messages []*model.Message) bool {
	return c.EstimateTokens(messages) > c.maxTokens
}

// Compress 压缩历史消息：保留最近若干条，更早的用 LLM 生成摘要。
//
// prevSummary 是上一轮已经生成的摘要（可能为空）。它会被**追加**而不是覆盖 ——
// 详见 mergeSummary 的说明。
//
// 这里修的是一个真实存在过的缺陷：原实现"保留最近 6 条"是写死的，
// 压完**不检查结果是否落在预算内**。于是用户粘贴一篇长文档之后，
// 最近 6 条本身就已经超预算，压缩看起来执行了、实际什么都没解决 ——
// 上下文照样爆掉，而且没有任何信号。
func (c *Compressor) Compress(
	ctx context.Context,
	messages []*model.Message,
	modelAdapter llmcore.AIModel,
	prevSummary string,
) ([]*model.Message, string, error) {
	totalTokens := c.EstimateTokens(messages)
	if totalTokens <= c.maxTokens || len(messages) <= 4 {
		return messages, prevSummary, nil
	}

	// 先按条数留最近 6 条，再**按预算回缩**：如果留 6 条仍然超预算，
	// 就继续往前丢，直到落进预算为止。
	keepFrom := len(messages) - 6
	if keepFrom < 0 {
		keepFrom = 0
	}
	recentMsgs := messages[keepFrom:]

	// 摘要本身也要占预算，先从可用额度里扣掉它的估算值。
	budget := c.maxTokens - c.countTokens(prevSummary)
	if budget < c.maxTokens/4 {
		// 旧摘要已经吃掉大半预算：这是"摘要无限增长"的病，
		// 由 mergeSummary 的上限控制，这里只保证不出现负数预算。
		budget = c.maxTokens / 4
	}

	for len(recentMsgs) > 2 && c.EstimateTokens(recentMsgs) > budget {
		recentMsgs = recentMsgs[1:]
	}

	// 连最后两条都超预算时，只能截断内容 —— 这时候的问题是"单条消息太长"，
	// 丢消息解决不了（丢掉就没上下文了），必须裁剪它本身。
	if c.EstimateTokens(recentMsgs) > budget && len(recentMsgs) > 0 {
		recentMsgs = c.trimLongMessages(recentMsgs, budget)
	}

	// 生成被丢弃部分的摘要
	oldMsgs := messages[:len(messages)-len(recentMsgs)]
	summary, err := c.summarize(ctx, oldMsgs, modelAdapter)
	if err != nil {
		log.Printf("[Compressor] summarize failed: %v, skipping compression", err)
		return messages, prevSummary, err
	}

	return recentMsgs, mergeSummary(prevSummary, summary, c.maxTokens/2), nil
}

// EstimateTokens 估算消息列表的 token 数（导出给调用方做上下文构成统计）。
func (c *Compressor) EstimateTokens(messages []*model.Message) int {
	total := 0
	for _, msg := range messages {
		total += c.countTokens(msg.Content)
	}
	return total
}

// CountTokens 估算一段文本的 token 数（导出给指标与工具结果裁剪复用）。
func (c *Compressor) CountTokens(text string) int { return c.countTokens(text) }

// Budget 本轮的 token 预算（供 deeptalk_agent_context_tokens{kind="budget"}）。
func (c *Compressor) Budget() int { return c.maxTokens }

// trimLongMessages 在总量超预算时，从**最长的**那条开始对半砍直到落进预算。
//
// 为什么从最长的开始：一条 3000 token 的工具结果和一条 50 token 的提问，
// 砍后者等于把问题本身删了；砍前者只是少看一段内容。
// 从最长开始砍，用最少的"信息损失次数"换回预算。
func (c *Compressor) trimLongMessages(msgs []*model.Message, budget int) []*model.Message {
	out := make([]*model.Message, len(msgs))
	copy(out, msgs)

	for c.EstimateTokens(out) > budget {
		idx, maxTok := -1, 0
		for i, m := range out {
			if tok := c.countTokens(m.Content); tok > maxTok {
				idx, maxTok = i, tok
			}
		}
		// 没有可砍的（每条都只剩一两个字）就放弃，避免死循环
		if idx < 0 || maxTok <= 1 {
			break
		}

		cp := *out[idx]
		cp.Content = truncateToTokens(cp.Content, maxTok/2)
		out[idx] = &cp
	}
	return out
}

// truncateToTokens 按估算的 token 数截断文本（保留头部，附省略标记）。
//
// 换算与 countTokens **必须一致**，否则会出现"截到目标长度却仍然超预算"
// 这种自相矛盾的结果。所以这里复用同一组系数，而不是另写一套。
func truncateToTokens(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	ascii, nonASCII := 0, 0
	for i, r := range text {
		if r < 128 {
			ascii++
		} else {
			nonASCII++
		}
		if (ascii+3)/4+(nonASCII+1)/2 >= maxTokens {
			return text[:i] + "…（内容过长已截断）"
		}
	}
	return text
}

// mergeSummary 把新摘要**追加**到旧摘要后面，超长时从最旧的段落开始丢。
//
// 为什么不是直接覆盖（原实现）：每压缩一次，上一次的摘要就消失了 ——
// 早期信息被反复丢弃，而"早期"往往包含最稳定的前提（用户是谁、在做什么）。
// 保留多段、从旧到新丢弃，能把信息衰减从"断崖"变成"渐进"。
func mergeSummary(prev, next string, maxRunes int) string {
	next = strings.TrimSpace(next)
	if prev == "" {
		return next
	}
	if next == "" {
		return prev
	}

	merged := prev + "\n\n" + next
	if maxRunes <= 0 || utf8.RuneCountInString(merged) <= maxRunes {
		return merged
	}

	// 按段落从旧到新丢，直到落进上限
	parts := strings.Split(merged, "\n\n")
	for len(parts) > 1 && utf8.RuneCountInString(strings.Join(parts, "\n\n")) > maxRunes {
		parts = parts[1:]
	}
	return strings.Join(parts, "\n\n")
}

// countTokens 粗略估算中文/英文混合文本的 Token 数
func (c *Compressor) countTokens(text string) int {
	asciiCount := 0
	for _, r := range text {
		if r < 128 {
			asciiCount++
		}
	}
	nonAscii := utf8.RuneCountInString(text) - asciiCount
	// 英文字符约 4 chars/token，中文约 2 chars/token，向上取整避免短文本被算成 0
	return (asciiCount+3)/4 + (nonAscii+1)/2
}

// summarize 调用 LLM 生成旧消息的简洁摘要
func (c *Compressor) summarize(ctx context.Context, messages []*model.Message, modelAdapter llmcore.AIModel) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// 先判 nil：调用 nil 接口的方法会 panic，而 panic 发生在压缩路径上时
	// 会把整轮对话打挂 —— 压缩是"锦上添花"的优化，不该有这种杀伤力。
	if modelAdapter == nil {
		return "", fmt.Errorf("no model adapter for summarization")
	}

	// 构造 old messages 文本
	var sb strings.Builder
	for _, msg := range messages {
		role := "用户"
		if !msg.IsUser {
			role = "AI"
		}
		sb.WriteString(fmt.Sprintf("[%s]%s\n", role, msg.Content))
	}

	prompt := []*schema.Message{
		{Role: schema.System, Content: "你是一个对话摘要助手。请用 200 字以内概括以下对话的核心内容，只保留关键信息和结论。"},
		{Role: schema.User, Content: fmt.Sprintf("请概括以下对话：\n%s", sb.String())},
	}

	resp, err := modelAdapter.GenerateResponse(ctx, prompt)
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", fmt.Errorf("empty summary response")
	}
	return resp.Content, nil
}

// GetMaxTokens 获取配置的最大 Token 数
func GetMaxTokens() int {
	cfg := config.GetConfig()
	if cfg.RagModelConfig.MaxContextTokens > 0 {
		return cfg.RagModelConfig.MaxContextTokens
	}
	return 4000
}
