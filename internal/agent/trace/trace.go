// Package trace 记录 Agent 一轮对话的完整执行轨迹。
//
// 为什么需要它：传统后端的 bug 有栈可看，模型的问题没有。用户说"这个回答不对"，
// 你无法从一行日志判断是检索没召回、工具选错、还是提示词没约束住。
// 轨迹把一轮里每一步（模型调用 / 工具调用 / 检索 / 澄清 / 缓存）都记下来，
// 于是"为什么"从猜测变成可查。
//
// 设计上最关键的一点是**一次记录、两个出口**：
//
//	每个模型调用 / 工具调用 → Recorder 记一条 Step
//	                              ↓ 轮结束
//	     ┌────────────────────────┴────────────────────────┐
//	     ▼                                                 ▼
//	Prometheus（步数分布 / 工具成功率 / 澄清触发率…）      MySQL（按 trace_id 回放）
//
// 指标口径集中在 Flush 一处，而不是散在各个调用点。否则加一个维度就要改五处，
// 而且必然出现"某个分支忘了记"的洞——那种洞在仪表盘上表现为数字偏小，
// 没有任何报错，最难发现。
package trace

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Kind 是步骤类型。刻意只有几种：维度太细会让指标失去可比性，
// 也太容易在"到底算 model 还是 tool"上产生分歧。
type Kind string

const (
	KindTurn     Kind = "turn"     // 整轮
	KindModel    Kind = "model"    // 一次模型调用（含工具分支上的）
	KindTool     Kind = "tool"     // 一次工具调用
	KindRetrieve Kind = "retrieve" // 一次检索（RAG 路径）
	KindClarify  Kind = "clarify"  // 澄清提问
	KindCache    Kind = "cache"    // 缓存命中
)

// Status 是步骤结局。
//
// rejected 与 error 必须分开：前者是"按策略正确拒绝了"（路径越界、未授权），
// 后者是"出了意外"。混在一起会让工具成功率失去意义——一个模型反复试探
// 越界路径的会话，看起来会像是"工具大面积故障"。
type Status string

const (
	StatusOK       Status = "ok"
	StatusError    Status = "error"
	StatusTimeout  Status = "timeout"
	StatusRejected Status = "rejected"
	StatusSkipped  Status = "skipped"
)

// Step 轨迹里的一步。
type Step struct {
	Step     int           `json:"step"`
	Kind     Kind          `json:"kind"`
	Name     string        `json:"name"`
	Status   Status        `json:"status"`
	Detail   string        `json:"detail,omitempty"`
	Start    time.Time     `json:"start"`
	Duration time.Duration `json:"duration"`
}

// Trace 一轮对话的完整轨迹。
type Trace struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	User      string    `json:"user"`
	Model     string    `json:"model"`
	ModelType string    `json:"modelType"`
	Start     time.Time `json:"start"`
	Status    Status    `json:"status"`
	Steps     []Step    `json:"steps"`
}

// Duration 整轮耗时；没有 turn 步骤时按首末步骤估算。
func (t Trace) Duration() time.Duration {
	var end time.Time
	for _, s := range t.Steps {
		e := s.Start.Add(s.Duration)
		if e.After(end) {
			end = e
		}
	}
	if end.IsZero() {
		return 0
	}
	return end.Sub(t.Start)
}

// Of 取某一类步骤。
func (t Trace) Of(k Kind) []Step {
	out := make([]Step, 0, len(t.Steps))
	for _, s := range t.Steps {
		if s.Kind == k {
			out = append(out, s)
		}
	}
	return out
}

// ======================== 记录器 ========================

// Recorder 收集**一轮**对话的轨迹。
//
// 生命周期与一轮严格一致：由服务层在开始一轮前创建并放进 ctx，
// 轮结束后 Snapshot 一次交给两个出口。与 askuser.Collector、tool.WarnCollector
// 是同一套路——下层只负责写，上层负责取。
type Recorder struct {
	id        string
	sessionID string
	user      string
	model     string
	modelType string
	start     time.Time

	mu    sync.Mutex
	steps []Step
	next  int
}

// New 创建一轮的记录器。modelType 用来区分 RAG 与 Agent 两条路径的指标。
func New(sessionID, user, model, modelType string) *Recorder {
	return &Recorder{
		id:        uuid.NewString(),
		sessionID: sessionID,
		user:      user,
		model:     model,
		modelType: modelType,
		start:     time.Now(),
	}
}

// ID 本轮轨迹 ID（响应里带给前端，便于按 ID 查轨迹）。
func (r *Recorder) ID() string {
	if r == nil {
		return ""
	}
	return r.id
}

// Span 一个进行中的步骤。用法：
//
//	sp := trace.Begin(ctx, trace.KindTool, "read_file")
//	...
//	sp.End(trace.StatusOK, "12 行")
func (r *Recorder) Begin(k Kind, name string) *Span {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.next++
	n := r.next
	r.mu.Unlock()

	return &Span{rec: r, step: n, kind: k, name: name, start: time.Now()}
}

// Record 直接记一条已经结束的步骤（用于事后补记）。
func (r *Recorder) Record(k Kind, name string, status Status, start time.Time, detail string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.next++
	n := r.next
	r.mu.Unlock()
	r.record(n, k, name, status, start, detail)
}

// record 内部写入。步号由调用方给，因为 Span 在 Begin 时就占了号——
// 否则步号会按**结束**顺序排，而轨迹应当按**开始**顺序读。
func (r *Recorder) record(n int, k Kind, name string, status Status, start time.Time, detail string) {
	r.mu.Lock()
	r.steps = append(r.steps, Step{
		Step: n, Kind: k, Name: name, Status: status,
		Detail: detail, Start: start, Duration: time.Since(start),
	})
	r.mu.Unlock()
}

// Snapshot 取一份完整拷贝，按开始时间排序。
func (r *Recorder) Snapshot(status Status) Trace {
	if r == nil {
		return Trace{}
	}
	r.mu.Lock()
	steps := make([]Step, len(r.steps))
	copy(steps, r.steps)
	r.mu.Unlock()

	sort.SliceStable(steps, func(i, j int) bool {
		if steps[i].Start.Equal(steps[j].Start) {
			return steps[i].Step < steps[j].Step
		}
		return steps[i].Start.Before(steps[j].Start)
	})

	return Trace{
		ID:        r.id,
		SessionID: r.sessionID,
		User:      r.user,
		Model:     r.model,
		ModelType: r.modelType,
		Start:     r.start,
		Status:    status,
		Steps:     steps,
	}
}

// Count 某类步骤的条数（例如"这轮调了几次模型"）。
func (r *Recorder) Count(k Kind) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.steps {
		if s.Kind == k {
			n++
		}
	}
	return n
}

// Span 一次进行中的步骤。对 nil 接收者安全——这样调用点不必到处判空，
// 也让"没有记录器"（例如单元测试）自然退化成什么都不记。
type Span struct {
	rec   *Recorder
	step  int
	kind  Kind
	name  string
	start time.Time
	done  bool
}

// End 结束这一步。重复调用只生效一次（defer 与显式调用并存时不至于记两条）。
func (s *Span) End(status Status, detail string) {
	if s == nil || s.done {
		return
	}
	s.done = true
	s.rec.record(s.step, s.kind, s.name, status, s.start, detail)
}

// EndOK 结束并标记成功。
func (s *Span) EndOK(detail string) { s.End(StatusOK, detail) }

// EndErr 按错误分类结局：超时、主动取消、策略拒绝、其他错误。
//
// 分类在这里收口是为了让所有调用点的口径一致——如果每个调用点自己判断
// "这算不算超时"，指标里迟早会混进两种标准。
func (s *Span) EndErr(err error, detail string) {
	if err == nil {
		s.End(StatusOK, detail)
		return
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		s.End(StatusTimeout, join(detail, err.Error()))
	case errors.Is(err, context.Canceled):
		// 用户断开不算失败，但也不该混进成功率里
		s.End(StatusSkipped, join(detail, "已取消"))
	default:
		s.End(StatusError, join(detail, err.Error()))
	}
}

func join(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "：" + b
	}
}

// ======================== ctx 传递 ========================

type recorderKey struct{}

// WithRecorder 把记录器放进 ctx（由服务层在每轮开始时调用）。
func WithRecorder(ctx context.Context, r *Recorder) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, recorderKey{}, r)
}

// FromContext 取出本轮的记录器。
func FromContext(ctx context.Context) (*Recorder, bool) {
	r, ok := ctx.Value(recorderKey{}).(*Recorder)
	return r, ok && r != nil
}

// Begin 是 Begin 的 ctx 版本，供只拿得到 ctx 的地方使用
// （工具执行器、模型包装器都只拿得到 ctx）。
//
// 返回的 span 可能是 nil；它的 End 系列方法对 nil 安全。
func Begin(ctx context.Context, k Kind, name string) *Span {
	r, ok := FromContext(ctx)
	if !ok {
		return nil
	}
	return r.Begin(k, name)
}
