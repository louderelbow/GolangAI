package rag

import (
	"context"
	redisPkg "deeptalk/common/redis"
	"deeptalk/config"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	embeddingArk "github.com/cloudwego/eino-ext/components/embedding/ark"
	redisIndexer "github.com/cloudwego/eino-ext/components/indexer/redis"
	redisRetriever "github.com/cloudwego/eino-ext/components/retriever/redis"
	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"
	redisCli "github.com/redis/go-redis/v9"
)

type RAGIndexer struct {
	embedding embedding.Embedder
	indexer   *redisIndexer.Indexer
}

type RAGQuery struct {
	embedding embedding.Embedder
	retriever retriever.Retriever
	filename  string
	indexName string
	rdb       *redisCli.Client
}

// 构建知识库索引
// 专业说法：文本解析、文本切块、向量化、存储向量
// 通俗理解：把“人能读的文档”，转换成“AI 能按语义搜索的格式”，并存起来
func NewRAGIndexer(filename, embeddingModel string) (*RAGIndexer, error) {

	// 用于控制整个初始化流程（超时 / 取消等），这里先用默认背景即可
	ctx := context.Background()

	// 从配置读取 Embedding API Key
	conf := config.GetConfig()
	apiKey := conf.RagModelConfig.RagApiKey
	if apiKey == "" {
		apiKey = os.Getenv("ALIYUN_API_KEY")
	}
	if apiKey == "" {
		apiKey = os.Getenv("DEEPSEEK_API_KEY")
	}

	// 向量的维度大小（等于向量模型输出的数字个数）
	// Redis 在创建向量索引时必须提前知道这个值
	dimension := conf.RagModelConfig.RagDimension

	// 1. 配置并创建”向量生成器”（Embedding）
	embedConfig := &embeddingArk.EmbeddingConfig{
		BaseURL: conf.RagModelConfig.RagBaseUrl, // 向量模型服务地址
		APIKey:  apiKey,                         // 鉴权信息
		Model:   embeddingModel,                 // 使用哪个向量模型
	}

	// 创建向量生成器实例
	// 后续所有文本的“向量化”都会通过它完成
	embedder, err := embeddingArk.NewEmbedder(ctx, embedConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create embedder: %w", err)
	}

	// ===============================
	// 2. 初始化 Redis 中的向量索引结构
	// ===============================
	// 可以理解为：先在 Redis 里建好“仓库”，
	// 告诉它以后要存向量，并且每个向量的维度是多少
	if err := redisPkg.InitRedisIndex(ctx, filename, dimension); err != nil {
		return nil, fmt.Errorf("failed to init redis index: %w", err)
	}

	// 获取 Redis 客户端，用于后续数据写入
	rdb := redisPkg.Rdb

	// ===============================
	// 3. 配置索引器（定义：文档如何被存进 Redis）
	// ===============================
	indexerConfig := &redisIndexer.IndexerConfig{
		Client:    rdb,                                        // Redis 客户端
		KeyPrefix: redisPkg.GenerateIndexNamePrefix(filename), // 不同知识库使用不同前缀，避免冲突
		BatchSize: 10,                                         // 批量处理文档，提高写入效率

		// 定义：一段文档（Document）在 Redis 中该如何存储
		DocumentToHashes: func(ctx context.Context, doc *schema.Document) (*redisIndexer.Hashes, error) {

			// 从文档的元数据中取出来源信息（例如文件名、URL）
			source := ""
			if s, ok := doc.MetaData["source"].(string); ok {
				source = s
			}

			// 构造 Redis 中实际存储的数据结构（Hash）
			return &redisIndexer.Hashes{
				// Redis Key，一般由“知识库名 + 文档块 ID”组成
				Key: fmt.Sprintf("%s:%s", filename, doc.ID),

				// Redis Hash 中的字段
				Field2Value: map[string]redisIndexer.FieldValue{
					// content：原始文本内容
					// EmbedKey 表示：该字段需要先做向量化，
					// 生成的向量会存入名为 "vector" 的字段中
					"content": {Value: doc.Content, EmbedKey: "vector"},

					// metadata：一些辅助信息，不参与向量计算
					"metadata": {Value: source},
				},
			}, nil
		},
	}

	// 将“向量生成器”交给索引器
	// 这样索引器在写入文本时，可以自动完成向量计算
	indexerConfig.Embedding = embedder

	// ===============================
	// 4. 创建最终可用的索引器实例
	// ===============================
	// 此时索引器已经具备：
	// - 文本 → 向量 的能力
	// - 向量写入 Redis 的能力
	idx, err := redisIndexer.NewIndexer(ctx, indexerConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create indexer: %w", err)
	}

	// 返回一个封装好的 RAGIndexer，
	// 后续只需要调用它，就可以把文档加入知识库
	return &RAGIndexer{
		embedding: embedder,
		indexer:   idx,
	}, nil
}

// IndexFile 读取文件内容并创建向量索引
func (r *RAGIndexer) IndexFile(ctx context.Context, filePath string) error {
	// 读取文件内容
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	// 将文件内容转换为文档并进行文本切块
	docs := splitDocument(content, filePath)

	// 使用 indexer 存储文档（会自动进行向量化）
	_, err = r.indexer.Store(ctx, docs)
	if err != nil {
		return fmt.Errorf("failed to store document: %w", err)
	}

	return nil
}

// DeleteIndex 删除指定文件的知识库索引（静态方法，不依赖实例）
func DeleteIndex(ctx context.Context, filename string) error {
	if err := redisPkg.DeleteRedisIndex(ctx, filename); err != nil {
		return fmt.Errorf("failed to delete redis index: %w", err)
	}
	return nil
}

// NewRAGQuery 创建 RAG 查询器（用于向量检索和问答）
func NewRAGQuery(ctx context.Context, username string) (*RAGQuery, error) {
	cfg := config.GetConfig()
	apiKey := cfg.RagModelConfig.RagApiKey
	if apiKey == "" {
		apiKey = os.Getenv("ALIYUN_API_KEY")
	}
	if apiKey == "" {
		apiKey = os.Getenv("DEEPSEEK_API_KEY")
	}

	// 创建 embedding 模型
	embedConfig := &embeddingArk.EmbeddingConfig{
		BaseURL: cfg.RagModelConfig.RagBaseUrl,
		APIKey:  apiKey,
		Model:   cfg.RagModelConfig.RagEmbeddingModel,
	}
	embedder, err := embeddingArk.NewEmbedder(ctx, embedConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create embedder: %w", err)
	}

	// 获取用户上传的文件名（假设每个用户只有一个文件）
	// 这里需要从用户目录读取文件名
	userDir := fmt.Sprintf("uploads/%s", username)
	files, err := os.ReadDir(userDir)
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("no uploaded file found for user %s", username)
	}

	var filename string
	for _, f := range files {
		if !f.IsDir() {
			filename = f.Name()
			break
		}
	}

	if filename == "" {
		return nil, fmt.Errorf("no valid file found for user %s", username)
	}

	// 创建 retriever
	rdb := redisPkg.Rdb
	indexName := redisPkg.GenerateIndexName(filename)

	retrieverConfig := &redisRetriever.RetrieverConfig{
		Client:       rdb,
		Index:        indexName,
		Dialect:      2,
		ReturnFields: []string{"content", "metadata", "distance"},
		TopK:         5,
		VectorField:  "vector",
		DocumentConverter: func(ctx context.Context, doc redisCli.Document) (*schema.Document, error) {
			resp := &schema.Document{
				ID:       doc.ID,
				Content:  "",
				MetaData: map[string]any{},
			}
			for field, val := range doc.Fields {
				if field == "content" {
					resp.Content = val
				} else {
					resp.MetaData[field] = val
				}
			}
			return resp, nil
		},
	}
	retrieverConfig.Embedding = embedder

	rtr, err := redisRetriever.NewRetriever(ctx, retrieverConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create retriever: %w", err)
	}

	return &RAGQuery{
		embedding: embedder,
		retriever: rtr,
		filename:  filename,
		indexName: indexName,
		rdb:       rdb,
	}, nil
}

// RetrieveDocuments 混合检索（向量 + 关键词 + RRF 融合）
func (r *RAGQuery) RetrieveDocuments(ctx context.Context, query string) ([]*schema.Document, error) {
	// 1. 向量检索
	vecDocs, err := r.retriever.Retrieve(ctx, query)
	if err != nil {
		log.Printf("[RAG] vector retrieve failed: %v, fallback to keyword only", err)
		vecDocs = nil
	}
	log.Printf("[RAG] vector retrieve: %d docs", len(vecDocs))

	// 2. 关键词检索
	kwDocs := r.keywordSearch(ctx, query)
	log.Printf("[RAG] keyword retrieve: %d docs", len(kwDocs))

	// 3. RRF 融合排序
	var finalDocs []*schema.Document
	if len(vecDocs) > 0 && len(kwDocs) > 0 {
		finalDocs = rrf(vecDocs, kwDocs, 60)
		log.Printf("[RAG] RRF fused: vector=%d keyword=%d -> final=%d", len(vecDocs), len(kwDocs), len(finalDocs))
	} else if len(vecDocs) > 0 {
		finalDocs = vecDocs
		log.Printf("[RAG] keyword returned 0, using vector only: %d docs", len(finalDocs))
	} else {
		finalDocs = kwDocs
		log.Printf("[RAG] vector returned 0, using keyword only: %d docs", len(finalDocs))
	}

	return finalDocs, nil
}

// keywordSearch 关键词全文检索（RediSearch + Friso 中文分词）
func (r *RAGQuery) keywordSearch(ctx context.Context, query string) []*schema.Document {
	if r.rdb == nil || r.indexName == "" {
		log.Printf("[RAG] keyword search SKIPPED: rdb=%v indexName=%q", r.rdb != nil, r.indexName)
		return nil
	}

	// FT.SEARCH idx query LANGUAGE chinese LIMIT 0 5
	raw, err := r.rdb.Do(ctx, "FT.SEARCH", r.indexName, query, "LANGUAGE", "chinese", "LIMIT", "0", "5").Result()
	if err != nil {
		log.Printf("[RAG] keyword search failed: %v", err)
		return nil
	}

	// 解析 FT.SEARCH 返回：[total, key1, [field1, val1, ...], key2, ...]
	arr, ok := raw.([]interface{})
	if !ok || len(arr) < 2 {
		log.Printf("[RAG] keyword search: 0 results or unexpected format, raw=%v", raw)
		return nil
	}
	total := int(arr[0].(int64))
	log.Printf("[RAG] keyword search hit %d docs for query: %s", total, query)

	docs := make([]*schema.Document, 0)
	for i := 1; i < len(arr); i += 2 {
		key := fmt.Sprintf("%v", arr[i])
		fieldsArr, ok := arr[i+1].([]interface{})
		if !ok {
			continue
		}
		fields := map[string]string{}
		for j := 0; j+1 < len(fieldsArr); j += 2 {
			fields[fmt.Sprintf("%v", fieldsArr[j])] = fmt.Sprintf("%v", fieldsArr[j+1])
		}
		content := ""
		if c, ok := fields["content"]; ok {
			content = c
		}
		meta := map[string]any{}
		for k, v := range fields {
			if k != "content" {
				meta[k] = v
			}
		}
		docs = append(docs, &schema.Document{
			ID:       key,
			Content:  content,
			MetaData: meta,
		})
	}
	return docs
}

// rrf 融合向量和关键词两路排序结果
// score(doc) = Σ 1/(k + rank_i)  k 通常取 60
func rrf(rankA, rankB []*schema.Document, k float64) []*schema.Document {
	scores := map[string]float64{}
	order := map[string]*schema.Document{}

	for i, d := range rankA {
		scores[d.ID] += 1.0 / (k + float64(i+1))
		order[d.ID] = d
	}
	for i, d := range rankB {
		scores[d.ID] += 1.0 / (k + float64(i+1))
		order[d.ID] = d
	}

	type pair struct {
		id    string
		score float64
	}
	list := make([]pair, 0, len(scores))
	for id, s := range scores {
		list = append(list, pair{id, s})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].score > list[j].score
	})

	result := make([]*schema.Document, 0, len(list))
	for _, p := range list {
		if len(result) >= 5 {
			break
		}
		if d, ok := order[p.id]; ok {
			result = append(result, d)
		}
	}
	return result
}

// BuildRAGPrompt 构建包含检索文档的提示词
func BuildRAGPrompt(query string, docs []*schema.Document) string {
	if len(docs) == 0 {
		return query
	}

	contextText := ""
	for i, doc := range docs {
		contextText += fmt.Sprintf("[文档 %d]: %s\n\n", i+1, doc.Content)
	}

	prompt := fmt.Sprintf(`基于以下参考文档回答用户的问题。如果文档中没有相关信息，请说明无法找到相关信息。

参考文档：
%s

用户问题：%s

请提供准确、完整的回答：`, contextText, query)

	return prompt
}

// splitDocument 智能分片：根据文件类型自动选择策略
func splitDocument(content []byte, filePath string) []*schema.Document {
	text := string(content)
	ext := strings.ToLower(filepath.Ext(filePath))

	switch ext {
	case ".md", ".markdown":
		return splitMarkdown(text, filePath)
	default:
		return splitPlainText(text, filePath)
	}
}

// splitMarkdown 按 ## 标题切割 Markdown，保留父标题上下文
func splitMarkdown(text, source string) []*schema.Document {
	const (
		chunkSize    = 500
		chunkOverlap = 50
	)

	// 按 ## 标题拆分章节
	sections := splitByHeaders(text)
	docs := make([]*schema.Document, 0)
	docID := 0

	for _, sec := range sections {
		content := sec.content
		prefix := sec.title
		if prefix != "" {
			prefix = prefix + "\n"
		}

		// 每个章节内部递归切片
		chunks := recursiveSplit(prefix+content, chunkSize, chunkOverlap)
		for _, c := range chunks {
			docs = append(docs, &schema.Document{
				ID:      fmt.Sprintf("doc_%d", docID),
				Content: c,
				MetaData: map[string]any{
					"source": source,
					"title":  sec.title,
				},
			})
			docID++
		}
	}
	return docs
}

// splitPlainText 递归文本分割：逐级尝试更小的分隔符
func splitPlainText(text, source string) []*schema.Document {
	const (
		chunkSize    = 500
		chunkOverlap = 50
	)

	chunks := recursiveSplit(text, chunkSize, chunkOverlap)
	docs := make([]*schema.Document, 0)
	for i, c := range chunks {
		docs = append(docs, &schema.Document{
			ID:      fmt.Sprintf("doc_%d", i),
			Content: c,
			MetaData: map[string]any{
				"source": source,
			},
		})
	}
	return docs
}

// recursiveSplit 递归切分：从大到小尝试分隔符
func recursiveSplit(text string, maxLen, overlap int) []string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		if len(runes) == 0 {
			return nil
		}
		return []string{text}
	}

	// 尝试的分隔符优先级
	separators := []string{"\n\n", "\n", "。", "，", " ", ""}
	for _, sep := range separators {
		if sep == "" {
			// 最后手段：硬切
			return splitBySize(runes, maxLen, overlap)
		}
		parts := strings.Split(text, sep)
		if len(parts) > 1 {
			result := make([]string, 0)
			for _, part := range parts {
				result = append(result, recursiveSplit(part, maxLen, overlap)...)
			}
			// 相邻块加 overlap
			return addOverlap(result, maxLen, overlap)
		}
	}
	return splitBySize(runes, maxLen, overlap)
}

// splitBySize 按 rune 硬切 + overlap
func splitBySize(runes []rune, maxLen, overlap int) []string {
	result := make([]string, 0)
	step := maxLen - overlap
	if step <= 0 {
		step = maxLen
	}
	for i := 0; i < len(runes); i += step {
		end := i + maxLen
		if end > len(runes) {
			end = len(runes)
		}
		result = append(result, string(runes[i:end]))
		if end == len(runes) {
			break
		}
	}
	return result
}

// addOverlap 为所有相邻块添加重叠
func addOverlap(chunks []string, maxLen, overlap int) []string {
	if len(chunks) <= 1 {
		return chunks
	}
	result := make([]string, 0, len(chunks))
	for i, ch := range chunks {
		runes := []rune(ch)
		if i > 0 && len(runes) < maxLen {
			// 从上一块的末尾取 overlap 字符加到本块开头
			prev := []rune(chunks[i-1])
			prevLen := len(prev)
			if prevLen > overlap {
				ch = string(prev[prevLen-overlap:]) + ch
			}
		}
		result = append(result, ch)
	}
	return result
}

// section 表示 Markdown 的一个章节
type section struct {
	title   string
	content string
}

// splitByHeaders 按 Markdown 标题（## 和 ###）拆分
func splitByHeaders(text string) []section {
	lines := strings.Split(text, "\n")
	sections := make([]section, 0)
	var currentTitle string
	var currentLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// 检测 ## 或 ### 标题（排除 # 一级标题，留给文档级别）
		if strings.HasPrefix(trimmed, "### ") || strings.HasPrefix(trimmed, "## ") {
			// 保存上一节
			if len(currentLines) > 0 {
				sections = append(sections, section{
					title:   currentTitle,
					content: strings.Join(currentLines, "\n"),
				})
			}
			currentTitle = trimmed
			currentLines = make([]string, 0)
		} else if strings.HasPrefix(trimmed, "# ") {
			// 一级标题作为文档标题，不作为分节边界
			if currentTitle == "" {
				currentTitle = trimmed
			}
		} else {
			currentLines = append(currentLines, line)
		}
	}
	// 最后一节
	if len(currentLines) > 0 {
		sections = append(sections, section{
			title:   currentTitle,
			content: strings.Join(currentLines, "\n"),
		})
	}
	if len(sections) == 0 {
		sections = append(sections, section{content: text})
	}
	return sections
}
