package rag

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/schema"
)

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
