package llm

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"deeptalk/internal/decision"
	"deeptalk/internal/infra/config"
	"deeptalk/internal/rag"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// =================== RAG 实现 ===================
type AliRAGModel struct {
	llm      model.ToolCallingChatModel
	name     string
	username string // 用于获取用户的文档
}

func NewAliRAGModel(ctx context.Context, username string) (*AliRAGModel, error) {
	conf := config.GetConfig()
	key := conf.RagModelConfig.RagApiKey
	if key == "" {
		key = os.Getenv("ALIYUN_API_KEY")
	}
	if key == "" {
		key = os.Getenv("DEEPSEEK_API_KEY")
	}
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	modelName := conf.RagModelConfig.RagChatModelName
	baseURL := conf.RagModelConfig.RagBaseUrl

	llm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: baseURL,
		Model:   modelName,
		APIKey:  key,
	})
	if err != nil {
		return nil, fmt.Errorf("create ali rag model failed: %v", err)
	}
	return &AliRAGModel{
		llm:      llm,
		name:     modelName,
		username: username,
	}, nil
}

func (o *AliRAGModel) GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}
	lastMessage := messages[len(messages)-1]
	query := lastMessage.Content

	// 分层意图识别：规则层高置信度直接用，模糊时才上抛 LLM 兜底
	intent := decision.ClassifyIntent(ctx, query, o.llm)

	// --- 总结全文 ---
	if intent.Intent == decision.IntentSummary {
		fullText, err := o.loadFullDocument()
		if err != nil {
			log.Printf("[RAG] loadFullDocument failed: %v, fallback to normal", err)
		} else {
			log.Printf("[RAG] summary intent detected, full doc len=%d", len([]rune(fullText)))
			prompt := fmt.Sprintf("请总结以下文档的全部内容，涵盖主要主题和关键信息：\n\n%s", fullText)
			summaryMessages := make([]*schema.Message, len(messages))
			copy(summaryMessages, messages)
			summaryMessages[len(summaryMessages)-1] = &schema.Message{Role: schema.User, Content: prompt}
			resp, err := o.llm.Generate(ctx, summaryMessages)
			if err != nil {
				return nil, fmt.Errorf("rag summary failed: %v", err)
			}
			return resp, nil
		}
	}

	// --- 闲聊 → 跳过 RAG ---
	if intent.Intent == decision.IntentChat {
		log.Printf("[RAG] chat intent, skip RAG")
		return o.llm.Generate(ctx, messages)
	}

	// --- 正常 RAG 管线 ---
	ragQuery, err := rag.NewRAGQuery(ctx, o.username)
	if err != nil {
		log.Printf("Failed to create RAG query: %v, fallback to normal chat", err)
		return o.llm.Generate(ctx, messages)
	}

	keywords := o.extractKeywords(ctx, query)
	if len(keywords) > 0 {
		log.Printf("[RAG] extracted keywords: %v", keywords)
		query = query + " " + strings.Join(keywords, " ")
	}

	docs, err := ragQuery.RetrieveDocuments(ctx, query)
	if err != nil {
		log.Printf("Failed to retrieve documents: %v", err)
		return o.llm.Generate(ctx, messages)
	}

	// Rerank：相似度过滤
	docs = o.filterByRelevance(docs)
	log.Printf("[RAG] after rerank: %d docs retained", len(docs))

	ragPrompt := rag.BuildRAGPrompt(query, docs)
	ragMessages := make([]*schema.Message, len(messages))
	copy(ragMessages, messages)
	ragMessages[len(ragMessages)-1] = &schema.Message{Role: schema.User, Content: ragPrompt}

	resp, err := o.llm.Generate(ctx, ragMessages)
	if err != nil {
		return nil, fmt.Errorf("ali rag generate failed: %v", err)
	}
	return resp, nil
}

func (o *AliRAGModel) StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, *schema.TokenUsage, error) {
	if len(messages) == 0 {
		return "", nil, fmt.Errorf("no messages provided")
	}
	lastMessage := messages[len(messages)-1]
	query := lastMessage.Content
	intent := decision.ClassifyIntent(ctx, query, o.llm)

	// 总结全文
	if intent.Intent == decision.IntentSummary {
		fullText, err := o.loadFullDocument()
		if err != nil {
			log.Printf("[RAG-Stream] loadFullDocument failed: %v, fallback", err)
		} else {
			log.Printf("[RAG-Stream] summary intent, full doc len=%d", len([]rune(fullText)))
			prompt := fmt.Sprintf("请总结以下文档的全部内容，涵盖主要主题和关键信息：\n\n%s", fullText)
			streamMessages := make([]*schema.Message, len(messages))
			copy(streamMessages, messages)
			streamMessages[len(streamMessages)-1] = &schema.Message{Role: schema.User, Content: prompt}
			return o.streamWithoutRAG(ctx, streamMessages, cb)
		}
	}

	// 闲聊 → 跳过 RAG
	if intent.Intent == decision.IntentChat {
		log.Printf("[RAG-Stream] chat intent, skip RAG")
		return o.streamWithoutRAG(ctx, messages, cb)
	}

	// 正常 RAG
	ragQuery, err := rag.NewRAGQuery(ctx, o.username)
	if err != nil {
		log.Printf("Failed to create RAG query: %v", err)
		return o.streamWithoutRAG(ctx, messages, cb)
	}

	keywords := o.extractKeywords(ctx, query)
	if len(keywords) > 0 {
		log.Printf("[RAG-Stream] extracted keywords: %v", keywords)
		query = query + " " + strings.Join(keywords, " ")
	}

	docs, err := ragQuery.RetrieveDocuments(ctx, query)
	if err != nil {
		log.Printf("Failed to retrieve documents: %v", err)
		return o.streamWithoutRAG(ctx, messages, cb)
	}

	docs = o.filterByRelevance(docs)
	log.Printf("[RAG-Stream] after rerank: %d docs", len(docs))

	ragPrompt := rag.BuildRAGPrompt(query, docs)
	ragMessages := make([]*schema.Message, len(messages))
	copy(ragMessages, messages)
	ragMessages[len(ragMessages)-1] = &schema.Message{Role: schema.User, Content: ragPrompt}

	stream, err := o.llm.Stream(ctx, ragMessages)
	if err != nil {
		return "", nil, fmt.Errorf("rag stream failed: %v", err)
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
			return fullResp.String(), usage, err
		}
		usage = pickUsage(usage, msg)
		if len(msg.Content) > 0 {
			fullResp.WriteString(msg.Content)
			cb(msg.Content)
		}
	}
	return fullResp.String(), usage, nil
}

// streamWithoutRAG 当没有 RAG 文档时的流式响应
func (o *AliRAGModel) streamWithoutRAG(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, *schema.TokenUsage, error) {
	stream, err := o.llm.Stream(ctx, messages)
	if err != nil {
		return "", nil, fmt.Errorf("ali rag stream failed: %v", err)
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
			return "", usage, fmt.Errorf("ali rag stream recv failed: %v", err)
		}
		usage = pickUsage(usage, msg)
		if len(msg.Content) > 0 {
			fullResp.WriteString(msg.Content)
			cb(msg.Content)
		}
	}

	return fullResp.String(), usage, nil
}

func (o *AliRAGModel) GetModelType() string { return ModelTypeRAG }
func (o *AliRAGModel) GetModelName() string { return o.name }
