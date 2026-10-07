package chat

import (
	"context"
	"log"
	"strings"
	"time"

	"deeptalk/internal/agent/askuser"
	"deeptalk/internal/agent/trace"
	"deeptalk/internal/infra/config"
	"deeptalk/internal/infra/metrics"
)

// finishClarify 检查本轮是否以"反问用户"结束。
//
// 返回非 nil 表示本轮没有答案，而是抛出了一个澄清提问。要做三件事：
//
//  1. 把它挂到 helper 上，让上层能区分"这是提问"和"这是回答"
//  2. 把提问本身作为**助手发言**落库。澄清那一轮助手确实说了一句话
//     （就是这句提问），存下来历史才是严格的 user/assistant 交替；
//     否则下一轮带上补充信息后会变成两条连续的 user 消息，
//     部分上游会因此直接报错。
//  3. 记账：这一轮模型被真实调用过（它得先想清楚要不要问），
//     所以算一次请求，但不产生 token 消耗。
func (a *AIHelper) finishClarify(ctx context.Context, userName, modelType, modelName string, start time.Time, firstTurn bool) *askuser.Request {
	collector, ok := askuser.FromContext(ctx)
	if !ok {
		// 没有收集器 = 本轮没开澄清能力，行为与从前完全一致
		return nil
	}
	req, has := collector.Take()
	// 调试开关**优先于**模型：带着固定的问题和选项做可复现的验收。
	// 反过来（模型优先）的话，模型偶尔自己抛一个别的提问，演示就对不上了。
	if forced, ok := forcedClarify(firstTurn); ok {
		req, has = forced, true
	}
	if !has {
		return nil
	}

	a.setClarify(req)
	a.AddMessage(req.AsText(), userName, false, true)

	// 轨迹里记一步。"问了"和"用户答了"分开记，两者的差值就是
	// "问了但用户没理"的流失率——这个数字直接决定澄清功能该不该留。
	trace.Clarify(ctx, "asked")

	metrics.RecordAIRequest(metrics.AIRequest{
		Model:     modelName,
		ModelType: modelType,
		User:      userName,
		Status:    "clarify",
		Source:    "agent",
		Latency:   time.Since(start),
	})
	log.Printf("[AIHelper] ask_user session=%s question=%.40s options=%d",
		a.SessionID, req.Question, len(req.Options))
	return &req
}

// forcedClarify 是**调试开关**：模型没调 ask_user 时，替它抛一个澄清提问。
//
// 配置见 [agent] forceClarifyQuestion / forceClarifyOptions，默认留空即关闭。
// 打开后只在本轮是会话首轮时生效——否则每一轮都会弹，用户永远看不到真正的
// 回答，也就走不完"选完 → 给出答案"的完整链路。
//
// 返回的 Reason 里写明"调试开关"，让弹窗上直接能看出这不是模型问的，
// 免得把调试用的固定提问误当成 Agent 的真实行为。
func forcedClarify(firstTurn bool) (askuser.Request, bool) {
	if !firstTurn {
		return askuser.Request{}, false
	}

	ac := config.GetConfig().GetAgent()
	question := strings.TrimSpace(ac.ForceClarifyQuestion)
	if question == "" {
		return askuser.Request{}, false
	}

	req := askuser.Request{
		Question: question,
		Reason:   "调试开关 forceClarifyQuestion 已打开：这一问不是模型发出的，用于验证前端弹窗",
	}
	for _, label := range ac.ForceClarifyOptions {
		req.Options = append(req.Options, askuser.Option{Label: label})
	}
	req.Normalize()

	if !req.Valid() {
		log.Printf("[AIHelper] forceClarifyQuestion 已打开，但有效选项不足 %d 个，已忽略", askuser.MinOptions)
		return askuser.Request{}, false
	}
	log.Printf("[AIHelper] 调试开关生效：强制抛出澄清 question=%.40q options=%d",
		req.Question, len(req.Options))
	return req, true
}
