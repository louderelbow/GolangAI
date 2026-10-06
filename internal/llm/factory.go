package llm

import (
	"context"
	"fmt"
	"sync"

	"deeptalk/internal/llm/llmcore"
)

// 模型类型常量（会话创建时确定，创建后不可更改）
const (
	ModelTypeDeepSeek = llmcore.ModelTypeDeepSeek
	ModelTypeRAG      = llmcore.ModelTypeRAG
	ModelTypeUnified  = llmcore.ModelTypeUnified

	// DefaultModelType 历史会话（未记录模型类型）的兜底模型
	DefaultModelType = llmcore.DefaultModelType
)

// ModelCreator 定义模型创建函数类型（需要 context）
type ModelCreator func(ctx context.Context, config map[string]interface{}) (AIModel, error)

// AIModelFactory AI模型工厂
type AIModelFactory struct {
	creators map[string]ModelCreator
}

var (
	globalFactory *AIModelFactory
	factoryOnce   sync.Once
)

// GetGlobalFactory 获取全局单例
func GetGlobalFactory() *AIModelFactory {
	factoryOnce.Do(func() {
		globalFactory = &AIModelFactory{
			creators: make(map[string]ModelCreator),
		}
		globalFactory.registerCreators()
	})
	return globalFactory
}

// 注册模型
func (f *AIModelFactory) registerCreators() {
	f.creators["1"] = func(ctx context.Context, config map[string]interface{}) (AIModel, error) {
		return NewOpenAIModel(ctx)
	}

	// 阿里百炼 RAG 模型
	f.creators["2"] = func(ctx context.Context, config map[string]interface{}) (AIModel, error) {
		username, ok := config["username"].(string)
		if !ok {
			return nil, fmt.Errorf("RAG model requires username")
		}
		return NewAliRAGModel(ctx, username)
	}

	// Unified Agent 融合原 MCP 与 ReAct 路径，后续检索工具也从这里接入。
	f.creators[ModelTypeUnified] = func(ctx context.Context, config map[string]interface{}) (AIModel, error) {
		username, ok := config["username"].(string)
		if !ok {
			return nil, fmt.Errorf("unified agent requires username")
		}
		return NewUnifiedModel(ctx, username)
	}

}

// CreateAIModel 根据类型创建 AI 模型
func (f *AIModelFactory) CreateAIModel(ctx context.Context, modelType string, config map[string]interface{}) (AIModel, error) {
	creator, ok := f.creators[modelType]
	if !ok {
		return nil, fmt.Errorf("unsupported model type: %s", modelType)
	}
	return creator(ctx, config)
}

// HasModelType 判断是否为已注册的模型类型
func (f *AIModelFactory) HasModelType(modelType string) bool {
	_, ok := f.creators[modelType]
	return ok
}

// IsValidModelType 全局校验模型类型（供 service 层做参数校验）
func IsValidModelType(modelType string) bool {
	return GetGlobalFactory().HasModelType(modelType)
}

// NormalizeModelType 将历史 3/4/5 会话映射到当前执行路径。
func NormalizeModelType(modelType string) (string, bool) {
	return llmcore.NormalizeModelType(modelType)
}
