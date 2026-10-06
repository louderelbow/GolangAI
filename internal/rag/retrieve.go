package rag

import (
	"context"
	redisPkg "deeptalk/internal/infra/redis"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	redisRetriever "github.com/cloudwego/eino-ext/components/retriever/redis"
	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"
	redisCli "github.com/redis/go-redis/v9"
)

type RAGQuery struct {
	embedding embedding.Embedder
	retriever retriever.Retriever
	filename  string
	indexName string
	rdb       *redisCli.Client
}

// NewRAGQuery 创建（或复用）RAG 查询器。
//
// 按用户名缓存：构建过程有磁盘 IO（读 uploads/<用户名>/ 找文件名）和客户端创建，
// 每个请求重建一次的代价在 CPU profile 里占了 20%+。缓存在用户重新上传文档时由
// InvalidateUser 失效。
func NewRAGQuery(ctx context.Context, username string) (*RAGQuery, error) {
	return cachedRAGQuery(username, func() (*RAGQuery, error) {
		return buildRAGQuery(ctx, username)
	})
}

// buildRAGQuery 真正构建 RAGQuery（缓存未命中时才会走到这里）
func buildRAGQuery(ctx context.Context, username string) (*RAGQuery, error) {
	// 复用进程内共享的 embedding 客户端（建一次要读 volcengine 的 ini 配置）
	embedder, err := SharedEmbedder()
	if err != nil {
		return nil, err
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

	// 2. 关键词检索
	kwDocs := r.keywordSearch(ctx, query)

	// 3. RRF 融合排序
	var finalDocs []*schema.Document
	mode := "rrf"
	switch {
	case len(vecDocs) > 0 && len(kwDocs) > 0:
		finalDocs = rrf(vecDocs, kwDocs, 60)
	case len(vecDocs) > 0:
		finalDocs = vecDocs
		mode = "vector-only"
	default:
		finalDocs = kwDocs
		mode = "keyword-only"
	}

	// 只打一行：这里原本有 4 条日志，26 RPS 下每秒上百行全在同步写控制台，
	// 是压测里日志开销的主要来源之一
	log.Printf("[RAG] retrieve: vector=%d keyword=%d final=%d mode=%s",
		len(vecDocs), len(kwDocs), len(finalDocs), mode)

	return finalDocs, nil
}

// keywordSearch 关键词全文检索（RediSearch + Friso 中文分词）
func (r *RAGQuery) keywordSearch(ctx context.Context, query string) []*schema.Document {
	if r.rdb == nil || r.indexName == "" {
		log.Printf("[RAG] keyword search SKIPPED: rdb=%v indexName=%q", r.rdb != nil, r.indexName)
		return nil
	}

	kwQuery := buildKeywordQuery(query)
	if kwQuery == "" {
		return nil
	}

	// FT.SEARCH idx query LANGUAGE chinese LIMIT 0 5
	raw, err := r.rdb.Do(ctx, "FT.SEARCH", r.indexName, kwQuery, "LANGUAGE", "chinese", "LIMIT", "0", "5").Result()
	if err != nil {
		log.Printf("[RAG] keyword search failed: %v", err)
		return nil
	}

	// 解析 FT.SEARCH 返回：[total, key1, [field1, val1, ...], key2, ...]
	arr, ok := raw.([]interface{})
	if !ok {
		log.Printf("[RAG] keyword search: 返回格式异常, raw=%v", raw)
		return nil
	}
	if len(arr) == 0 {
		return nil
	}
	total := 0
	if t, ok := arr[0].(int64); ok {
		total = int(t)
	}
	if total == 0 || len(arr) < 2 {
		// 没有命中是正常结果，不是错误
		log.Printf("[RAG] keyword search: no hits")
		return nil
	}
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

// buildKeywordQuery 把用户输入转成安全的 RediSearch 查询串
// 策略：按空白/标点/语法字符切词，再用 | 组成 OR 查询；切不出词时返回空串
func buildKeywordQuery(query string) string {
	const maxTerms = 8
	sep := " \t\r\n,，.。;；!！?？:：/\\\"'`()[]{}<>|@#$%^&*+=~-"
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return strings.ContainsRune(sep, r)
	})

	terms := make([]string, 0, maxTerms)
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		terms = append(terms, f)
		if len(terms) >= maxTerms {
			break
		}
	}
	if len(terms) == 0 {
		return ""
	}
	return "(" + strings.Join(terms, "|") + ")"
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
