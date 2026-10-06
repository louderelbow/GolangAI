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

	"deeptalk/internal/decision"
	"deeptalk/internal/infra/config"
	"deeptalk/internal/llm"
	"deeptalk/internal/rag"

	einomodel "github.com/cloudwego/eino/components/model"

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
	// ExpectIntent 期望的意图分类（summary/question/chat），留空表示不评测意图
	ExpectIntent string `json:"expectIntent,omitempty"`
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
	IntentAccuracy  float64 `json:"intentAccuracy"`
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
	Intent          *IntentEval  `json:"intent,omitempty"`
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
	// IntentOnly 只评测意图识别（规则层，本地零成本），跳过检索与生成
	IntentOnly bool
	// IntentLLM 用真实模型跑"规则层不确定"的那部分（会产生少量模型调用）
	IntentLLM bool
}

var refusalPatterns = []string{
	"无法找到相关信息", "没有相关信息", "未找到相关信息", "无法回答",
	"资料中没有", "文档中没有", "没有提到", "无法提供",
}

// Run 执行评测
func Run(ctx context.Context, set *Set, opts Options) *Report {
	report := &Report{SetName: set.Name, ModelType: opts.ModelType, User: opts.User, Total: len(set.Cases)}

	// 意图评测：纯本地规则层，零成本（不调模型），所以永远先跑
	report.Intent = RunIntentEval(set)
	report.Metrics.IntentAccuracy = report.Intent.Accuracy

	if opts.IntentOnly {
		// 只评测意图：不跑检索/生成，秒级返回、不花一分钱
		return report
	}

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
	model, err := llm.GetGlobalFactory().CreateAIModel(ctx, modelType, map[string]interface{}{"username": opts.User})
	if err != nil {
		res.Error = "create model failed: " + err.Error()
		return res
	}

	// RAG / MCP 这两类模型**自己会做检索**（内部就是"检索 + RAG 提示词"），
	// 所以只把原始问题交给它们；否则会把我们拼好的 RAG 提示词再套一层，
	// 变成"拿整个提示词去检索"，既浪费又会污染评测结果。
	var prompt string
	if modelType == llm.ModelTypeRAG || modelType == llm.ModelTypeUnified {
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
func judgeFaithfulness(ctx context.Context, model llm.AIModel, docs []*schema.Document, answer string) float64 {
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

// ======================== 意图识别评测 ========================
//
// 只跑规则层（纯本地、零成本），因此可以每次改动都跑；
// 统计三件事：整体准确率、每类 P/R/F1、以及"低置信度占比"
// （后者 = 线上需要走 LLM 兜底的流量比例，直接决定兜底的调用成本）

// IntentCaseResult 单条意图判定结果
type IntentCaseResult struct {
	ID        string `json:"id"`
	Question  string `json:"question"`
	Expected  string `json:"expected"`
	Predicted string `json:"predicted"`
	Correct   bool   `json:"correct"`
	Confident bool   `json:"confident"` // false = 线上会走 LLM 兜底
	UsedLLM   bool   `json:"usedLlm,omitempty"`
	Reason    string `json:"reason"`
}

// ClassMetric 单个意图类别的指标
type ClassMetric struct {
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
	Support   int     `json:"support"`
}

// IntentEval 意图评测汇总
type IntentEval struct {
	Total          int                       `json:"total"`
	Correct        int                       `json:"correct"`
	Accuracy       float64                   `json:"accuracy"`
	LegacyCorrect  int                       `json:"legacyCorrect"`
	LegacyAccuracy float64                   `json:"legacyAccuracy"` // 重构前规则在同一份集合上的表现
	LowConf        int                       `json:"lowConfidenceCount"`
	LowConfRate    float64                   `json:"lowConfidenceRate"`
	Confusion      map[string]map[string]int `json:"confusion"`
	PerClass       map[string]ClassMetric    `json:"perClass"`
	Wrong          []IntentCaseResult        `json:"wrong,omitempty"`
	Results        []IntentCaseResult        `json:"results,omitempty"` // 全部用例（用于算兜底后的准确率）
	LLMCorrect     int                       `json:"llmCorrect,omitempty"`
	LLMAccuracy    float64                   `json:"llmAccuracy,omitempty"` // 加入 LLM 兜底后的准确率
}

// ApplyLLMFallback 对"规则层不确定"的用例调用真实模型重判，量化兜底带来的提升
// 只对少量用例产生模型调用（实测约 7%），成本很小
func (e *IntentEval) ApplyLLMFallback(ctx context.Context, model einomodel.ToolCallingChatModel) {
	if e == nil || model == nil {
		return
	}

	e.LLMCorrect = 0
	for i := range e.Results {
		r := &e.Results[i]
		if !r.Confident {
			res := decision.ClassifyIntent(ctx, r.Question, model)
			r.Predicted = string(res.Intent)
			r.Correct = r.Predicted == r.Expected
			r.Reason = "llm兜底: " + res.Reason
			r.UsedLLM = true
		}
		if r.Correct {
			e.LLMCorrect++
		}
	}
	if e.Total > 0 {
		e.LLMAccuracy = float64(e.LLMCorrect) / float64(e.Total)
	}

	// 兜底后重新统计"判错清单"，避免报告里出现"准确率 1.00 但仍列出错例"的矛盾
	e.Wrong = e.Wrong[:0]
	for i := range e.Results {
		if !e.Results[i].Correct {
			e.Wrong = append(e.Wrong, e.Results[i])
		}
	}
}

// legacyRuleIntent 重构前的规则快照（仅用于对比，不参与线上逻辑）
// 特点：纯子串匹配 + 长度阈值，没有整词匹配、没有指代/疑问词守卫、没有置信度与兜底
func legacyRuleIntent(question string) string {
	q := strings.ToLower(question)
	for _, w := range []string{"总结", "概括", "摘要", "全文", "全部内容", "整篇文档", "整体内容", "大致内容"} {
		if strings.Contains(q, w) {
			return "summary"
		}
	}
	for _, w := range []string{"你好", "谢谢", "再见", "怎么样", "你是谁", "能做什么", "hello", "hi", "thanks"} {
		if strings.Contains(q, w) && len([]rune(q)) < 15 {
			return "chat"
		}
	}
	return "question"
}

// RunIntentEval 跑意图评测（不调模型）
func RunIntentEval(set *Set) *IntentEval {
	e := &IntentEval{
		Confusion: map[string]map[string]int{},
		PerClass:  map[string]ClassMetric{},
	}

	for _, label := range decision.AllIntents() {
		e.Confusion[string(label)] = map[string]int{}
	}

	for i := range set.Cases {
		c := set.Cases[i]
		if c.ExpectIntent == "" {
			continue // 未标注期望意图的用例不参与
		}

		res, confident := decision.RuleIntentWithConfidence(c.Question)
		predicted := string(res.Intent)

		e.Total++
		if !confident {
			e.LowConf++
		}
		if legacyRuleIntent(c.Question) == c.ExpectIntent {
			e.LegacyCorrect++
		}
		e.Confusion[c.ExpectIntent][predicted]++
		if predicted == c.ExpectIntent {
			e.Correct++
		}

		row := IntentCaseResult{
			ID: c.ID, Question: c.Question, Expected: c.ExpectIntent,
			Predicted: predicted, Correct: predicted == c.ExpectIntent,
			Confident: confident, Reason: res.Reason,
		}
		e.Results = append(e.Results, row)
		if !row.Correct {
			e.Wrong = append(e.Wrong, row)
		}
	}

	if e.Total > 0 {
		e.Accuracy = float64(e.Correct) / float64(e.Total)
		e.LegacyAccuracy = float64(e.LegacyCorrect) / float64(e.Total)
		e.LowConfRate = float64(e.LowConf) / float64(e.Total)
	}
	e.computePerClass()
	return e
}

func (e *IntentEval) computePerClass() {
	labels := make([]string, 0, 3)
	for _, l := range decision.AllIntents() {
		labels = append(labels, string(l))
	}

	for _, label := range labels {
		var tp, fp, fn int
		for expected, row := range e.Confusion {
			for predicted, n := range row {
				switch {
				case expected == label && predicted == label:
					tp += n
				case expected == label && predicted != label:
					fn += n
				case expected != label && predicted == label:
					fp += n
				}
			}
		}
		m := ClassMetric{Support: tp + fn}
		if tp+fp > 0 {
			m.Precision = float64(tp) / float64(tp+fp)
		}
		if tp+fn > 0 {
			m.Recall = float64(tp) / float64(tp+fn)
		}
		if m.Precision+m.Recall > 0 {
			m.F1 = 2 * m.Precision * m.Recall / (m.Precision + m.Recall)
		}
		e.PerClass[label] = m
	}
}

// Matrix 输出文本混淆矩阵（行=期望，列=预测）
func (e *IntentEval) Matrix() string {
	labels := make([]string, 0, 3)
	for _, l := range decision.AllIntents() {
		labels = append(labels, string(l))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%-10s", "期望\\预测"))
	for _, l := range labels {
		sb.WriteString(fmt.Sprintf("%-10s", l))
	}
	sb.WriteString("\n")
	for _, exp := range labels {
		sb.WriteString(fmt.Sprintf("%-10s", exp))
		for _, pre := range labels {
			sb.WriteString(fmt.Sprintf("%-10d", e.Confusion[exp][pre]))
		}
		sb.WriteString("\n")
	}
	return sb.String()
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
	if th.IntentAccuracy > 0 && report.Metrics.IntentAccuracy < th.IntentAccuracy {
		report.ThresholdFailed = append(report.ThresholdFailed,
			fmt.Sprintf("意图准确率 %.2f < 阈值 %.2f", report.Metrics.IntentAccuracy, th.IntentAccuracy))
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
