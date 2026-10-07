package trace

import (
	"context"
	"errors"
)

// Turn 一轮对话的追踪句柄。
//
// 存在的意义是把"什么时候算一轮"这件事收口：Recorder 只管记，
// 由 StartTurn / Finish 这一对决定边界。服务层的两条路径
// （GenerateResponse / StreamResponse）各自 defer 一次 Finish 即可，
// 不必关心轨迹怎么落库、指标怎么导出。
type Turn struct {
	rec  *Recorder
	span *Span
}

// StartTurn 开启一轮追踪，返回带记录器的 ctx 与该轮的句柄。
//
// 返回新的 ctx 而不是让调用方自己 WithRecorder：进入之后再想补放进 ctx，
// 下层（模型包装器、工具执行器）早就拿到旧的 ctx 了——那正是"轨迹里
// 工具步骤总是空的"这类问题的根源。
func StartTurn(ctx context.Context, sessionID, user, model, modelType string) (context.Context, *Turn) {
	rec := New(sessionID, user, model, modelType)
	return WithRecorder(ctx, rec), &Turn{
		rec:  rec,
		span: rec.Begin(KindTurn, "turn"),
	}
}

// Finish 结束本轮：收尾 turn 步骤，然后导出指标 + 落库。
//
// 幂等（Span.End 只生效一次），所以调用方可以放心 defer。
func (t *Turn) Finish(ctx context.Context, status Status) {
	if t == nil || t.rec == nil {
		return
	}
	t.span.End(status, "")
	Persist(ctx, t.rec.Snapshot(status))
}

// ID 本轮轨迹 ID。
func (t *Turn) ID() string {
	if t == nil {
		return ""
	}
	return t.rec.ID()
}

// StatusOf 把一次执行的错误映射成轨迹状态。
//
// 映射规则集中在这里，是为了让"什么算超时、什么算取消"只有一份判断。
// 各调用点自行判断的话，指标里迟早混进两种标准，
// 而那种偏差不会报错，只会让看板慢慢地不准。
func StatusOf(err error) Status {
	switch {
	case err == nil:
		return StatusOK
	case errors.Is(err, context.DeadlineExceeded):
		return StatusTimeout
	case errors.Is(err, context.Canceled):
		return StatusSkipped
	default:
		return StatusError
	}
}

// Clarify 在 ctx 上记一次澄清追问。outcome 取 asked / answered / abandoned。
//
// 用 Name 传结局（而不是 Status），因为"问了用户"这一动作本身是成功的，
// 它有意义的信息是结果分类。这样 asked 与 answered 的差值就直接是
// "问了但没答"的流失率。
func Clarify(ctx context.Context, outcome string) {
	sp := Begin(ctx, KindClarify, outcome)
	sp.EndOK("")
}

// Cache 在 ctx 上记一次缓存命中。
func Cache(ctx context.Context, level string) {
	sp := Begin(ctx, KindCache, level)
	sp.EndOK("")
}
