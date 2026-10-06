// Package aihelper 提供会话级 AI 编排的兼容入口。
package llm

import (
	"deeptalk/internal/llm/llmcore"
	"github.com/cloudwego/eino/schema"
)

type StreamCallback = llmcore.StreamCallback
type AIModel = llmcore.AIModel

func pickUsage(cur *schema.TokenUsage, msg *schema.Message) *schema.TokenUsage {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return cur
	}
	usage := msg.ResponseMeta.Usage
	if cur == nil || usage.TotalTokens >= cur.TotalTokens {
		return usage
	}
	return cur
}
