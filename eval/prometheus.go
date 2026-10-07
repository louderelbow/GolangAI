package eval

import "deeptalk/internal/infra/metrics"

// ExportPrometheus 把一次评测的结果导成 Prometheus 指标。
//
// 为什么评测结果要进 Prometheus：分数本身是个数字，**分数的趋势**才是信息。
// 一次跑出 0.74 说明不了什么；连着五次 0.74 → 0.70 → 0.66 说明检索在退化。
// 把分数做成时间序列，评测才从"发布前跑一次的仪式"变成"能看趋势的门禁"。
//
// 分母直接取 CaseResult 上的 Scored* 标记，而不是在这里重算一遍
// "哪些用例算数"——两处各写一遍口径，正是之前召回率门禁永远失败的成因。
func ExportPrometheus(r *Report) {
	if r == nil {
		return
	}
	m := r.Metrics

	if m.RecallSamples > 0 {
		metrics.SetEvalScore("retrieval_recall", m.RetrievalRecall)
		countScored(r, "retrieval_recall",
			func(res CaseResult) bool { return res.ScoredRecall },
			func(res CaseResult) bool { return res.RetrievalHit })
	}
	if m.CoverageSamples > 0 {
		metrics.SetEvalScore("answer_coverage", m.AnswerCoverage)
		// 覆盖率是连续值，"通过"按"要点全中"计
		countScored(r, "answer_coverage",
			func(res CaseResult) bool { return res.ScoredCoverage },
			func(res CaseResult) bool { return res.Expected > 0 && res.Covered == res.Expected })
	}
	if m.RefusalSamples > 0 {
		metrics.SetEvalScore("refusal_accuracy", m.RefusalAccuracy)
		countScored(r, "refusal_accuracy",
			func(res CaseResult) bool { return res.ScoredRefusal },
			func(res CaseResult) bool { return res.RefusalOK })
	}
	if m.JudgeSamples > 0 {
		metrics.SetEvalScore("faithfulness", m.Faithfulness)
	}
	if r.Intent != nil && r.Intent.Total > 0 {
		metrics.SetEvalScore("intent_accuracy", r.Intent.Accuracy)
	}
}

func countScored(r *Report, metric string, inScope, pass func(CaseResult) bool) {
	passN, failN := 0, 0
	for _, res := range r.Results {
		if !inScope(res) {
			continue
		}
		if pass(res) {
			passN++
		} else {
			failN++
		}
	}
	if passN+failN == 0 {
		return
	}
	metrics.CountEvalCase(metric, "pass", passN)
	metrics.CountEvalCase(metric, "fail", failN)
}
