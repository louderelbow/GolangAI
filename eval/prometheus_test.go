package eval

import (
	"net/http/httptest"
	"strings"
	"testing"

	"deeptalk/internal/infra/metrics"
)

// TestExportPrometheus 一条测试里按顺序验两件事：
//
//  1. 样本数为 0 的指标不导出（空分数会污染趋势线，看起来像"退化到 0"）
//  2. 导出指标的分母跟着结果上的 Scored* 标记走，而不是"用例总数"
//
// 两项必须在同一个测试里有先后：Prometheus 的指标是**进程级全局**的，
// 一旦先跑了有样本的那次，就再也断言不出"空样本不导出"了。
//
// 第 2 点正是原实现里召回率门禁永远失败的成因：算比率时只在声明了
// expectSources 的用例上累加分子，分母却是全部用例。两处口径各写一遍，
// 一处对了另一处没对。现在两边共用同一个标记。
func TestExportPrometheus(t *testing.T) {
	metrics.RegisterHelp()

	// ---- 1. 空样本：什么都不该导出 ----
	ExportPrometheus(&Report{Metrics: Metrics{}})
	ExportPrometheus(nil) // 也不能 panic

	body := scrapeEval(t)
	for _, m := range []string{"retrieval_recall", "answer_coverage", "refusal_accuracy", "faithfulness"} {
		if strings.Contains(body, `deeptalk_eval_score{metric="`+m+`"}`) {
			t.Fatalf("样本数为 0 的 %s 不该导出分数\n%s", m, lineWith(body, "deeptalk_eval_score"))
		}
	}

	// ---- 2. 有样本：分母必须只数计入的用例 ----
	r := &Report{
		Metrics: Metrics{RetrievalRecall: 0.5, RecallSamples: 2},
		Results: []CaseResult{
			{ID: "a", RetrievalHit: true, ScoredRecall: true},   // 计入且命中
			{ID: "b", RetrievalHit: false, ScoredRecall: true},  // 计入但未命中
			{ID: "c", RetrievalHit: false, ScoredRecall: false}, // 不计入
		},
	}
	ExportPrometheus(r)

	body = scrapeEval(t)
	if !strings.Contains(body, `deeptalk_eval_score{metric="retrieval_recall"} 0.5`) {
		t.Errorf("缺少召回率分数\n%s", lineWith(body, "deeptalk_eval_score"))
	}
	if !strings.Contains(body, `deeptalk_eval_cases_total{metric="retrieval_recall",result="pass"} 1`) {
		t.Errorf("通过数应为 1（只有 a 命中且计入）\n%s", lineWith(body, "eval_cases_total"))
	}
	if !strings.Contains(body, `deeptalk_eval_cases_total{metric="retrieval_recall",result="fail"} 1`) {
		t.Errorf("未通过数应为 1（b 计入但未命中；c 不计入）\n%s", lineWith(body, "eval_cases_total"))
	}
}

func scrapeEval(t *testing.T) string {
	t.Helper()
	w := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}

func lineWith(body, needle string) string {
	var sb strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, needle) {
			sb.WriteString(line + "\n")
		}
	}
	return sb.String()
}
