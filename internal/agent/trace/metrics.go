package trace

import "deeptalk/internal/infra/metrics"

// Flush 把一条轨迹派生成 Prometheus 指标。
//
// 这是本包存在的一半理由：**指标口径只有这一处**。
// 一轮对话产生的所有能力指标都在这里算出来，而不是让每个调用点各自 Count——
// 后者迟早会出现"某个分支忘了记"，而那种洞在仪表盘上只表现为数字偏小，
// 没有任何报错，最难发现。
//
// 另一半理由是落库（见 store.go）：指标告诉你"整体在变差"，
// 单条轨迹才能告诉你"这一轮到底怎么了"。两者缺一不可。
func Flush(t Trace) {
	if t.ID == "" && len(t.Steps) == 0 {
		return
	}

	metrics.CountAgentTurn(t.Model, t.ModelType, string(t.Status))
	metrics.ObserveAgentTurnDuration(t.Model, t.ModelType, t.Duration().Seconds())

	// ReAct 步数 = 这一轮调了几次模型。
	//
	// 比"工具调用次数"更能反映 Agent 效率：一个反复空转的 Agent 会有很多次
	// 模型调用、很少工具调用（它在想但没动手）。这个分布长尾变厚，
	// 通常意味着工具描述写得不好，模型在瞎试。
	metrics.ObserveAgentSteps(t.Model, len(t.Of(KindModel)))

	for _, s := range t.Steps {
		metrics.CountAgentTraceSpan(string(s.Kind))
		sec := s.Duration.Seconds()

		switch s.Kind {
		case KindModel:
			// 模型名取自整轮，不取自步骤名：模型是本轮的属性，
			// 而步骤名区分的是 generate / stream。
			metrics.CountAgentModelCall(t.Model, string(s.Status))
			metrics.ObserveAgentModelLatency(t.Model, sec)
		case KindTool:
			metrics.CountAgentToolCall(s.Name, string(s.Status))
			metrics.ObserveAgentToolDuration(s.Name, sec)
		case KindClarify:
			// 澄清的"结局"放在 Name 里（asked / answered / abandoned），
			// 而不是 Status——Status 描述的是这一步执行得成不成功，
			// 而"问了用户"本身就是成功，它有意义的维度是结果分类。
			metrics.CountAgentClarify(s.Name)
		}
	}
}
