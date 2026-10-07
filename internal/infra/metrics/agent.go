package metrics

// ======================== Agent 能力指标 ========================
//
// 这一组和 deeptalk_ai_* 的区别在于**它们回答的是"这个 AI 应用好不好用"，
// 而不是"它有没有在跑"**。
//
//	deeptalk_ai_requests_total      → 有多少请求（容量视角）
//	deeptalk_agent_tool_calls_total → 工具选对了吗、Agent 转了几圈（效果视角）
//
// 每一轮对话会从 trace 里派生出这些指标（见 internal/agent/trace）。
// 口径只有那一处，这里只负责落地。
const (
	// 轮次：澄清触发率、平均轮次耗时的分母
	MetricAgentTurns        = "deeptalk_agent_turns_total"           // labels: model, model_type, status=ok|error|timeout
	MetricAgentTurnDuration = "deeptalk_agent_turn_duration_seconds" // labels: model, model_type

	// ReAct 步数分布：Agent 效率的直接体现。
	// 步数持续偏高通常意味着工具描述写得不好，模型在瞎试。
	MetricAgentStepsPerTurn = "deeptalk_agent_steps_per_turn" // labels: model

	// 工具调用：成功率 = status="ok" / 全部。
	// rejected 单列，是因为"按策略正确拒绝"不该被算成故障。
	MetricAgentToolCalls    = "deeptalk_agent_tool_calls_total"       // labels: tool, status=ok|error|timeout|rejected|skipped
	MetricAgentToolDuration = "deeptalk_agent_tool_duration_seconds"  // labels: tool
	MetricAgentModelCalls   = "deeptalk_agent_model_calls_total"      // labels: model, status
	MetricAgentModelLatency = "deeptalk_agent_model_duration_seconds" // labels: model

	// 澄清追问：asked 与 answered 分开才能算出"问了但没答"的流失
	MetricAgentClarify = "deeptalk_agent_clarify_total" // labels: outcome=asked|answered|abandoned

	// 上下文构成：上下文工程的证据。
	// 总和逼近上限而效果没提升，就是该做压缩/截断的信号。
	// kind 另含 budget（本轮可用总预算）与 summary（历史摘要占用）。
	MetricAgentContextTokens = "deeptalk_agent_context_tokens" // labels: kind=system|history|rag|tool|question|summary|budget

	// 循环检测：同一个工具 + 同样的参数被反复成功调用，拦下来的次数。
	// 这个数持续 >0 说明模型在空转（通常是工具描述写得不好），
	// 是"该去改提示词或工具 schema"的信号。
	MetricAgentLoopDetected = "deeptalk_agent_loop_detected_total" // labels: tool

	// 历史压缩的结果。failed 值得单独看：压缩失败会退回未压缩的历史，
	// 表现为"上下文悄悄变长"，而对话本身没有任何异常。
	MetricAgentCompress = "deeptalk_agent_context_compress_total" // labels: result=skipped|compressed|failed

	// 轨迹自身
	MetricAgentTraceSpans   = "deeptalk_agent_trace_spans_total"   // labels: kind
	MetricAgentTracePersist = "deeptalk_agent_trace_persist_total" // labels: result=ok|fail

	// 本地工作区
	MetricLocalAgentOnline    = "deeptalk_local_agent_online"          // 当前在线连接数
	MetricLocalAgentWorkspace = "deeptalk_local_agent_workspace_total" // labels: result=ok|fail

	// 评测：把质量分数也变成时间序列，才能看趋势而不是看一个孤零零的数字
	MetricEvalScore = "deeptalk_eval_score"       // labels: metric
	MetricEvalCases = "deeptalk_eval_cases_total" // labels: metric, result=pass|fail
)

// CountAgentTurn 记一轮对话结束。
func CountAgentTurn(model, modelType, status string) {
	Count(MetricAgentTurns, Labels{"model": model, "model_type": modelType, "status": status}, 1)
}

// ObserveAgentTurnDuration 记一轮耗时（秒）。
func ObserveAgentTurnDuration(model, modelType string, seconds float64) {
	Observe(MetricAgentTurnDuration, Labels{"model": model, "model_type": modelType}, seconds)
}

// ObserveAgentSteps 记一轮里的模型调用次数（= ReAct 步数）。
func ObserveAgentSteps(model string, steps int) {
	Observe(MetricAgentStepsPerTurn, Labels{"model": model}, float64(steps))
}

// CountAgentToolCall 记一次工具调用的结局。
func CountAgentToolCall(tool, status string) {
	Count(MetricAgentToolCalls, Labels{"tool": tool, "status": status}, 1)
}

// ObserveAgentToolDuration 记一次工具耗时（秒）。
func ObserveAgentToolDuration(tool string, seconds float64) {
	Observe(MetricAgentToolDuration, Labels{"tool": tool}, seconds)
}

// CountAgentModelCall 记一次模型调用。
func CountAgentModelCall(model, status string) {
	Count(MetricAgentModelCalls, Labels{"model": model, "status": status}, 1)
}

// ObserveAgentModelLatency 记一次模型调用耗时（秒）。
func ObserveAgentModelLatency(model string, seconds float64) {
	Observe(MetricAgentModelLatency, Labels{"model": model}, seconds)
}

// CountAgentClarify 记一次澄清追问的结局。
func CountAgentClarify(outcome string) {
	Count(MetricAgentClarify, Labels{"outcome": outcome}, 1)
}

// SetAgentContextTokens 设置某一类上下文当前占用的 token 数。
//
// 用 gauge 而不是 counter：它回答的是"这一刻上下文长什么样"，
// 累加没有意义。
func SetAgentContextTokens(kind string, n int) {
	SetGauge(MetricAgentContextTokens, Labels{"kind": kind}, float64(n))
}

// CountAgentTraceSpan 记一条轨迹步骤。
func CountAgentTraceSpan(kind string) {
	Count(MetricAgentTraceSpans, Labels{"kind": kind}, 1)
}

// CountAgentTracePersist 记一次轨迹落库的结果。
func CountAgentTracePersist(ok bool) {
	result := "ok"
	if !ok {
		result = "fail"
	}
	Count(MetricAgentTracePersist, Labels{"result": result}, 1)
}

// CountAgentLoopDetected 记一次被拦下的重复工具调用。
func CountAgentLoopDetected(tool string) {
	Count(MetricAgentLoopDetected, Labels{"tool": tool}, 1)
}

// CountAgentCompress 记一次历史压缩的结果。
func CountAgentCompress(result string) {
	Count(MetricAgentCompress, Labels{"result": result}, 1)
}

// SetLocalAgentOnline 设置当前在线的本地连接器数量。
func SetLocalAgentOnline(n int) {
	SetGauge(MetricLocalAgentOnline, nil, float64(n))
}

// CountLocalAgentWorkspace 记一次工作区切换的结果。
func CountLocalAgentWorkspace(ok bool) {
	result := "ok"
	if !ok {
		result = "fail"
	}
	Count(MetricLocalAgentWorkspace, Labels{"result": result}, 1)
}

// SetEvalScore 设置某个评测指标的分数（0~1）。
func SetEvalScore(metric string, v float64) {
	SetGauge(MetricEvalScore, Labels{"metric": metric}, v)
}

// CountEvalCase 记一批评测用例的通过情况。
//
// 带 n 而不是每条 +1：评测是跑完一次性汇总的，逐条 Count 没有意义，
// 也会让"这批用例覆盖了多少样本"这件事看不出来。
func CountEvalCase(metric, result string, n int) {
	if n <= 0 {
		return
	}
	Count(MetricEvalCases, Labels{"metric": metric, "result": result}, float64(n))
}

// agentMetricsHelp 声明帮助文本并预置 0 值序列。
//
// 预置的意义：没有流量时 /metrics 里也能看到这些序列。
// 否则第一次真正出问题时，你会因为"指标不存在"而误以为是采集坏了。
func agentMetricsHelp() {
	Describe(MetricAgentTurns, "Agent 对话轮次（按模型/类型/结局）")
	Describe(MetricAgentTurnDuration, "单轮对话耗时分布（秒）")
	Describe(MetricAgentStepsPerTurn, "每轮模型调用次数分布（ReAct 步数）")
	Describe(MetricAgentToolCalls, "Agent 工具调用次数与结局")
	Describe(MetricAgentToolDuration, "工具调用耗时分布（秒）")
	Describe(MetricAgentModelCalls, "Agent 模型调用次数与结局")
	Describe(MetricAgentModelLatency, "Agent 模型调用耗时分布（秒）")
	Describe(MetricAgentClarify, "澄清追问的触发与回应情况")
	Describe(MetricAgentContextTokens, "上下文各部分的 token 占用")
	Describe(MetricAgentTraceSpans, "轨迹步骤数（按类型）")
	Describe(MetricAgentTracePersist, "轨迹落库结果")
	Describe(MetricLocalAgentOnline, "在线的本地工作区连接器数量")
	Describe(MetricLocalAgentWorkspace, "本地工作区切换结果")
	Describe(MetricEvalScore, "评测集得分（0~1）")
	Describe(MetricEvalCases, "评测用例通过情况")
	Describe(MetricAgentLoopDetected, "被拦下的重复工具调用次数（模型空转信号）")
	Describe(MetricAgentCompress, "历史压缩结果（跳过/已压缩/失败）")

	// 步数与耗时的分桶要贴合实际：Agent 一般 1~8 步，
	// 默认的 0.005/0.01/... 对"秒"这个量级完全不合适。
	SetHistogramBuckets(MetricAgentStepsPerTurn, []float64{1, 2, 3, 4, 5, 6, 7, 8, 10, 12})
	SetHistogramBuckets(MetricAgentTurnDuration, []float64{0.5, 1, 2, 3, 5, 8, 13, 21, 34, 55})
	SetHistogramBuckets(MetricAgentModelLatency, []float64{0.2, 0.5, 1, 2, 3, 5, 8, 13, 21, 34})
	SetHistogramBuckets(MetricAgentToolDuration, []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20})

	SetGauge(MetricLocalAgentOnline, nil, 0)
	for _, r := range []string{"ok", "fail"} {
		Count(MetricAgentTracePersist, Labels{"result": r}, 0)
		Count(MetricLocalAgentWorkspace, Labels{"result": r}, 0)
	}
	Count(MetricEvalCases, Labels{"metric": "none", "result": "pass"}, 0)
	for _, s := range []string{"ok", "error", "timeout", "rejected", "skipped"} {
		Count(MetricAgentToolCalls, Labels{"tool": "none", "status": s}, 0)
		Count(MetricAgentModelCalls, Labels{"model": "none", "status": s}, 0)
	}
	for _, o := range []string{"asked", "answered", "abandoned"} {
		Count(MetricAgentClarify, Labels{"outcome": o}, 0)
	}
	for _, k := range []string{"system", "history", "rag", "tool", "question", "summary", "budget"} {
		SetGauge(MetricAgentContextTokens, Labels{"kind": k}, 0)
	}
	for _, r := range []string{"skipped", "compressed", "failed"} {
		Count(MetricAgentCompress, Labels{"result": r}, 0)
	}
	Count(MetricAgentLoopDetected, Labels{"tool": "none"}, 0)
	for _, k := range []string{"turn", "model", "tool", "retrieve", "clarify", "cache"} {
		Count(MetricAgentTraceSpans, Labels{"kind": k}, 0)
	}
	for _, s := range []string{"ok", "error", "timeout"} {
		Count(MetricAgentTurns, Labels{"model": "none", "model_type": "none", "status": s}, 0)
	}
}
