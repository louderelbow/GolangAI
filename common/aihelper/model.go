package aihelper

import (
	"context"
	"deeptalk/common/rag"
	"deeptalk/config"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/ollama"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
)

type StreamCallback func(msg string)

// AIModel 定义AI模型接口
type AIModel interface {
	GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error)
	// StreamResponse 返回聚合后的完整回答 + token 用量（用于计费/可观测）
	StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, *schema.TokenUsage, error)
	GetModelType() string
	GetModelName() string
}

// pickUsage 从流式分片里提取 token 用量
// OpenAI 兼容协议（eino-ext 已开启 stream_options.include_usage）通常只在最后一个分片带上 usage，
// 因此这里取"信息量最大"的那一份。
func pickUsage(cur *schema.TokenUsage, msg *schema.Message) *schema.TokenUsage {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return cur
	}
	u := msg.ResponseMeta.Usage
	if cur == nil || u.TotalTokens >= cur.TotalTokens {
		return u
	}
	return cur
}

// =================== DeepSeek 实现（兼容 OpenAI 协议）===================
type OpenAIModel struct {
	llm  model.ToolCallingChatModel
	name string
}

// deepSeekSettings 解析大模型（OpenAI 兼容）连接配置
// 来源：环境变量 > 代码默认值
//   DEEPSEEK_BASE_URL / OPENAI_BASE_URL     默认 https://api.deepseek.com
//   DEEPSEEK_MODEL_NAME / OPENAI_MODEL_NAME 默认 deepseek-chat
//   DEEPSEEK_API_KEY / OPENAI_API_KEY       默认空
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
	return &OpenAIModel{llm: llm, name: modelName}, nil
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

// =================== Ollama 实现 ===================

// OllamaModel Ollama模型实现
type OllamaModel struct {
	llm  model.ToolCallingChatModel
	name string
}

func NewOllamaModel(ctx context.Context, baseURL, modelName string) (*OllamaModel, error) {
	llm, err := ollama.NewChatModel(ctx, &ollama.ChatModelConfig{
		BaseURL: baseURL,
		Model:   modelName,
	})
	if err != nil {
		return nil, fmt.Errorf("create ollama model failed: %v", err)
	}
	return &OllamaModel{llm: llm, name: modelName}, nil
}

func (o *OllamaModel) GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	resp, err := o.llm.Generate(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("ollama generate failed: %v", err)
	}
	return resp, nil
}

func (o *OllamaModel) StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, *schema.TokenUsage, error) {
	stream, err := o.llm.Stream(ctx, messages)
	if err != nil {
		return "", nil, fmt.Errorf("ollama stream failed: %v", err)
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
			return "", usage, fmt.Errorf("ollama stream recv failed: %v", err)
		}
		usage = pickUsage(usage, msg)
		if len(msg.Content) > 0 {
			fullResp.WriteString(msg.Content) // 聚合
			cb(msg.Content)                   // 实时调用cb函数，方便主动发送给前端
		}
	}
	return fullResp.String(), usage, nil //返回完整内容，方便后续存储
}

func (o *OllamaModel) GetModelType() string { return ModelTypeOllama }
func (o *OllamaModel) GetModelName() string { return o.name }

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

func (o *AliRAGModel) StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, *schema.TokenUsage, error) {
	if len(messages) == 0 {
		return "", nil, fmt.Errorf("no messages provided")
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
// 阈值与兜底条数都可配置：语料/embedding 模型不同，最优阈值不同。
func (o *AliRAGModel) filterByRelevance(docs []*schema.Document) []*schema.Document {
	cfg := config.GetConfig().RagModelConfig

	maxDistance := cfg.RagMaxDistance
	if maxDistance <= 0 {
		maxDistance = 0.5
	}
	fallbackTopN := cfg.RagFallbackTopN
	if fallbackTopN <= 0 {
		fallbackTopN = 3
	}

	filtered := make([]*schema.Document, 0, len(docs))
	for _, doc := range docs {
		dist, ok := parseDistance(doc.MetaData["distance"])
		if !ok {
			// 没有距离信息时不能当作"不相关"丢掉
			filtered = append(filtered, doc)
			continue
		}
		if dist < maxDistance {
			filtered = append(filtered, doc)
		}
	}

	// 全都没过阈值时，至少保留最相关的 N 条（检索结果本身已按距离升序）
	if len(filtered) == 0 && len(docs) > 0 {
		n := fallbackTopN
		if n > len(docs) {
			n = len(docs)
		}
		log.Printf("[RAG] 所有分片都超过距离阈值 %.2f，兜底保留最相关 %d 条", maxDistance, n)
		return docs[:n]
	}
	return filtered
}

// parseDistance 解析检索结果中的 distance。
// Redis(FT.SEARCH) 返回的字段是字符串，go-redis 的 Document.Fields 也是 map[string]string，
// 因此这里必须兼容 string，不能只断言 float64。
func parseDistance(v any) (float64, bool) {
	switch d := v.(type) {
	case float64:
		return d, true
	case float32:
		return float64(d), true
	case int:
		return float64(d), true
	case int64:
		return float64(d), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(d), 64)
		return f, err == nil
	case []byte:
		f, err := strconv.ParseFloat(strings.TrimSpace(string(d)), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// =================== MCP 实现（原生 function calling） ===================
//
// 工具来源与白名单全部走配置（[mcpConfig.servers]），见 mcp_tools.go：
// 启动时从各 MCP 服务端拉取 tools/list，包装成 eino 工具，交给 Agent 让模型
// 用**协议原生**的 tool_calls 调用 —— 不再依赖"让模型吐 JSON 再解析"。

type MCPModel struct {
	llm   model.ToolCallingChatModel
	agent *react.Agent
	name  string
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

	llm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: baseURL,
		Model:   modelName,
		APIKey:  key,
	})
	if err != nil {
		return nil, fmt.Errorf("create mcp model failed: %v", err)
	}

	m := &MCPModel{llm: llm, name: modelName}

	// 拉取 MCP 工具（注册表内部带缓存与白名单过滤）
	tools := GetMCPRegistry().Tools(ctx)
	if len(tools) == 0 {
		// 没有任何工具时退化为普通对话，而不是直接报错
		log.Printf("[MCP] no tools available, fallback to plain chat (user=%s)", username)
		return m, nil
	}

	maxStep := conf.McpConfig.MaxStep
	if maxStep <= 0 {
		maxStep = 5
	}

	agent, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: llm,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: tools,
		},
		MaxStep: maxStep,
	})
	if err != nil {
		return nil, fmt.Errorf("create mcp agent failed: %v", err)
	}
	m.agent = agent
	log.Printf("[MCP] agent ready: user=%s tools=%d maxStep=%d", username, len(tools), maxStep)
	return m, nil
}

// GenerateResponse 生成响应（有工具时走 Agent，原生 function calling）
func (m *MCPModel) GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}
	if m.agent == nil {
		return m.llm.Generate(ctx, messages)
	}

	resp, err := m.agent.Generate(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("mcp agent generate failed: %v", err)
	}
	return resp, nil
}

// StreamResponse 流式响应（有工具时走 Agent，原生 function calling）
func (m *MCPModel) StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, *schema.TokenUsage, error) {
	if len(messages) == 0 {
		return "", nil, fmt.Errorf("no messages provided")
	}

	if m.agent == nil {
		stream, err := m.llm.Stream(ctx, messages)
		if err != nil {
			return "", nil, fmt.Errorf("mcp stream failed: %v", err)
		}
		defer stream.Close()
		var sb strings.Builder
		var usage *schema.TokenUsage
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return sb.String(), usage, err
			}
			usage = pickUsage(usage, msg)
			if len(msg.Content) > 0 {
				sb.WriteString(msg.Content)
				cb(msg.Content)
			}
		}
		return sb.String(), usage, nil
	}

	stream, err := m.agent.Stream(ctx, messages)
	if err != nil {
		return "", nil, fmt.Errorf("mcp agent stream failed: %v", err)
	}
	defer stream.Close()

	var finalResp strings.Builder
	var usage *schema.TokenUsage

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return finalResp.String(), usage, fmt.Errorf("mcp agent stream recv failed: %v", err)
		}
		usage = pickUsage(usage, msg)
		if len(msg.Content) > 0 {
			finalResp.WriteString(msg.Content)
			cb(msg.Content)
		}
	}

	return finalResp.String(), usage, nil
}

// GetModelType 获取模型类型
func (m *MCPModel) GetModelType() string { return ModelTypeMCP }

// GetModelName 获取模型名
func (m *MCPModel) GetModelName() string { return m.name }

// =================== ReAct Agent 实现（类型 "5"） ===================

type ReActModel struct {
	agent *react.Agent
	llm   model.ToolCallingChatModel
	name  string
}

func NewReActModel(ctx context.Context) (*ReActModel, error) {
	// 与 OpenAIModel 共用同一套配置解析（config.toml > 环境变量 > 默认值）
	baseURL, modelName, key := deepSeekSettings()

	llm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: baseURL,
		Model:   modelName,
		APIKey:  key,
	})
	if err != nil {
		return nil, fmt.Errorf("create react llm failed: %v", err)
	}

	tools := RegisterAllTools()
	maxStep := config.GetConfig().McpConfig.MaxStep
	if maxStep <= 0 {
		maxStep = 5
	}
	agent, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: llm,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: tools,
		},
		MaxStep: maxStep,
	})
	if err != nil {
		return nil, fmt.Errorf("create react agent failed: %v", err)
	}

	return &ReActModel{agent: agent, llm: llm, name: modelName}, nil
}

func (r *ReActModel) GenerateResponse(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	resp, err := r.agent.Generate(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("react generate failed: %v", err)
	}
	return resp, nil
}

func (r *ReActModel) StreamResponse(ctx context.Context, messages []*schema.Message, cb StreamCallback) (string, *schema.TokenUsage, error) {
	stream, err := r.agent.Stream(ctx, messages)
	if err != nil {
		return "", nil, fmt.Errorf("react stream failed: %v", err)
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
			return fullResp.String(), usage, fmt.Errorf("react stream recv failed: %v", err)
		}
		usage = pickUsage(usage, msg)
		if len(msg.Content) > 0 {
			fullResp.WriteString(msg.Content)
			cb(msg.Content)
		}
	}
	return fullResp.String(), usage, nil
}

func (r *ReActModel) GetModelType() string { return ModelTypeReAct }
func (r *ReActModel) GetModelName() string { return r.name }
