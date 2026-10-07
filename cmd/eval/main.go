// 评测 CLI：跑黄金集并输出指标报告
//
// 用法：
//
//	go run ./cmd/eval -set eval/golden_example.json -user <账号> [-type 2] [-judge] [-v]
//
// 指标低于黄金集里的 thresholds 时进程退出码为 1（可直接用于 CI / 发布前卡点）
package main

import (
	"context"
	"deeptalk/eval"
	"deeptalk/internal/decision"
	"deeptalk/internal/infra/config"
	"deeptalk/internal/infra/metrics"
	"deeptalk/internal/infra/mysql"
	"deeptalk/internal/infra/redis"
	"flag"
	"fmt"
	"log"
	"os"
	"time"
)

// printCapability 打印分维度成绩。
//
// 这是"企业级"和"跑个分"的分界：一个总分无法指导优化，因为分数变化
// 可能只来自简单档的题量变化。分维度之后才看得出能力到底哪一块变了。
//
// 每个指标都带 95% Wilson 置信区间，样本不足时判决是"样本不足"而不是
// "不达标" —— 后者会让人去改本来没坏的东西。
func printCapability(r *eval.Report) {
	if len(r.Dimensions) == 0 {
		return
	}

	for _, kind := range []string{"difficulty", "tag"} {
		var dims []eval.Dimension
		for _, d := range r.Dimensions {
			if d.Kind == kind {
				dims = append(dims, d)
			}
		}
		if len(dims) == 0 {
			continue
		}

		head := map[string]string{"difficulty": "按难度", "tag": "按能力标签"}[kind]
		fmt.Printf("\n--- %s ---\n", head)
		fmt.Printf("%-14s %-16s %s\n", "维度", "指标", "成绩（含 95% 置信区间）")
		for _, d := range dims {
			for i, s := range d.Scores {
				name := d.Name
				if i > 0 {
					name = "" // 同一维度的后续行不重复打维度名
				}
				if s.Samples == 0 && s.Threshold <= 0 {
					continue
				}
				label := name
				if i == 0 {
					label = fmt.Sprintf("%s(%d)", d.Name, d.Total)
				}
				fmt.Printf("%-14s %-16s %s\n", label, s.Metric, s.String())
			}
		}
	}

	if len(r.Insufficient) > 0 {
		fmt.Printf("\n⚠ 样本不足（补样本，不要改代码）：\n")
		for _, s := range r.Insufficient {
			fmt.Printf("   - %s\n", s)
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "(未记录)"
	}
	return s
}

func main() {
	modelName := flag.String("model", "", "本次实际使用的生成模型名（写进报告，便于跨模型对照与归因）")
	setPath := flag.String("set", "eval/golden_example.json", "黄金集文件（JSON）")
	user := flag.String("user", "", "用哪个账号的文档做检索（必填）")
	modelType := flag.String("type", "2", "模型类型：2 RAG / 6 Unified Agent")
	judge := flag.Bool("judge", false, "额外用 LLM 给忠实度打分（更慢、更贵）")
	intentOnly := flag.Bool("intentOnly", false, "只评测意图识别（规则层，本地零成本，秒级返回）")
	intentLLM := flag.Bool("intentLLM", false, "意图评测时对低置信度用例调用真实模型跑兜底（少量调用）")
	verbose := flag.Bool("v", true, "打印每条用例的明细")
	out := flag.String("json", "", "把完整报告写到指定 JSON 文件")
	metricsOut := flag.String("metricsOut", "", "把评测指标按 Prometheus 文本格式写到指定文件（给 Pushgateway / textfile collector）")
	timeout := flag.Duration("timeout", 10*time.Minute, "整体超时")
	flag.Parse()

	// 只评测意图时不需要账号/文档，也不需要连 MySQL
	if *user == "" && !*intentOnly {
		fmt.Fprintln(os.Stderr, "缺少 -user 参数（评测需要指定一个账号来读取它的知识库文档）")
		flag.Usage()
		os.Exit(2)
	}

	config.GetConfig()
	metrics.RegisterHelp()

	if !*intentOnly {
		// 与线上一致的初始化顺序（意图评测不需要 DB/Redis）
		if err := mysql.InitMysql(); err != nil {
			log.Fatalf("InitMysql failed: %v", err)
		}
		redis.Init()
	}

	set, err := eval.LoadSet(*setPath)
	if err != nil {
		log.Fatalf("加载黄金集失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	fmt.Printf("=== 评测开始 ===\n")
	fmt.Printf("黄金集: %s (%d 条)  账号: %s  模型类型: %s  忠实度打分: %v\n\n",
		set.Name, len(set.Cases), *user, *modelType, *judge)

	report := eval.Run(ctx, set, eval.Options{
		User:       *user,
		ModelType:  *modelType,
		Judge:      *judge,
		Verbose:    *verbose,
		IntentOnly: *intentOnly,
		IntentLLM:  *intentLLM,
	})

	// 记录被测模型：不记录被测对象的评测，原理上无法回答"被测对象变强了吗"。
	// 未显式传 -model 时退回实际装配出来的模型名。
	report.Model = *modelName
	if report.Model == "" {
		report.Model = "（由 config 决定，未显式标注）"
	}

	// ---- 意图识别评测：先用规则层（本地零成本），可选再用真实模型跑兜底 ----
	if *intentLLM && report.Intent != nil {
		intentModel, err := decision.NewIntentLLM(ctx)
		if err != nil {
			log.Printf("构造意图兜底模型失败: %v", err)
		} else {
			fmt.Printf("\n>>> 对低置信度用例调用真实模型跑兜底...\n")
			report.Intent.ApplyLLMFallback(ctx, intentModel)
			report.Metrics.IntentAccuracy = report.Intent.LLMAccuracy
		}
	}

	if report.Intent != nil && report.Intent.Total > 0 {
		ie := report.Intent
		fmt.Printf("\n=== 意图识别评测（规则层，不调模型）===\n")
		fmt.Printf("标注用例    : %d（正确 %d）\n", ie.Total, ie.Correct)
		fmt.Printf("准确率      : %.2f  （重构前旧规则 %.2f，提升了 %.0f 个百分点）\n", ie.Accuracy, ie.LegacyAccuracy, (ie.Accuracy-ie.LegacyAccuracy)*100)
		fmt.Printf("低置信占比  : %.2f  (%d/%d，这部分线上会走 LLM 兜底)\n", ie.LowConfRate, ie.LowConf, ie.Total)
		if ie.LLMAccuracy > 0 {
			fmt.Printf("加入 LLM 兜底后准确率: %.2f  （%d/%d）\n", ie.LLMAccuracy, ie.LLMCorrect, ie.Total)
		}
		fmt.Printf("\n混淆矩阵（行=期望，列=预测）:\n%s", ie.Matrix())
		fmt.Printf("\n分类别指标:\n")
		for _, label := range []string{"summary", "question", "chat"} {
			m, ok := ie.PerClass[label]
			if !ok || m.Support == 0 {
				continue
			}
			fmt.Printf("  %-9s P=%.2f R=%.2f F1=%.2f (支撑=%d)\n", label, m.Precision, m.Recall, m.F1, m.Support)
		}
		if len(ie.Wrong) > 0 {
			fmt.Printf("\n判错的用例 (%d 条):\n", len(ie.Wrong))
			for _, w := range ie.Wrong {
				fmt.Printf("  [%s] 期望=%s 实际=%s | %s | %s\n", w.ID, w.Expected, w.Predicted, w.Question, w.Reason)
			}
		}
	}

	if *intentOnly {
		fmt.Printf("\n（-intentOnly：已跳过检索与生成评测）\n")
		if *out != "" {
			if err := eval.SaveJSON(report, *out); err != nil {
				log.Printf("写报告失败: %v", err)
			}
		}
		// 用黄金集里可配置的 intentAccuracy 阈值判定（不再硬编码），
		// 并把未达标的项打印出来，便于 CI 日志里直接看到原因。
		if len(report.ThresholdFailed) > 0 {
			fmt.Printf("\n❌ 未达标：\n")
			for _, f := range report.ThresholdFailed {
				fmt.Printf("   - %s\n", f)
			}
			os.Exit(1)
		}
		return
	}

	fmt.Printf("\n=== 评测结果 ===\n")
	fmt.Printf("模型        : %s（类型 %s）\n", orNone(report.Model), report.ModelType)
	fmt.Printf("用例数      : %d（失败 %d）\n", report.Total, report.Failed)
	fmt.Printf("耗时        : %d ms\n", report.DurationMS)

	// 比率后面带上样本数：只报比率不报样本数，是评测报告最常见的误导来源
	//（"召回 0.74"在 12 条和 200 条上是两回事）。
	fmt.Printf("检索召回率  : %.2f  （%d 条计入）\n", report.Metrics.RetrievalRecall, report.Metrics.RecallSamples)
	fmt.Printf("要点覆盖率  : %.2f  （%d 条计入）\n", report.Metrics.AnswerCoverage, report.Metrics.CoverageSamples)
	fmt.Printf("拒答准确率  : %.2f  （%d 条计入）\n", report.Metrics.RefusalAccuracy, report.Metrics.RefusalSamples)
	if *judge {
		fmt.Printf("忠实度      : %.2f  （%d 条计入）\n", report.Metrics.Faithfulness, report.Metrics.JudgeSamples)
	}
	fmt.Printf("平均延迟    : %d ms\n", report.AvgLatencyMS)

	printCapability(report)

	// 导出成 Prometheus 时间序列：分数本身是数字，**分数的趋势**才是信息。
	// 一次 0.74 说明不了什么；连着 0.74 → 0.70 → 0.66 说明检索在退化。
	eval.ExportPrometheus(report)
	if *metricsOut != "" {
		f, err := os.Create(*metricsOut)
		if err != nil {
			log.Printf("写指标失败: %v", err)
		} else {
			if err := metrics.WriteTo(f); err != nil {
				log.Printf("写指标失败: %v", err)
			}
			_ = f.Close()
			fmt.Printf("指标已写入 %s（交给 Pushgateway 或 textfile collector）\n", *metricsOut)
		}
	}

	if len(report.ThresholdFailed) > 0 {
		fmt.Printf("\n❌ 未达标：\n")
		for _, f := range report.ThresholdFailed {
			fmt.Printf("   - %s\n", f)
		}
	}

	if *out != "" {
		if err := eval.SaveJSON(report, *out); err != nil {
			log.Printf("写报告失败: %v", err)
		} else {
			fmt.Printf("\n报告已写入 %s\n", *out)
		}
	}

	if len(report.ThresholdFailed) > 0 {
		os.Exit(1)
	}
}
