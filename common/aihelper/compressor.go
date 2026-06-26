package aihelper

import (
	"deeptalk/config"
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
	return c.estimateTokens(messages) > c.maxTokens
}

// Compress 压缩历史消息：保留最近 N 轮，旧消息用 LLM 生成摘要
func (c *Compressor) Compress(messages []*model.Message, llm AIModel) ([]*model.Message, string, error) {
	totalTokens := c.estimateTokens(messages)
	if totalTokens <= c.maxTokens || len(messages) <= 4 {
		return messages, "", nil
	}

	// 保留最近 3 轮（6 条消息：3 用户 + 3 AI）
	keepFrom := len(messages) - 6
	if keepFrom < 0 {
		keepFrom = 0
	}
	oldMsgs := messages[:keepFrom]
	recentMsgs := messages[keepFrom:]

	// 生成旧消息摘要
	summary, err := c.summarize(oldMsgs, llm)
	if err != nil {
		log.Printf("[Compressor] summarize failed: %v, skipping compression", err)
		return messages, "", nil
	}

	// 构造新的消息列表：system(摘要) + 最近的消息
	newMsgs := make([]*model.Message, 0, len(recentMsgs)+1)
	newMsgs = append(newMsgs, &model.Message{
		Content:  summary,
		IsUser:   false,
		UserName: "system",
	})
	newMsgs = append(newMsgs, recentMsgs...)

	return newMsgs, summary, nil
}

// estimateTokens 估算消息列表的 Token 数
func (c *Compressor) estimateTokens(messages []*model.Message) int {
	total := 0
	for _, msg := range messages {
		total += c.countTokens(msg.Content)
	}
	return total
}

// countTokens 粗略估算中文/英文混合文本的 Token 数
func (c *Compressor) countTokens(text string) int {
	runes := utf8.RuneCountInString(text)
	// 简单策略：英文字符约 1 token/4 chars，中文字符约 1 token/1-2 chars
	asciiCount := 0
	for _, r := range text {
		if r < 128 {
			asciiCount++
		}
	}
	nonAscii := runes - asciiCount
	return asciiCount/4 + nonAscii/2
}

// summarize 调用 LLM 生成旧消息的简洁摘要
func (c *Compressor) summarize(messages []*model.Message, llm AIModel) (string, error) {
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

	resp, err := llm.GenerateResponse(nil, prompt)
	if err != nil {
		return "", err
	}
	return "[对话摘要] " + resp.Content, nil
}

// GetMaxTokens 获取配置的最大 Token 数
func GetMaxTokens() int {
	cfg := config.GetConfig()
	if cfg.RagModelConfig.MaxContextTokens > 0 {
		return cfg.RagModelConfig.MaxContextTokens
	}
	return 4000
}
