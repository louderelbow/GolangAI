package llm

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"deeptalk/internal/inference"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// =================== DeepSeek 实现（兼容 OpenAI 协议）===================
type OpenAIModel struct {
	llm  model.ToolCallingChatModel
	name string
}

func deepSeekSettings() (baseURL, modelName, apiKey string) {
	baseURL = firstNonEmpty(
		os.Getenv("DEEPSEEK_BASE_URL"),
		os.Getenv("OPENAI_BASE_URL"),
		"https://api.deepseek.com",
	)
	modelName = firstNonEmpty(
		os.Getenv("DEEPSEEK_MODEL_NAME"),
		os.Getenv("OPENAI_MODEL_NAME"),
		"deepseek-chat",
	)
	apiKey = firstNonEmpty(
		os.Getenv("DEEPSEEK_API_KEY"),
		os.Getenv("OPENAI_API_KEY"),
	)
	return baseURL, modelName, apiKey
}

func NewOpenAIModel(ctx context.Context) (*OpenAIModel, error) {
	baseURL, modelName, key := deepSeekSettings()

	llm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: baseURL,
		Model:   modelName,
		APIKey:  key,
	})
	if err != nil {
		return nil, fmt.Errorf("create deepseek model failed: %v", err)
	}
	// 与 RAG 路径一致：所有模型调用都要经过推理调度器
	var scheduled model.ToolCallingChatModel = inference.Shared().Wrap(modelName, llm)
	return &OpenAIModel{llm: scheduled, name: modelName}, nil
}

func (o *OpenAIModel) GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	resp, err := o.llm.Generate(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("deepseek generate failed: %v", err)
	}
	return resp, nil
}

func (o *OpenAIModel) StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, *schema.TokenUsage, error) {
	stream, err := o.llm.Stream(ctx, messages)
	if err != nil {
		return "", nil, fmt.Errorf("deepseek stream failed: %v", err)
	}
	defer stream.Close()

	var fullResp strings.Builder
	var usage *schema.TokenUsage

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", usage, fmt.Errorf("openai stream recv failed: %v", err)
		}
		usage = pickUsage(usage, msg)
		if len(msg.Content) > 0 {
			fullResp.WriteString(msg.Content) // 聚合

			cb(msg.Content) // 实时调用cb函数，方便主动发送给前端
		}
	}

	return fullResp.String(), usage, nil //返回完整内容，方便后续存储
}

func (o *OpenAIModel) GetModelType() string { return ModelTypeDeepSeek }
func (o *OpenAIModel) GetModelName() string { return o.name }
