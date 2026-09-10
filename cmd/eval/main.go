// 评测 CLI：跑黄金集并输出指标报告
//
// 用法：
//   go run ./cmd/eval -set eval/golden_example.json -user <账号> [-type 2] [-judge] [-v]
//
// 指标低于黄金集里的 thresholds 时进程退出码为 1（可直接用于 CI / 发布前卡点）
package main

import (
	"context"
	"deeptalk/common/metrics"
	"deeptalk/common/mysql"
	"deeptalk/common/redis"
	"deeptalk/config"
	"deeptalk/eval"
	"flag"
	"fmt"
	"log"
	"os"
	"time"
)

func main() {
	setPath := flag.String("set", "eval/golden_example.json", "黄金集文件（JSON）")
	user := flag.String("user", "", "用哪个账号的文档做检索（必填）")
	modelType := flag.String("type", "2", "模型类型：1DeepSeek 2RAG 3MCP 4Ollama 5ReAct")
	judge := flag.Bool("judge", false, "额外用 LLM 给忠实度打分（更慢、更贵）")
	verbose := flag.Bool("v", true, "打印每条用例的明细")
	out := flag.String("json", "", "把完整报告写到指定 JSON 文件")
	timeout := flag.Duration("timeout", 10*time.Minute, "整体超时")
	flag.Parse()

	if *user == "" {
		fmt.Fprintln(os.Stderr, "缺少 -user 参数（评测需要指定一个账号来读取它的知识库文档）")
		flag.Usage()
		os.Exit(2)
	}

	// 与线上一致的初始化顺序
	config.GetConfig()
	if err := mysql.InitMysql(); err != nil {
		log.Fatalf("InitMysql failed: %v", err)
	}
	redis.Init()
	metrics.RegisterHelp()

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
		User:      *user,
		ModelType: *modelType,
		Judge:     *judge,
		Verbose:   *verbose,
	})

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
