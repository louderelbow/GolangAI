package inference

import (
	"time"

	"deeptalk/internal/infra/metrics"
)

// 推理调度指标。
//
// 关于分位数：规格里写的是 quantile 标签（Summary 风格），这里改用 Histogram。
// 原因是 Summary 的分位数无法跨实例聚合——多副本部署时算不出全局 P99，
// 而 Histogram 可以用 histogram_quantile() 在查询侧聚合。取分位数的写法见 README。
const (
	MetricQueueDepth   = "deeptalk_inference_queue_depth"
	MetricInflight     = "deeptalk_inference_inflight"
	MetricQueueWait    = "deeptalk_inference_queue_wait_ms"
	MetricLatency      = "deeptalk_inference_latency_ms"
	MetricRejected     = "deeptalk_inference_rejected_total"
	MetricBreakerState = "deeptalk_inference_breaker_state"
)

// 拒绝原因（对应 deeptalk_inference_rejected_total 的 reason 标签）
const (
	ReasonQueueFull    = "queue_full"
	ReasonQueueTimeout = "queue_timeout"
	ReasonBreakerOpen  = "breaker_open"
	ReasonShuttingDown = "shutting_down"
)

// 熔断状态取值（对应 state 标签）
const (
	StateClosed   = "closed"
	StateOpen     = "open"
	StateHalfOpen = "half_open"
)

// msBuckets 以毫秒为单位的耗时分桶：覆盖"排队 3 秒上限"与"模型几十秒"两个量级。
var msBuckets = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 3000, 10000, 30000, 60000}

var breakerStateValue = map[string]float64{StateClosed: 0, StateHalfOpen: 1, StateOpen: 2}

// registerMetrics 登记 HELP 与自定义分桶，并预置 0 值序列。
//
// 必须在任何请求进入之前调用（NewScheduler 里做）：直方图分桶在 collector
// 首次注册时固定，晚了就只能用默认的秒级分桶。
func registerMetrics(models []string) {
	metrics.Describe(MetricQueueDepth, "推理队列当前排队深度（按模型）")
	metrics.Describe(MetricInflight, "推理中（已占槽位）的请求数（按模型）")
	metrics.Describe(MetricQueueWait, "推理请求排队等待时长（毫秒，按模型）")
	metrics.Describe(MetricLatency, "推理请求端到端耗时（毫秒，按模型）")
	metrics.Describe(MetricRejected, "被调度器拒绝的推理请求数（按模型与原因）")
	metrics.Describe(MetricBreakerState, "推理准入熔断器状态（0=closed 1=half_open 2=open）")

	metrics.SetHistogramBuckets(MetricQueueWait, msBuckets)
	metrics.SetHistogramBuckets(MetricLatency, msBuckets)

	// 预置 0 值：让 /metrics 从第一次抓取就完整，便于验证抓取配置
	if len(models) == 0 {
		models = []string{""}
	}
	for _, m := range models {
		metrics.SetGauge(MetricQueueDepth, metrics.Labels{"model": m}, 0)
		metrics.SetGauge(MetricInflight, metrics.Labels{"model": m}, 0)
		metrics.SetGauge(MetricBreakerState, metrics.Labels{"model": m}, 0)
		metrics.EnsureHistogram(MetricQueueWait, metrics.Labels{"model": m})
		metrics.EnsureHistogram(MetricLatency, metrics.Labels{"model": m})
	}
	for _, reason := range []string{ReasonQueueFull, ReasonQueueTimeout, ReasonBreakerOpen, ReasonShuttingDown} {
		metrics.Count(MetricRejected, metrics.Labels{"model": "", "reason": reason}, 0)
	}
}

func setQueueDepth(model string, n int) {
	metrics.SetGauge(MetricQueueDepth, metrics.Labels{"model": model}, float64(n))
}

func setInflight(model string, n int) {
	metrics.SetGauge(MetricInflight, metrics.Labels{"model": model}, float64(n))
}

func observeQueueWait(model string, d time.Duration) {
	metrics.Observe(MetricQueueWait, metrics.Labels{"model": model}, float64(d.Milliseconds()))
}

func observeLatency(model string, d time.Duration) {
	metrics.Observe(MetricLatency, metrics.Labels{"model": model}, float64(d.Milliseconds()))
}

func countRejected(model, reason string) {
	metrics.Count(MetricRejected, metrics.Labels{"model": model, "reason": reason}, 1)
}

func setBreakerState(model, state string) {
	v, ok := breakerStateValue[state]
	if !ok {
		v = -1
	}
	metrics.SetGauge(MetricBreakerState, metrics.Labels{"model": model}, v)
}
