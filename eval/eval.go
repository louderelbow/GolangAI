// Package eval 提供 RAG / 回答质量的离线评测框架。
//
// 用法：
//
//	go run ./cmd/eval -set eval/golden_example.json -user <账号>
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"deeptalk/common/aihelper"
	"deeptalk/common/rag"
	"deeptalk/config"

	"github.com/cloudwego/eino/schema"
)

// Case 一条评测用例
type Case struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	// ExpectPoints 期望答案覆盖的要点：命中率 = 覆盖要点数 / 要点总数
	ExpectPoints []string `json:"expectPoints"`
	// ExpectSources 期望被检索到的关键词（用于算召回率@k）
	ExpectSources []string `json:"expectSources"`
	// ExpectRefusal true 表示文档里没有相关信息，模型应当拒答
	ExpectRefusal bool `json:"expectRefusal"`
	// ModelType 用例使用的模型类型，留空用命令行默认值
	ModelType string `json:"modelType"`
}

// Set 黄金集
type Set struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Cases       []Case  `json:"cases"`
	Thresholds  Metrics `json:"thresholds"` // 可选：低于阈值即失败
}

// Metrics 评测指标
type Metrics struct {
	RetrievalRecall float64 `json:"retrievalRecall"`
	AnswerCoverage  float64 `json:"answerCoverage"`
	RefusalAccuracy float64 `json:"refusalAccuracy"`
	Faithfulness    float64 `json:"faithfulness"`
}

// CaseResult 单条结果
type CaseResult struct {
	ID           string   `json:"id"`
	Question     string   `json:"question"`
	Answer       string   `json:"answer"`
	Docs         int      `json:"docs"`
	DocSnippets  []string `json:"docSnippets,omitempty"`
	RetrievalHit bool     `json:"retrievalHit"`
	Covered      int      `json:"covered"`
	Expected     int      `json:"expected"`
	Refused      bool     `json:"refused"`
	RefusalOK    bool     `json:"refusalOk"`
	Faithfulness float64  `json:"faithfulness"`
	LatencyMS    int64    `json:"latencyMs"`
	Error        string   `json:"error,omitempty"`
}

// Report 评测报告
type Report struct {
	SetName         string       `json:"setName"`
	ModelType       string       `json:"modelType"`
	User            string       `json:"user"`
	Total           int          `json:"total"`
	Failed          int          `json:"failed"`
	Metrics         Metrics      `json:"metrics"`
	AvgLatencyMS    int64        `json:"avgLatencyMs"`
	Results         []CaseResult `json:"results"`
	ThresholdFailed []string     `json:"thresholdFailed,omitempty"`
}

// LoadSet 读取黄金集（JSON，便于人工编写与 diff）
func LoadSet(path string) (*Set, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var set Set
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("parse golden set: %w", err)
	}
	if len(set.Cases) == 0 {
		return nil, fmt.Errorf("golden set %s has no cases", path)
	}
	return &set, nil
}

// Options 运行参数
type Options struct {
	User      string // 用哪个账号的文档做 RAG 检索
	ModelType string // 默认模型类型
	TopK      int    // 检索返回条数（0 用默认）
	Judge     bool   // 是否用 LLM 给"忠实度"打分（会多花 token）
	Verbose   bool
}

var refusalPatterns = []string{
	"无法找到相关信息", "没有相关信息", "未找到相关信息", "无法回答",
	"资料中没有", "文档中没有", "没有提到", "无法提供",
}

// Run 执行评测
func Run(ctx context.Context, set *Set, opts Options) *Report {
	report := &Report{SetName: set.Name, ModelType: opts.ModelType, User: opts.User, Total: len(set.Cases)}

	var recallSum, coverageSum, refusalSum, faithfulSum float64
	var latencySum int64
	var latencyCount int64

	for i := range set.Cases {
		c := set.Cases[i]
		res := runCase(ctx, c, opts)
		report.Results = append(report.Results, res)

		if res.Error != "" {
			report.Failed++
		}
		if set.Cases[i].ExpectSources != nil {
			if res.RetrievalHit {
				recallSum++
			}
		}
		if res.Expected > 0 {
			coverageSum += float64(res.Covered) / float64(res.Expected)
		}
		if res.RefusalOK {
			refusalSum++
		}
		if opts.Judge {
			faithfulSum += res.Faithfulness
		}
		latencySum += res.LatencyMS
		latencyCount++

		if opts.Verbose {
			fmt.Printf("  [%s] docs=%d 覆盖=%d/%d 拒答OK=%v 延迟=%dms %s\n",
				c.ID, res.Docs, res.Covered, res.Expected, res.RefusalOK, res.LatencyMS, res.Error)
		}
	}

	n := float64(len(set.Cases))
	report.Metrics.RetrievalRecall = ratio(recallSum, n)
	report.Metrics.AnswerCoverage = ratio(coverageSum, n)
	report.Metrics.RefusalAccuracy = ratio(refusalSum, n)
	if opts.Judge {
		report.Metrics.Faithfulness = ratio(faithfulSum, n)
	}
	if latencyCount > 0 {
		report.AvgLatencyMS = latencySum / latencyCount
	}

	checkThresholds(report, set.Thresholds)
	return report
}

func ratio(sum, n float64) float64 {
	if n == 0 {
		return 0
	}
	return sum / n
}

// runCase 跑一条用例。
// 注意用「具名返回值」：延迟是在 defer 里写入的，如果直接 `return res`，
// 返回值会先被复制、defer 的赋值就丢了（之前延迟恒为 0 就是这个原因）。
func runCase(ctx context.Context, c Case, opts Options) (res CaseResult) {
	res = CaseResult{ID: c.ID, Question: c.Question, Expected: len(c.ExpectPoints)}
	modelType := c.ModelType
	if modelType == "" {
		modelType = opts.ModelType
	}

	start := time.Now()
	defer func() { res.LatencyMS = time.Since(start).Milliseconds() }()

	// 1) 检索（与线上完全同一条链路：RAG 检索 + 关键词增强 + RRF）
	var docs []*schema.Document
	ragQuery, err := rag.NewRAGQuery(ctx, opts.User)
	if err != nil {
		res.Error = "retrieval unavailable: " + err.Error()
	} else {
		retrieved, rerr := ragQuery.RetrieveDocuments(ctx, c.Question)
		if rerr != nil {
			res.Error = "retrieve failed: " + rerr.Error()
		}
		docs = retrieved
		res.Docs = len(docs)
		for _, d := range docs {
			snippet := []rune(d.Content)
			if len(snippet) > 60 {
				snippet = snippet[:60]
			}
			res.DocSnippets = append(res.DocSnippets, string(snippet))
		}
	}

	// 2) 检索召回：期望关键词是否出现在检索结果里
	if len(c.ExpectSources) > 0 {
		hit := 0
		joined := strings.ToLower(strings.Join(res.DocSnippets, "\n"))
		full := joined
		for _, d := range docs {
			full += "\n" + strings.ToLower(d.Content)
		}
		for _, kw := range c.ExpectSources {
			if strings.Contains(full, strings.ToLower(kw)) {
				hit++
			}
		}
		res.RetrievalHit = hit == len(c.ExpectSources)
	}

	// 3) 生成答案（走与线上一致的模型与提示词）
	model, err := aihelper.GetGlobalFactory().CreateAIModel(ctx, modelType, map[string]interface{}{"username": opts.User})
	if err != nil {
		res.Error = "create model failed: " + err.Error()
		return res
	}

	// RAG / MCP 这两类模型**自己会做检索**（内部就是"检索 + RAG 提示词"），
	// 所以只把原始问题交给它们；否则会把我们拼好的 RAG 提示词再套一层，
	// 变成"拿整个提示词去检索"，既浪费又会污染评测结果。
	var prompt string
	if modelType == aihelper.ModelTypeRAG || modelType == aihelper.ModelTypeMCP {
		prompt = c.Question
	} else {
		prompt = rag.BuildRAGPrompt(c.Question, docs)
	}

	resp, err := model.GenerateResponse(ctx, buildMessages(prompt))
	if err != nil {
		res.Error = "generate failed: " + err.Error()
		return res
	}
	res.Answer = resp.Content

	// 4) 要点覆盖
	lowerAnswer := strings.ToLower(res.Answer)
	for _, p := range c.ExpectPoints {
		if strings.Contains(lowerAnswer, strings.ToLower(p)) {
			res.Covered++
		}
	}

	// 5) 拒答判定
	res.Refused = containsAny(res.Answer, refusalPatterns)
	res.RefusalOK = res.Refused == c.ExpectRefusal

	// 6) 忠实度（可选，LLM 打分）
	if opts.Judge && len(docs) > 0 {
		res.Faithfulness = judgeFaithfulness(ctx, model, docs, res.Answer)
	}

	return res
}

// buildMessages 组装评测用的消息：system 提示 + 提问
func buildMessages(prompt string) []*schema.Message {
	cfg := config.GetConfig()
	msgs := make([]*schema.Message, 0, 3)
	if cfg.AiPromptConfig.SystemPrompt != "" {
		msgs = append(msgs, &schema.Message{Role: schema.System, Content: cfg.AiPromptConfig.SystemPrompt})
	}
	msgs = append(msgs, &schema.Message{Role: schema.System, Content: "当前时间：" + time.Now().Format("2006-01-02 15:04:05")})
	msgs = append(msgs, &schema.Message{Role: schema.User, Content: prompt})
	return msgs
}

func containsAny(text string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}

// judgeFaithfulness 用 LLM 给"是否忠实于参考资料"打分（0~1）
func judgeFaithfulness(ctx context.Context, model aihelper.AIModel, docs []*schema.Document, answer string) float64 {
	var ctxText strings.Builder
	for i, d := range docs {
		ctxText.WriteString(fmt.Sprintf("[文档 %d] %s\n", i+1, d.Content))
	}

	prompt := fmt.Sprintf(`你是评测员。请判断下面的【回答】是否忠实于【参考资料】。
标准：回答中的事实都能在参考资料里找到 → 1 分；有明显编造或与资料矛盾 → 0 分；部分编造 → 0.5 分。
只输出一个数字（0、0.5 或 1），不要任何其他内容。

【参考资料】
%s

【回答】
%s`, ctxText.String(), answer)

	resp, err := model.GenerateResponse(ctx, []*schema.Message{
		{Role: schema.System, Content: "你是一个严格的评测员，只输出分数。"},
		{Role: schema.User, Content: prompt},
	})
	if err != nil {
		return 0
	}

	score := strings.TrimSpace(resp.Content)
	switch {
	case strings.HasPrefix(score, "1"):
		return 1
	case strings.HasPrefix(score, "0.5"):
		return 0.5
	default:
		return 0
	}
}

func checkThresholds(report *Report, th Metrics) {
	if th.RetrievalRecall > 0 && report.Metrics.RetrievalRecall < th.RetrievalRecall {
		report.ThresholdFailed = append(report.ThresholdFailed,
			fmt.Sprintf("检索召回 %.2f < 阈值 %.2f", report.Metrics.RetrievalRecall, th.RetrievalRecall))
	}
	if th.AnswerCoverage > 0 && report.Metrics.AnswerCoverage < th.AnswerCoverage {
		report.ThresholdFailed = append(report.ThresholdFailed,
			fmt.Sprintf("要点覆盖 %.2f < 阈值 %.2f", report.Metrics.AnswerCoverage, th.AnswerCoverage))
	}
	if th.RefusalAccuracy > 0 && report.Metrics.RefusalAccuracy < th.RefusalAccuracy {
		report.ThresholdFailed = append(report.ThresholdFailed,
			fmt.Sprintf("拒答准确率 %.2f < 阈值 %.2f", report.Metrics.RefusalAccuracy, th.RefusalAccuracy))
	}
	if th.Faithfulness > 0 && report.Metrics.Faithfulness < th.Faithfulness {
		report.ThresholdFailed = append(report.ThresholdFailed,
			fmt.Sprintf("忠实度 %.2f < 阈值 %.2f", report.Metrics.Faithfulness, th.Faithfulness))
	}
}

// SaveJSON 输出报告（便于 CI 归档、跨版本对比）
func SaveJSON(report *Report, path string) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
