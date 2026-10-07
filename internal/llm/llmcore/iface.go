// Package llmcore 定义模型接入层与上层协作所需的最小稳定接口。
package llmcore

import (
	"context"

	"github.com/cloudwego/eino/schema"
)

const (
	ModelTypeRAG     = "2"
	ModelTypeUnified = "6"
	ModelTypeMulti   = "7"

	DefaultModelType = ModelTypeRAG
)

// StreamCallback 接收模型流式输出的文本增量。
type StreamCallback func(msg string)

// AIModel 是模型接入层对业务层暴露的稳定边界。
type AIModel interface {
	GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error)
	StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, *schema.TokenUsage, error)
	GetModelType() string
	GetModelName() string
}

// NormalizeModelType 把历史会话类型映射到仍受支持的执行路径。
//
// 现状只保留两条路径：2 RAG（确定性检索，必然查文档）与 6 Unified Agent
// （自主决定用不用工具）。
//
// "1"（原 DeepSeek 纯对话）已删除。历史会话映射到 6 而不是 2：
// 它当时就是"不带检索的通用对话"，映射到 RAG 会让一个从来不查文档的会话
// 突然开始按文档回答，答案性质变了。Agent 更接近它原来的行为。
func NormalizeModelType(modelType string) (string, bool) {
	switch modelType {
	case "", ModelTypeRAG:
		return ModelTypeRAG, true
	case "1", "3", "5", ModelTypeUnified:
		return ModelTypeUnified, true
	case "4":
		return ModelTypeRAG, true
	case ModelTypeMulti:
		return ModelTypeMulti, true
	default:
		return "", false
	}
}

// IsCreatableModelType 判断客户端是否可创建该类型的新会话。
func IsCreatableModelType(modelType string) bool {
	return modelType == ModelTypeRAG || modelType == ModelTypeUnified
}
