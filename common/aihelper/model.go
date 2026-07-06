package aihelper

import (
	"context"
	"deeptalk/common/rag"
	"deeptalk/config"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/ollama"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

type StreamCallback func(msg string)

// AIModel 定义AI模型接口
type AIModel interface {
	GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error)
	StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, error)
	GetModelType() string
}

// =================== DeepSeek 实现（兼容 OpenAI 协议）===================
type OpenAIModel struct {
	llm model.ToolCallingChatModel
}

func NewOpenAIModel(ctx context.Context) (*OpenAIModel, error) {
	baseURL := os.Getenv("DEEPSEEK_BASE_URL")
	if baseURL == "" {
		baseURL = os.Getenv("OPENAI_BASE_URL")
	}
	if baseURL == "" {
		baseURL = "https://api.deepseek.com"
	}

	modelName := os.Getenv("DEEPSEEK_MODEL_NAME")
	if modelName == "" {
		modelName = os.Getenv("OPENAI_MODEL_NAME")
	}
	if modelName == "" {
		modelName = "deepseek-chat"
	}

	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}

	llm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: baseURL,
		Model:   modelName,
		APIKey:  key,
	})
	if err != nil {
		return nil, fmt.Errorf("create deepseek model failed: %v", err)
	}
	return &OpenAIModel{llm: llm}, nil
}

func (o *OpenAIModel) GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	resp, err := o.llm.Generate(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("deepseek generate failed: %v", err)
	}
	return resp, nil
}

func (o *OpenAIModel) StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, error) {
	stream, err := o.llm.Stream(ctx, messages)
	if err != nil {
		return "", fmt.Errorf("deepseek stream failed: %v", err)
	}
	defer stream.Close()

	var fullResp strings.Builder

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("openai stream recv failed: %v", err)
		}
		if len(msg.Content) > 0 {
			fullResp.WriteString(msg.Content) // 聚合

			cb(msg.Content) // 实时调用cb函数，方便主动发送给前端
		}
	}

	return fullResp.String(), nil //返回完整内容，方便后续存储
}

func (o *OpenAIModel) GetModelType() string { return "1" }

// =================== Ollama 实现 ===================

// OllamaModel Ollama模型实现
type OllamaModel struct {
	llm model.ToolCallingChatModel
}

func NewOllamaModel(ctx context.Context, baseURL, modelName string) (*OllamaModel, error) {
	llm, err := ollama.NewChatModel(ctx, &ollama.ChatModelConfig{
		BaseURL: baseURL,
		Model:   modelName,
	})
	if err != nil {
		return nil, fmt.Errorf("create ollama model failed: %v", err)
	}
	return &OllamaModel{llm: llm}, nil
}

func (o *OllamaModel) GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	resp, err := o.llm.Generate(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("ollama generate failed: %v", err)
	}
	return resp, nil
}

func (o *OllamaModel) StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, error) {
	stream, err := o.llm.Stream(ctx, messages)
	if err != nil {
		return "", fmt.Errorf("ollama stream failed: %v", err)
	}
	defer stream.Close()
	var fullResp strings.Builder
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("openai stream recv failed: %v", err)
		}
		if len(msg.Content) > 0 {
			fullResp.WriteString(msg.Content) // 聚合
			cb(msg.Content)                   // 实时调用cb函数，方便主动发送给前端
		}
	}
	return fullResp.String(), nil //返回完整内容，方便后续存储
}

func (o *OllamaModel) GetModelType() string { return "4" }

// =================== RAG 实现 ===================
type AliRAGModel struct {
	llm      model.ToolCallingChatModel
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
		username: username,
	}, nil
}

func (o *AliRAGModel) GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}
	lastMessage := messages[len(messages)-1]
	query := lastMessage.Content

	// 0. 意图分类：总结全文 / 具体问题 / 闲聊
	intent := o.classifyIntent(query)

	// --- 总结全文 ---
	if intent == intentSummary {
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
	if intent == intentChat {
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

func (o *AliRAGModel) StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, error) {
	if len(messages) == 0 {
		return "", fmt.Errorf("no messages provided")
	}
	lastMessage := messages[len(messages)-1]
	query := lastMessage.Content
	intent := o.classifyIntent(query)

	// 总结全文
	if intent == intentSummary {
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
	if intent == intentChat {
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
		return "", fmt.Errorf("rag stream failed: %v", err)
	}
	defer stream.Close()
	var fullResp strings.Builder
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fullResp.String(), err
		}
		if len(msg.Content) > 0 {
			fullResp.WriteString(msg.Content)
			cb(msg.Content)
		}
	}
	return fullResp.String(), nil
}

// streamWithoutRAG 当没有 RAG 文档时的流式响应
func (o *AliRAGModel) streamWithoutRAG(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, error) {
	stream, err := o.llm.Stream(ctx, messages)
	if err != nil {
		return "", fmt.Errorf("ali rag stream failed: %v", err)
	}
	defer stream.Close()

	var fullResp strings.Builder

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("ali rag stream recv failed: %v", err)
		}
		if len(msg.Content) > 0 {
			fullResp.WriteString(msg.Content)
			cb(msg.Content)
		}
	}

	return fullResp.String(), nil
}

func (o *AliRAGModel) GetModelType() string { return "2" }

// extractKeywords 用 LLM 从用户问题中提取关键词
func (o *AliRAGModel) extractKeywords(ctx context.Context, query string) []string {
	// 只对较长问题做关键词提取，短问题自带了关键词
	if len([]rune(query)) < 10 {
		return nil
	}
	prompt := []*schema.Message{
		{Role: schema.System, Content: "你是一个关键词提取助手。从用户问题中提取3-5个最重要的关键词，用逗号分隔。只返回关键词，不要其他内容。\n\n示例：\n用户：怎么申请退货退款？\n关键词：退货,退款,申请流程"},
		{Role: schema.User, Content: query},
	}
	resp, err := o.llm.Generate(ctx, prompt)
	if err != nil {
		return nil
	}
	// 解析逗号分隔的关键词
	parts := strings.Split(resp.Content, ",")
	keywords := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			keywords = append(keywords, p)
		}
	}
	return keywords
}

// intentType 表示用户问题的意图类型
type intentType int

const (
	intentSummary intentType = iota // 总结全文
	intentQuestion                  // 具体问题 → RAG
	intentChat                      // 闲聊 → 普通对话
)

// classifyIntent 用关键词匹配判断用户意图
func (o *AliRAGModel) classifyIntent(query string) intentType {
	q := strings.ToLower(query)
	summaryWords := []string{"总结", "概括", "摘要", "全文", "全部内容", "整篇文档", "整体内容", "大致内容"}
	for _, w := range summaryWords {
		if strings.Contains(q, w) {
			return intentSummary
		}
	}
	// 太短的问题或者纯闲聊 → 不触发 RAG
	chatWords := []string{"你好", "谢谢", "再见", "怎么样", "你是谁", "能做什么", "hello", "hi", "thanks"}
	for _, w := range chatWords {
		if strings.Contains(q, w) && len([]rune(q)) < 15 {
			return intentChat
		}
	}
	return intentQuestion
}

// loadFullDocument 读取用户上传的文档全文
func (o *AliRAGModel) loadFullDocument() (string, error) {
	userDir := fmt.Sprintf("uploads/%s", o.username)
	files, err := os.ReadDir(userDir)
	if err != nil || len(files) == 0 {
		return "", fmt.Errorf("no file found for user %s", o.username)
	}
	var filename string
	for _, f := range files {
		if !f.IsDir() {
			filename = f.Name()
			break
		}
	}
	if filename == "" {
		return "", fmt.Errorf("no valid file")
	}
	data, err := os.ReadFile(filepath.Join(userDir, filename))
	if err != nil {
		return "", err
	}
	text := string(data)
	if len([]rune(text)) > 8000 {
		text = string([]rune(text)[:8000])
	}
	return text, nil
}

// filterByRelevance 按相似度阈值过滤检索结果
func (o *AliRAGModel) filterByRelevance(docs []*schema.Document) []*schema.Document {
	const maxDistance = 0.5
	filtered := make([]*schema.Document, 0, len(docs))
	for _, doc := range docs {
		if dist, ok := doc.MetaData["distance"].(float64); ok && dist < maxDistance {
			filtered = append(filtered, doc)
		} else if doc.MetaData["distance"] == nil {
			filtered = append(filtered, doc)
		}
	}
	if len(filtered) == 0 && len(docs) > 0 {
		return docs[:1]
	}
	return filtered
}

// =================== MCP 实现 ===================

// MCPModel MCP模型实现，集成MCP服务
type MCPModel struct {
	llm        model.ToolCallingChatModel
	mcpClient  *client.Client
	username   string
	mcpBaseURL string
}

// NewMCPModel 创建MCP模型实例
func NewMCPModel(ctx context.Context, username string) (*MCPModel, error) {
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

	// 创建LLM
	llm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: baseURL,
		Model:   modelName,
		APIKey:  key,
	})
	if err != nil {
		return nil, fmt.Errorf("create mcp model failed: %v", err)
	}

	mcpBaseURL := "http://localhost:8081/mcp"

	return &MCPModel{
		llm:        llm,
		mcpBaseURL: mcpBaseURL,
		username:   username,
	}, nil
}

// getMCPClient 获取或创建MCP客户端
func (m *MCPModel) getMCPClient(ctx context.Context) (*client.Client, error) {
	if m.mcpClient == nil {
		// 创建MCP客户端
		httpTransport, err := transport.NewStreamableHTTP(m.mcpBaseURL)
		if err != nil {
			return nil, fmt.Errorf("create mcp transport failed: %v", err)
		}

		m.mcpClient = client.NewClient(httpTransport)

		// 初始化MCP客户端
		initRequest := mcp.InitializeRequest{}
		initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
		initRequest.Params.ClientInfo = mcp.Implementation{
			Name:    "MCP-Go AIHelper Client",
			Version: "1.0.0",
		}
		initRequest.Params.Capabilities = mcp.ClientCapabilities{}

		if _, err := m.mcpClient.Initialize(ctx, initRequest); err != nil {
			return nil, fmt.Errorf("mcp client initialize failed: %v", err)
		}
	}
	return m.mcpClient, nil
}

// GenerateResponse 生成响应，集成MCP工具
func (m *MCPModel) GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}

	// 获取最后一条消息
	lastMessage := messages[len(messages)-1]
	query := lastMessage.Content

	// 第一次调用AI：告诉AI使用固定的JSON格式
	firstPrompt := m.buildFirstPrompt(query)
	firstMessages := make([]*schema.Message, len(messages))
	copy(firstMessages, messages)
	firstMessages[len(firstMessages)-1] = &schema.Message{
		Role:    schema.User,
		Content: firstPrompt,
	}

	// 调用LLM生成第一次响应
	firstResp, err := m.llm.Generate(ctx, firstMessages)
	if err != nil {
		return nil, fmt.Errorf("mcp first generate failed: %v", err)
	}
	log.Println("first resp is ", firstResp)
	// 解析AI响应
	aiResult := firstResp.Content
	toolCall, err := m.parseAIResponse(aiResult)
	if err != nil {
		log.Printf("Failed to parse AI response: %v", err)
		return firstResp, nil
	}

	// 情况1：AI不调用工具，直接返回响应
	if !toolCall.IsToolCall {
		log.Println("toolCall IsToolCall is false ", firstResp)
		return firstResp, nil
	}
	log.Println("toolCall IsToolCall is true ", firstResp)
	// 情况2：AI要调用工具
	// 获取MCP客户端
	mcpClient, err := m.getMCPClient(ctx)
	if err != nil {
		log.Printf("MCP client error: %v", err)
		return &schema.Message{
			Role:    schema.Assistant,
			Content: "抱歉，MCP服务未启动，无法调用工具获取数据。请先启动MCP服务。",
		}, nil
	}

	// 调用MCP工具
	toolResult, err := m.callMCPTool(ctx, mcpClient, toolCall.ToolName, toolCall.Args)
	if err != nil {
		log.Printf("MCP tool call failed: %v", err)
		return &schema.Message{
			Role:    schema.Assistant,
			Content: fmt.Sprintf("抱歉，调用MCP工具 %s 失败：%v", toolCall.ToolName, err),
		}, nil
	}

	// 第二次调用AI：将工具结果告诉AI
	secondPrompt := m.buildSecondPrompt(query, toolCall.ToolName, toolCall.Args, toolResult)
	secondMessages := make([]*schema.Message, len(messages))
	copy(secondMessages, messages)
	secondMessages[len(secondMessages)-1] = &schema.Message{
		Role:    schema.User,
		Content: secondPrompt,
	}

	// 调用LLM生成最终响应
	finalResp, err := m.llm.Generate(ctx, secondMessages)

	if err != nil {
		return nil, fmt.Errorf("mcp second generate failed: %v", err)
	}
	log.Println("最终响应为：", finalResp)
	return finalResp, nil
}

// StreamResponse 流式响应，集成MCP工具
func (m *MCPModel) StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, error) {
	if len(messages) == 0 {
		return "", fmt.Errorf("no messages provided")
	}

	// 获取最后一条消息
	lastMessage := messages[len(messages)-1]
	query := lastMessage.Content

	// 第一次调用AI：告诉AI使用固定的JSON格式
	firstPrompt := m.buildFirstPrompt(query)
	firstMessages := make([]*schema.Message, len(messages))
	copy(firstMessages, messages)
	firstMessages[len(firstMessages)-1] = &schema.Message{
		Role:    schema.User,
		Content: firstPrompt,
	}

	// 第一次调用使用同步接口（非流式）
	firstResp, err := m.llm.Generate(ctx, firstMessages)
	if err != nil {
		return "", fmt.Errorf("mcp first generate failed: %v", err)
	}

	aiResult := firstResp.Content
	toolCall, err := m.parseAIResponse(aiResult)
	if err != nil {
		log.Printf("Failed to parse AI response: %v", err)
		return aiResult, nil
	}

	// 情况1：AI不调用工具，直接返回响应
	if !toolCall.IsToolCall {
		return aiResult, nil
	}

	// 情况2：AI要调用工具
	// 获取MCP客户端
	mcpClient, err := m.getMCPClient(ctx)
	if err != nil {
		log.Printf("MCP client error: %v", err)
		fallbackMsg := "抱歉，MCP服务未启动，无法调用工具获取数据。请先启动MCP服务。"
		cb(fallbackMsg)
		return fallbackMsg, nil
	}

	// 调用MCP工具
	toolResult, err := m.callMCPTool(ctx, mcpClient, toolCall.ToolName, toolCall.Args)
	if err != nil {
		log.Printf("MCP tool call failed: %v", err)
		fallbackMsg := fmt.Sprintf("抱歉，调用MCP工具 %s 失败：%v", toolCall.ToolName, err)
		cb(fallbackMsg)
		return fallbackMsg, nil
	}

	// 第二次调用AI：将工具结果告诉AI，使用流式接口
	secondPrompt := m.buildSecondPrompt(query, toolCall.ToolName, toolCall.Args, toolResult)
	secondMessages := make([]*schema.Message, len(messages))
	copy(secondMessages, messages)
	secondMessages[len(secondMessages)-1] = &schema.Message{
		Role:    schema.User,
		Content: secondPrompt,
	}

	// 调用LLM生成最终响应（流式）
	stream, err := m.llm.Stream(ctx, secondMessages)
	if err != nil {
		return "", fmt.Errorf("mcp second stream failed: %v", err)
	}
	defer stream.Close()

	var finalResp strings.Builder

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("mcp second stream recv failed: %v", err)
		}
		if len(msg.Content) > 0 {
			finalResp.WriteString(msg.Content)
			cb(msg.Content)
		}
	}

	return finalResp.String(), nil
}

// AIToolCall 表示AI工具调用请求
type AIToolCall struct {
	IsToolCall bool                   `json:"isToolCall"`
	ToolName   string                 `json:"toolName"`
	Args       map[string]interface{} `json:"args"`
}

// buildFirstPrompt 构建第一次调用的提示词
func (m *MCPModel) buildFirstPrompt(query string) string {
	return fmt.Sprintf(`你是一个智能助手，可以调用MCP工具来获取信息。

可用工具:
- get_weather: 获取指定城市的天气信息，参数: city（城市名称，支持中文和英文，如北京、Shanghai等）

重要规则:
1. 如果需要调用工具，必须严格返回以下JSON格式：
{
  "isToolCall": true,
  "toolName": "工具名称",
  "args": {"参数名": "参数值"}
}
2. 如果不需要调用工具，直接返回自然语言回答
3. 请根据用户问题决定是否需要调用工具

用户问题: %s

请根据需要调用适当的工具，然后给出综合的回答。`, query)
}

// buildSecondPrompt 构建第二次调用的提示词
func (m *MCPModel) buildSecondPrompt(query, toolName string, args map[string]interface{}, toolResult string) string {
	return fmt.Sprintf(`你是一个智能助手，可以调用MCP工具来获取信息。

工具执行结果:
工具名称: %s
工具参数: %v
工具结果: %s

用户问题: %s

请根据工具结果和用户问题，给出最终的综合回答。`, toolName, args, toolResult, query)
}

// parseAIResponse 解析AI响应，检查是否包含工具调用
func (m *MCPModel) parseAIResponse(response string) (*AIToolCall, error) {
	// 尝试解析为JSON
	var toolCall AIToolCall
	if err := json.Unmarshal([]byte(response), &toolCall); err == nil {
		return &toolCall, nil
	}

	// 如果不是JSON，检查是否包含工具调用关键词
	if strings.Contains(response, "get_weather") {
		// 尝试提取城市名称
		city := m.extractCityFromResponse(response)
		if city != "" {
			return &AIToolCall{
				IsToolCall: true,
				ToolName:   "get_weather",
				Args:       map[string]interface{}{"city": city},
			}, nil
		}
	}

	// 不是工具调用
	return &AIToolCall{IsToolCall: false}, nil
}

// callMCPTool 调用MCP工具
func (m *MCPModel) callMCPTool(ctx context.Context, client *client.Client, toolName string, args map[string]interface{}) (string, error) {
	callToolRequest := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: args,
		},
	}

	result, err := client.CallTool(ctx, callToolRequest)
	if err != nil {
		return "", fmt.Errorf("mcp tool call failed: %v", err)
	}

	// 提取工具结果文本
	var text string
	for _, content := range result.Content {
		if textContent, ok := content.(mcp.TextContent); ok {
			text += textContent.Text + "\n"
		}
	}

	return text, nil
}

// extractCityFromResponse 从响应中提取城市名称
// 直接从AI返回的JSON中提取城市，不预留城市列表
func (m *MCPModel) extractCityFromResponse(response string) string {
	// 尝试从JSON中提取城市
	var toolCall AIToolCall
	if err := json.Unmarshal([]byte(response), &toolCall); err == nil {
		if args, ok := toolCall.Args["city"].(string); ok {
			return args
		}
	}

	// 如果JSON解析失败，尝试从文本中提取城市名称
	// 这部分可以根据实际需要扩展，但不再预留固定城市列表
	return ""
}

// GetModelType 获取模型类型
func (m *MCPModel) GetModelType() string { return "3" }

// =================== ReAct Agent 实现（类型 "5"） ===================

type ReActModel struct {
	agent  *react.Agent
	llm    model.ToolCallingChatModel
}

func NewReActModel(ctx context.Context) (*ReActModel, error) {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}

	llm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: "https://api.deepseek.com",
		Model:   "deepseek-chat",
		APIKey:  key,
	})
	if err != nil {
		return nil, fmt.Errorf("create react llm failed: %v", err)
	}

	tools := RegisterAllTools()
	agent, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: llm,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: tools,
		},
		MaxStep: 5,
	})
	if err != nil {
		return nil, fmt.Errorf("create react agent failed: %v", err)
	}

	return &ReActModel{agent: agent, llm: llm}, nil
}

func (r *ReActModel) GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	resp, err := r.agent.Generate(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("react generate failed: %v", err)
	}
	return resp, nil
}

func (r *ReActModel) StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, error) {
	stream, err := r.agent.Stream(ctx, messages)
	if err != nil {
		return "", fmt.Errorf("react stream failed: %v", err)
	}
	defer stream.Close()

	var fullResp strings.Builder
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fullResp.String(), fmt.Errorf("react stream recv failed: %v", err)
		}
		if len(msg.Content) > 0 {
			fullResp.WriteString(msg.Content)
			cb(msg.Content)
		}
	}
	return fullResp.String(), nil
}

func (r *ReActModel) GetModelType() string { return "5" }
