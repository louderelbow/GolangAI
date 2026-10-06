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

func main() {
	setPath := flag.String("set", "eval/golden_example.json", "黄金集文件（JSON）")
	user := flag.String("user", "", "用哪个账号的文档做检索（必填）")
	modelType := flag.String("type", "2", "模型类型：1 DeepSeek / 2 RAG / 6 Unified Agent")
	judge := flag.Bool("judge", false, "额外用 LLM 给忠实度打分（更慢、更贵）")
	intentOnly := flag.Bool("intentOnly", false, "只评测意图识别（规则层，本地零成本，秒级返回）")
	intentLLM := flag.Bool("intentLLM", false, "意图评测时对低置信度用例调用真实模型跑兜底（少量调用）")
	verbose := flag.Bool("v", true, "打印每条用例的明细")
	out := flag.String("json", "", "把完整报告写到指定 JSON 文件")
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

	start := time.Now()
	report := eval.Run(ctx, set, eval.Options{
		User:       *user,
		ModelType:  *modelType,
		Judge:      *judge,
		Verbose:    *verbose,
		IntentOnly: *intentOnly,
		IntentLLM:  *intentLLM,
	})

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
		if report.Intent != nil && report.Intent.Accuracy < 0.8 {
			os.Exit(1)
		}
		return
	}

	fmt.Printf("\n=== 评测结果 ===\n")
	fmt.Printf("用例数      : %d（失败 %d）\n", report.Total, report.Failed)
	fmt.Printf("检索召回率  : %.2f\n", report.Metrics.RetrievalRecall)
	fmt.Printf("要点覆盖率  : %.2f\n", report.Metrics.AnswerCoverage)
	fmt.Printf("拒答准确率  : %.2f\n", report.Metrics.RefusalAccuracy)
	if *judge {
		fmt.Printf("忠实度      : %.2f\n", report.Metrics.Faithfulness)
	}
	fmt.Printf("平均延迟    : %d ms\n", report.AvgLatencyMS)
	fmt.Printf("总耗时      : %s\n", time.Since(start).Round(time.Second))

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
