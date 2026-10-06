package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ======================== 轻量指标注册表 ========================
//
// 用法：
//   metrics.Count("deeptalk_ai_requests_total", metrics.Labels{"model": "deepseek-chat", "status": "ok"}, 1)
//   metrics.Observe("deeptalk_ai_request_duration_seconds", metrics.Labels{"model": "..."}, seconds)

type Labels map[string]string

type series struct {
	name   string
	labels string // 已渲染好的 {k="v",...}（含大括号，无标签时为空串）
	value  float64
}

type histogram struct {
	name    string
	labels  string
	buckets []float64
	counts  []float64
	sum     float64
	count   float64
}

type Registry struct {
	mu         sync.Mutex
	counters   map[string]*series
	gauges     map[string]*series
	histograms map[string]*histogram

	help map[string]string
	typ  map[string]string
}

var defaultBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60}

func NewRegistry() *Registry {
	return &Registry{
		counters:   make(map[string]*series),
		gauges:     make(map[string]*series),
		histograms: make(map[string]*histogram),
		help:       make(map[string]string),
		typ:        make(map[string]string),
	}
}

var defaultRegistry = NewRegistry()

// Default 返回默认注册表
func Default() *Registry { return defaultRegistry }

func renderLabels(l Labels) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := strings.ReplaceAll(l[k], `\`, `\\`)
		v = strings.ReplaceAll(v, `"`, `\"`)
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func seriesKey(name, labels string) string { return name + labels }

// Describe 登记指标的 HELP/TYPE（可选，只为输出更好看）
func (r *Registry) Describe(name, help, typ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.help[name] = help
	r.typ[name] = typ
}

// Count 计数器累加
func (r *Registry) Count(name string, labels Labels, delta float64) {
	key := seriesKey(name, renderLabels(labels))
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.counters[key]
	if !ok {
		s = &series{name: name, labels: renderLabels(labels)}
		r.counters[key] = s
		if _, exists := r.typ[name]; !exists {
			r.typ[name] = "counter"
		}
	}
	s.value += delta
}

// SetGauge 设置瞬时值
func (r *Registry) SetGauge(name string, labels Labels, value float64) {
	key := seriesKey(name, renderLabels(labels))
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.gauges[key]
	if !ok {
		s = &series{name: name, labels: renderLabels(labels)}
		r.gauges[key] = s
		if _, exists := r.typ[name]; !exists {
			r.typ[name] = "gauge"
		}
	}
	s.value = value
}

// Observe 观测值（用于直方图，如耗时）
func (r *Registry) Observe(name string, labels Labels, v float64) {
	key := seriesKey(name, renderLabels(labels))
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.histograms[key]
	if !ok {
		h = &histogram{
			name:    name,
			labels:  renderLabels(labels),
			buckets: defaultBuckets,
			counts:  make([]float64, len(defaultBuckets)),
		}
		r.histograms[key] = h
		if _, exists := r.typ[name]; !exists {
			r.typ[name] = "histogram"
		}
	}
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
		}
	}
	h.sum += v
	h.count++
}

// Snapshot 导出为 Prometheus 文本格式
func (r *Registry) Snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var sb strings.Builder
	written := map[string]bool{}

	writeHeader := func(name string) {
		if written[name] {
			return
		}
		written[name] = true
		if help, ok := r.help[name]; ok {
			fmt.Fprintf(&sb, "# HELP %s %s\n", name, help)
		}
		if typ, ok := r.typ[name]; ok {
			fmt.Fprintf(&sb, "# TYPE %s %s\n", name, typ)
		}
	}

	nameSet := map[string]struct{}{}
	for _, s := range r.counters {
		nameSet[s.name] = struct{}{}
	}
	for _, s := range r.gauges {
		nameSet[s.name] = struct{}{}
	}
	for _, h := range r.histograms {
		nameSet[h.name] = struct{}{}
	}
	for name := range r.help {
		nameSet[name] = struct{}{}
	}
	for name := range r.typ {
		nameSet[name] = struct{}{}
	}

	names := make([]string, 0, len(nameSet))
	for n := range nameSet {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		writeHeader(n)
	}

	emit := func(items map[string]*series) {
		keys := make([]string, 0, len(items))
		for k := range items {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := items[k]
			fmt.Fprintf(&sb, "%s%s %g\n", s.name, s.labels, s.value)
		}
	}
	emit(r.counters)
	emit(r.gauges)

	hkeys := make([]string, 0, len(r.histograms))
	for k := range r.histograms {
		hkeys = append(hkeys, k)
	}
	sort.Strings(hkeys)
	for _, k := range hkeys {
		h := r.histograms[k]
		for i, b := range h.buckets {
			fmt.Fprintf(&sb, "%s_bucket%s %g\n", h.name, mergeLabel(h.labels, "le", fmt.Sprintf("%g", b)), h.counts[i])
		}
		fmt.Fprintf(&sb, "%s_bucket%s %g\n", h.name, mergeLabel(h.labels, "le", "+Inf"), h.count)
		fmt.Fprintf(&sb, "%s_sum%s %g\n", h.name, h.labels, h.sum)
		fmt.Fprintf(&sb, "%s_count%s %g\n", h.name, h.labels, h.count)
	}

	return sb.String()
}

func mergeLabel(labels, key, value string) string {
	if labels == "" {
		return fmt.Sprintf(`{%s="%s"}`, key, value)
	}
	return labels[:len(labels)-1] + fmt.Sprintf(`,%s="%s"}`, key, value)
}

// Handler 暴露 /metrics（Prometheus 文本格式）
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(defaultRegistry.Snapshot()))
	}
}

// ======================== AI 业务指标 ========================

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
)

var cbStateValue = map[string]float64{"closed": 0, "half-open": 1, "open": 2}

// SetCircuitState 记录熔断器当前状态（供 /metrics 暴露）
func SetCircuitState(name, state string) {
	v, ok := cbStateValue[state]
	if !ok {
		v = -1
	}
	defaultRegistry.SetGauge(MetricCBState, Labels{"name": name}, v)
}

// CountCircuitEvent 记录一次状态迁移
func CountCircuitEvent(name, to string) {
	defaultRegistry.Count(MetricCBEvents, Labels{"name": name, "to": to}, 1)
}

// CountCircuitRejected 记录一次"因熔断被快速拒绝"
func CountCircuitRejected(name string) {
	defaultRegistry.Count(MetricCBRejected, Labels{"name": name}, 1)
}

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

// RecordAIRequest 记录一次 AI 请求（token、费用、延迟、状态）
func RecordAIRequest(req AIRequest) {
	r := defaultRegistry
	modelLabel := Labels{"model": req.Model, "model_type": req.ModelType, "status": req.Status, "source": req.Source}
	r.Count(MetricAIRequests, modelLabel, 1)

	if req.PromptTokens > 0 {
		r.Count(MetricAITokens, Labels{"model": req.Model, "kind": "prompt"}, float64(req.PromptTokens))
	}
	if req.CompletionTokens > 0 {
		r.Count(MetricAITokens, Labels{"model": req.Model, "kind": "completion"}, float64(req.CompletionTokens))
	}
	if req.CachedTokens > 0 {
		// 命中上游前缀缓存的 token 数（省下来的钱在这里体现）
		r.Count(MetricAITokens, Labels{"model": req.Model, "kind": "cached"}, float64(req.CachedTokens))
	}
	if req.CostMicros > 0 {
		r.Count(MetricAICostMicros, Labels{"model": req.Model}, float64(req.CostMicros))
	}
	r.Observe(MetricAIDuration, Labels{"model": req.Model, "source": req.Source}, req.Latency.Seconds())
}

// SetGauge 设置瞬时值（包级便捷函数）
func SetGauge(name string, labels Labels, value float64) {
	defaultRegistry.SetGauge(name, labels, value)
}

// EnsureHistogram 预创建直方图序列（不记录观测值）
// 让 /metrics 在没有流量时也能看到完整分桶，而不是只有 HELP/TYPE 空壳
func EnsureHistogram(name string, labels Labels) {
	defaultRegistry.EnsureHistogram(name, labels)
}

// EnsureHistogram 预创建直方图序列（count/sum 均为 0，语义正确）
func (r *Registry) EnsureHistogram(name string, labels Labels) {
	key := seriesKey(name, renderLabels(labels))
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.histograms[key]; ok {
		return
	}
	r.histograms[key] = &histogram{
		name:    name,
		labels:  renderLabels(labels),
		buckets: defaultBuckets,
		counts:  make([]float64, len(defaultBuckets)),
	}
	if _, exists := r.typ[name]; !exists {
		r.typ[name] = "histogram"
	}
}

// Count 计数器累加（包级便捷函数）
func Count(name string, labels Labels, delta float64) {
	defaultRegistry.Count(name, labels, delta)
}

// Observe 观测值（包级便捷函数）
func Observe(name string, labels Labels, v float64) {
	defaultRegistry.Observe(name, labels, v)
}

// RecordCacheLookup 记录语义缓存查询结果
func RecordCacheLookup(hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}
	defaultRegistry.Count(MetricAICacheHits, Labels{"result": result}, 1)
}

// RegisterHelp 初始化指标说明（启动时调用一次即可）
func RegisterHelp() {
	r := defaultRegistry
	r.Describe(MetricAIRequests, "AI 请求总数", "counter")
	r.Describe(MetricAITokens, "AI token 消耗（按 prompt/completion/cached 分类）", "counter")
	r.Describe(MetricAICostMicros, "AI 费用累计（微元，1 元 = 1e6）", "counter")
	r.Describe(MetricAIDuration, "AI 请求耗时分布", "histogram")
	r.Describe(MetricAICacheHits, "语义缓存命中/未命中次数", "counter")
	r.Describe(MetricActiveSession, "内存中活跃会话数", "gauge")

	// 预置 0 值数据点，保证 /metrics 从第一次抓取就能看到全部指标
	r.SetGauge(MetricActiveSession, nil, 0)
	r.Count(MetricAIRequests, Labels{"model": "", "model_type": "", "status": "none", "source": "none"}, 0)
	r.Count(MetricAICacheHits, Labels{"result": "none"}, 0)
	// 直方图也要预置分桶，否则无流量时只有 HELP/TYPE、看不到 _bucket/_sum/_count
	r.EnsureHistogram(MetricAIDuration, Labels{"model": "", "source": "none"})
}
