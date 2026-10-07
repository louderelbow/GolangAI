// Package metrics 提供业务与 Agent 可靠性指标的采集与暴露。
//
// 实现基于官方 Prometheus client_golang：指标对象注册到默认 registry，
// /metrics 由 promhttp 渲染，因此天然带上 go_* / process_* 运行时指标，
// 输出格式也由官方库保证，不再自行拼接文本。
//
// 对调用方保留便捷函数（Count / SetGauge / Observe / RecordAIRequest …），
// 业务代码不需要知道底层用的是哪个指标库。
package metrics

import (
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Labels 标签集。
type Labels map[string]string

var (
	mu       sync.Mutex
	counters = map[string]*prometheus.CounterVec{}
	gauges   = map[string]*prometheus.GaugeVec{}
	hists    = map[string]*prometheus.HistogramVec{}
	helps    = map[string]string{}
)

// defaultBuckets 直方图分桶（秒），与原实现保持一致，便于历史数据衔接。
var defaultBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60}

func sortedKeys(l Labels) []string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func helpOf(name string) string {
	if h := helps[name]; h != "" {
		return h
	}
	return name
}

// Describe 登记指标的 HELP 文案（可选；未登记时用指标名兜底）。
// 可变参数是为了兼容旧的 Describe(name, help, type) 调用形式——类型由
// client_golang 的 collector 决定，不再需要显式声明。
// 必须在首次记录该指标之前调用效果最好——client_golang 的 HELP 在注册时固定。
func Describe(name, help string, _ ...string) {
	mu.Lock()
	defer mu.Unlock()
	helps[name] = help
}

func labelsOf(l Labels) prometheus.Labels {
	if len(l) == 0 {
		return nil
	}
	return prometheus.Labels(l)
}

// register 三种 Vec 共用的注册逻辑：已存在同名 collector 时复用，
// 避免"重复 init / 重复调用 RegisterHelp"直接 panic。
func counterVec(name string, labels Labels) *prometheus.CounterVec {
	mu.Lock()
	defer mu.Unlock()
	if v, ok := counters[name]; ok {
		return v
	}
	keys := sortedKeys(labels)
	vec := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: helpOf(name)}, keys)
	if err := prometheus.Register(vec); err != nil {
		if reused, ok := reuseCounter(err); ok {
			counters[name] = reused
			return reused
		}
		log.Printf("[metrics] register counter %s failed: %v", name, err)
	}
	counters[name] = vec
	return vec
}

func gaugeVec(name string, labels Labels) *prometheus.GaugeVec {
	mu.Lock()
	defer mu.Unlock()
	if v, ok := gauges[name]; ok {
		return v
	}
	keys := sortedKeys(labels)
	vec := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: helpOf(name)}, keys)
	if err := prometheus.Register(vec); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if reused, ok := are.ExistingCollector.(*prometheus.GaugeVec); ok {
				gauges[name] = reused
				return reused
			}
		}
		log.Printf("[metrics] register gauge %s failed: %v", name, err)
	}
	gauges[name] = vec
	return vec
}

func histVec(name string, labels Labels) *prometheus.HistogramVec {
	mu.Lock()
	defer mu.Unlock()
	if v, ok := hists[name]; ok {
		return v
	}
	keys := sortedKeys(labels)
	vec := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    name,
		Help:    helpOf(name),
		Buckets: defaultBuckets,
	}, keys)
	if err := prometheus.Register(vec); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if reused, ok := are.ExistingCollector.(*prometheus.HistogramVec); ok {
				hists[name] = reused
				return reused
			}
		}
		log.Printf("[metrics] register histogram %s failed: %v", name, err)
	}
	hists[name] = vec
	return vec
}

func reuseCounter(err error) (*prometheus.CounterVec, bool) {
	are, ok := err.(prometheus.AlreadyRegisteredError)
	if !ok {
		return nil, false
	}
	reused, ok := are.ExistingCollector.(*prometheus.CounterVec)
	return reused, ok
}

// Count 计数器累加。
func Count(name string, labels Labels, delta float64) {
	counterVec(name, labels).With(labelsOf(labels)).Add(delta)
}

// SetGauge 设置瞬时值。
func SetGauge(name string, labels Labels, value float64) {
	gaugeVec(name, labels).With(labelsOf(labels)).Set(value)
}

// Observe 记录一次观测值（耗时等）。
func Observe(name string, labels Labels, v float64) {
	histVec(name, labels).With(labelsOf(labels)).Observe(v)
}

// EnsureHistogram 预创建直方图序列（count/sum 为 0）。
// 让 /metrics 在没有流量时也能看到完整分桶，而不是只有 HELP/TYPE 空壳。
func EnsureHistogram(name string, labels Labels) {
	histVec(name, labels).With(labelsOf(labels))
}

// Handler 暴露 /metrics：由 promhttp 渲染标准 Prometheus 文本格式，
// 同时包含 go_* / process_* 运行时指标。
func Handler() http.Handler {
	return promhttp.Handler()
}

// ======================== 指标名 ========================

const (
	MetricAIRequests    = "deeptalk_ai_requests_total"
	MetricAITokens      = "deeptalk_ai_tokens_total"
	MetricAICostMicros  = "deeptalk_ai_cost_micros_total"
	MetricAIDuration    = "deeptalk_ai_request_duration_seconds"
	MetricAICacheHits   = "deeptalk_ai_cache_total"
	MetricActiveSession = "deeptalk_ai_active_sessions"

	// 熔断器（sony/gobreaker）
	MetricCBState    = "deeptalk_circuit_breaker_state"          // 0=closed 1=half-open 2=open
	MetricCBEvents   = "deeptalk_circuit_breaker_events_total"   // 状态迁移次数
	MetricCBRejected = "deeptalk_circuit_breaker_rejected_total" // 因熔断被快速拒绝的次数

	// Agent 可靠性（PHASE-2）
	MetricAgentBudgetExceeded = "deeptalk_agent_budget_exceeded_total" // 预算耗尽，labels: reason=steps|tokens|wallclock
	MetricAgentToolRetry      = "deeptalk_agent_tool_retry_total"      // 工具重试，labels: tool, result=success|fail
	MetricAgentDegraded       = "deeptalk_agent_degraded_total"        // 降级，labels: level=retry|fallback_model|plain_chat|error
	MetricAgentTimeout        = "deeptalk_agent_timeout_total"         // 超时，labels: kind=tool|model|turn
)

// ======================== Agent 可靠性便捷计数 ========================
//
// 把 label 取值集中在一处，避免各调用点拼错导致指标分裂成多条时间线。

// CountAgentBudgetExceeded 记录一次预算耗尽。reason 取 steps / tokens / wallclock。
func CountAgentBudgetExceeded(reason string) {
	Count(MetricAgentBudgetExceeded, Labels{"reason": reason}, 1)
}

// CountAgentToolRetry 记录一次工具重试结果。
func CountAgentToolRetry(tool string, ok bool) {
	result := "fail"
	if ok {
		result = "success"
	}
	Count(MetricAgentToolRetry, Labels{"tool": tool, "result": result}, 1)
}

// CountAgentDegraded 记录一次降级。level 取 retry / fallback_model / plain_chat / error。
func CountAgentDegraded(level string) {
	Count(MetricAgentDegraded, Labels{"level": level}, 1)
}

// CountAgentTimeout 记录一次超时。kind 取 tool / model / turn。
func CountAgentTimeout(kind string) {
	Count(MetricAgentTimeout, Labels{"kind": kind}, 1)
}

// ======================== 熔断器 ========================

var cbStateValue = map[string]float64{"closed": 0, "half-open": 1, "open": 2}

// SetCircuitState 记录熔断器当前状态。
func SetCircuitState(name, state string) {
	v, ok := cbStateValue[state]
	if !ok {
		v = -1
	}
	SetGauge(MetricCBState, Labels{"name": name}, v)
}

// CountCircuitEvent 记录一次状态迁移。
func CountCircuitEvent(name, to string) {
	Count(MetricCBEvents, Labels{"name": name, "to": to}, 1)
}

// CountCircuitRejected 记录一次"因熔断被快速拒绝"。
func CountCircuitRejected(name string) {
	Count(MetricCBRejected, Labels{"name": name}, 1)
}

// ======================== AI 业务指标 ========================

// AIRequest AI 请求观测数据
type AIRequest struct {
	Model            string
	User             string
	ModelType        string
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int // 命中上游前缀缓存的 token
	Latency          time.Duration
	CostMicros       int64
	Status           string // ok / error / refused / quota_exceeded
	Source           string // llm / semantic_cache
}

// RecordAIRequest 记录一次 AI 请求（token、费用、延迟、状态）。
func RecordAIRequest(req AIRequest) {
	Count(MetricAIRequests, Labels{
		"model": req.Model, "model_type": req.ModelType,
		"status": req.Status, "source": req.Source,
	}, 1)

	if req.PromptTokens > 0 {
		Count(MetricAITokens, Labels{"model": req.Model, "kind": "prompt"}, float64(req.PromptTokens))
	}
	if req.CompletionTokens > 0 {
		Count(MetricAITokens, Labels{"model": req.Model, "kind": "completion"}, float64(req.CompletionTokens))
	}
	if req.CachedTokens > 0 {
		// 命中上游前缀缓存的 token 数（省下来的钱在这里体现）
		Count(MetricAITokens, Labels{"model": req.Model, "kind": "cached"}, float64(req.CachedTokens))
	}
	if req.CostMicros > 0 {
		Count(MetricAICostMicros, Labels{"model": req.Model}, float64(req.CostMicros))
	}
	Observe(MetricAIDuration, Labels{"model": req.Model, "source": req.Source}, req.Latency.Seconds())
}

// RecordCacheLookup 记录语义缓存查询结果。
func RecordCacheLookup(hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}
	Count(MetricAICacheHits, Labels{"result": result}, 1)
}

// RegisterHelp 登记 HELP 文案并预置 0 值序列（启动时调用一次即可）。
//
// 预置的意义：Prometheus 的 HELP 在 collector 注册时固定，而带标签的序列
// 要等第一次 With() 才出现。这里先把常用序列创建出来，保证 /metrics
// 从第一次抓取起就是完整的，抓取配置和目标发现也更容易验证。
func RegisterHelp() {
	Describe(MetricAIRequests, "AI 请求总数（按模型/类型/状态/来源）")
	Describe(MetricAITokens, "AI token 消耗（按 prompt/completion/cached 分类）")
	Describe(MetricAICostMicros, "AI 费用累计（微元，1 元 = 1e6）")
	Describe(MetricAIDuration, "AI 请求耗时分布（秒）")
	Describe(MetricAICacheHits, "语义缓存命中/未命中次数")
	Describe(MetricActiveSession, "内存中活跃会话数")
	Describe(MetricCBState, "熔断器状态（0=closed 1=half-open 2=open）")
	Describe(MetricCBEvents, "熔断器状态迁移次数")
	Describe(MetricCBRejected, "因熔断被快速拒绝的次数")
	Describe(MetricAgentBudgetExceeded, "Agent 预算耗尽次数（steps/tokens/wallclock）")
	Describe(MetricAgentToolRetry, "Agent 工具重试次数与结果")
	Describe(MetricAgentDegraded, "Agent 降级次数（重试/备用模型/纯对话/错误）")
	Describe(MetricAgentTimeout, "Agent 超时次数（工具/模型/整轮）")

	// 预置 0 值序列
	SetGauge(MetricActiveSession, nil, 0)
	Count(MetricAIRequests, Labels{"model": "", "model_type": "", "status": "none", "source": "none"}, 0)
	Count(MetricAICacheHits, Labels{"result": "none"}, 0)
	for _, reason := range []string{"steps", "tokens", "wallclock"} {
		Count(MetricAgentBudgetExceeded, Labels{"reason": reason}, 0)
	}
	Count(MetricAgentToolRetry, Labels{"tool": "none", "result": "none"}, 0)
	for _, level := range []string{"retry", "fallback_model", "plain_chat", "error"} {
		Count(MetricAgentDegraded, Labels{"level": level}, 0)
	}
	for _, kind := range []string{"tool", "model", "turn"} {
		Count(MetricAgentTimeout, Labels{"kind": kind}, 0)
	}
	// 直方图也要预置分桶，否则无流量时看不到 _bucket/_sum/_count
	EnsureHistogram(MetricAIDuration, Labels{"model": "", "source": "none"})
}
