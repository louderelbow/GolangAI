package aihelper

import (
	"context"
	"fmt"
	"os"
	"sync"
)

// 模型类型常量（会话创建时确定，创建后不可更改）
const (
	ModelTypeDeepSeek = "1"
	ModelTypeRAG      = "2"
	ModelTypeMCP      = "3"
	ModelTypeOllama   = "4"
	ModelTypeReAct    = "5"

	// DefaultModelType 历史会话（未记录模型类型）的兜底模型
	DefaultModelType = ModelTypeRAG
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

	// MCP 模型（集成MCP服务）
	f.creators["3"] = func(ctx context.Context, config map[string]interface{}) (AIModel, error) {
		username, ok := config["username"].(string)
		if !ok {
			return nil, fmt.Errorf("MCP model requires username")
		}
		return NewMCPModel(ctx, username)
	}

	f.creators["4"] = func(ctx context.Context, config map[string]interface{}) (AIModel, error) {
		baseURL, _ := config["baseURL"].(string)
		if baseURL == "" {
			baseURL = os.Getenv("OLLAMA_BASE_URL")
		}
		if baseURL == "" {
			baseURL = "http://localhost:11434"
		}
		modelName, _ := config["modelName"].(string)
		if modelName == "" {
			modelName = os.Getenv("OLLAMA_MODEL_NAME")
		}
		if modelName == "" {
			modelName = "qwen2.5:7b"
		}
		return NewOllamaModel(ctx, baseURL, modelName)
	}

	f.creators["5"] = func(ctx context.Context, config map[string]interface{}) (AIModel, error) {
		return NewReActModel(ctx)
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

// CreateAIHelper 一键创建 AIHelper
func (f *AIModelFactory) CreateAIHelper(ctx context.Context, modelType string, SessionID string, config map[string]interface{}) (*AIHelper, error) {
	model, err := f.CreateAIModel(ctx, modelType, config)
	if err != nil {
		return nil, err
	}
	return NewAIHelper(model, SessionID), nil
}
