package llm

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"deeptalk/internal/infra/config"

	"github.com/cloudwego/eino/schema"
)

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

// 意图识别已迁移到 intent.go（分层：规则层 + LLM 兜底，带置信度与评测）

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
