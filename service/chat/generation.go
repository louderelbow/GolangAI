package chat

import (
	"context"
	"time"

	"deeptalk/internal/agent/askuser"
	agenttool "deeptalk/internal/agent/tool"
	"deeptalk/internal/agent/trace"
	cachepkg "deeptalk/internal/cache"
	"deeptalk/internal/inference"
	"deeptalk/internal/infra/config"
	"deeptalk/internal/infra/metrics"
	"deeptalk/internal/infra/resilience"
	llmpkg "deeptalk/internal/llm"
	"deeptalk/model"

	"github.com/cloudwego/eino/schema"
)

// loopGuardThreshold 读取循环检测阈值。
//
// 负数表示关闭 —— 排查"某个工具确实需要重复调用"时用得上，
// 关掉比调大阈值更明确（调大只是把问题推后）。
func loopGuardThreshold() int {
	n := config.GetConfig().GetAgent().LoopGuardThreshold
	if n < 0 {
		return 1 << 30 // 实际上永不触发
	}
	return n
}

// warningMessages 把本轮的工具告警收敛成给用户看的一句话列表。
func warningMessages(ws []agenttool.Warn) []string {
	if len(ws) == 0 {
		return nil
	}
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Message)
	}
	return out
}

// 同步生成
func (a *AIHelper) GenerateResponse(ctx context.Context, userName string, userQuestion string) (msg *model.Message, err error) {
	// 同一会话同一时刻只处理一轮对话
	a.turn.Lock()
	defer a.turn.Unlock()
	a.touch()

	// 澄清式追问：本轮可能以"Agent 反问用户"收尾而不是给出答案。
	// 收集器的生命周期与本轮严格一致——在这里放进 ctx，在本轮结束前取走。
	ctx = askuser.WithCollector(ctx, askuser.NewCollector())
	// 工具告警同理：工具失败会被回填成 observation 让本轮继续，
	// 但用户有权知道"这次的回答可能不完整"，所以也要按轮收集。
	ctx = agenttool.WithWarnings(ctx, agenttool.NewWarnCollector())
	// 循环检测也按轮：同一个工具 + 同样的参数反复成功，说明模型在空转，
	// 拦下来并给它出路（详见 loopguard.go）。生命周期与上一行完全一致。
	ctx = agenttool.WithLoopGuard(ctx, agenttool.NewLoopGuard(loopGuardThreshold()))

	// 优先级：**用户在屏幕前等着这一次回答**，所以标 High。
	//
	// 判据只有一条：**失败的代价越大，优先级越高**。
	// 主回答失败 = 用户什么都拿不到；而摘要压缩、意图兜底失败了都有降级路径。
	// 池子紧张时，后者应当先让路（见 assistant.go 的压缩处与 rag/rewrite.go）。
	ctx = inference.WithPriority(ctx, inference.PriorityHigh)

	//调用存储函数
	a.AddMessage(userQuestion, userName, true, true)

	// 记忆压缩检查（锁外调用 LLM）
	a.compressIfNeeded(ctx)

	modelName := a.model.GetModelName()
	modelType := a.GetModelType()

	// 链路追踪：和上面两个收集器同一套路，生命周期严格等于一轮。
	//
	// 收尾放在 defer 里而不是散在几个 return 前面——因为轨迹最需要的
	// 恰恰是**失败的那些轮**，而失败路径往往是在写代码时最容易被漏掉的那条。
	ctx, turn := trace.StartTurn(ctx, a.SessionID, userName, modelName, modelType)
	override := trace.Status("")
	defer func() {
		st := trace.StatusOf(err)
		if override != "" {
			st = override
		}
		turn.Finish(ctx, st)
	}()

	// 缓存命名空间必须带用户名：RAG 的答案取决于该用户自己的文档，
	// 不带用户就是跨租户串答案（详见 cache.Key 的注释）。
	cacheKey := cachepkg.Key{User: userName, Model: modelType + "|" + modelName}
	start := time.Now()

	// 配额预检：超额直接拒绝，避免继续烧 token
	if ok, used, limit := metrics.CheckQuota(userName, 0); !ok {
		metrics.RecordAIRequest(metrics.AIRequest{Model: modelName, ModelType: modelType, User: userName,
			Status: "quota_exceeded", Source: "llm", Latency: time.Since(start)})
		// 配额拦截是策略，不是故障：标 rejected，否则"被限流"和"服务坏了"
		// 在成功率指标上长得一样。
		override = trace.StatusRejected
		return nil, metrics.QuotaExceeded(used, limit)
	}

	// 缓存只在"首轮提问"生效：有历史上下文时，同一个问题的正确答案会不同
	//（"它多少钱"在不同上下文里指代不同），命中必然答错。
	firstTurn := a.messageCount() <= 1

	// 回源：真正调用模型，并记录用量与费用
	generate := func() (cachepkg.Source, error) {
		var schemaMsg *schema.Message
		err := resilience.DoErr(resilience.ModelKey(modelName), func() error {
			var e error
			schemaMsg, e = a.model.GenerateResponse(ctx, a.schemaMessages())
			return e
		})
		if err != nil {
			return cachepkg.Source{}, err
		}
		usage := usageOf(schemaMsg)
		a.recordUsage(userName, modelName, modelType, usage, time.Since(start), "ok", "llm")
		return sourceOf(modelName, schemaMsg.Content, usage), nil
	}

	answer := ""
	level := cachepkg.LevelMiss
	var genErr error
	if firstTurn {
		// 多级缓存 + 击穿防护：
		// 同一个 key 的并发回源只真正执行一次，其余请求共享结果——
		// 否则"热点 key 刚失效"会变成"模型被打 N 次"。
		var hit cachepkg.Hit
		hit, genErr = cachepkg.GetCoordinator().Do(ctx, cacheKey, userQuestion, generate)
		answer, level = hit.Answer, hit.Level
	} else {
		var src cachepkg.Source
		src, genErr = generate()
		answer = src.Answer
	}

	// 工具告警要在返回前落到 helper 上：它跟成功/失败/澄清都无关，
	// 是这三条路径都该带出去的信息。
	a.setWarnings(warningMessages(agenttool.WarningsFrom(ctx)))

	// 澄清请求优先于错误：Agent 是靠"让 ask_user 工具报错"来中断本轮的，
	// 所以必须先看收集器，否则一次正常的反问会被当成整轮失败。
	if req := a.finishClarify(ctx, userName, modelType, modelName, start, firstTurn); req != nil {
		return &model.Message{SessionID: a.SessionID, UserName: userName, Content: req.AsText(), IsUser: false}, nil
	}

	if genErr != nil {
		recordAIFailure(userName, modelName, modelType, start, genErr)
		return nil, genErr
	}

	if level != cachepkg.LevelMiss {
		// 命中缓存：这一次没有产生模型调用，来源标成命中的层级
		metrics.RecordAIRequest(metrics.AIRequest{Model: modelName, ModelType: modelType, User: userName,
			Status: "ok", Source: level, Latency: time.Since(start)})
		// 轨迹里补一步：为什么这轮没有模型调用？答案是命中缓存。
		// 少了这一步，看轨迹的人会以为"模型没被调用"是出了故障。
		trace.Cache(ctx, string(level))
	}

	//调用存储函数
	a.AddMessage(answer, userName, false, true)

	return &model.Message{SessionID: a.SessionID, UserName: userName, Content: answer, IsUser: false}, nil
}

// 流式生成
func (a *AIHelper) StreamResponse(ctx context.Context, userName string, cb llmpkg.StreamCallback, userQuestion string) (msg *model.Message, err error) {
	a.turn.Lock()
	defer a.turn.Unlock()
	a.touch()

	// 澄清式追问：流式路径同样支持——Agent 反问时不会有内容流出，
	// controller 会改发一个 clarify 事件，而不是把提问当作正文推送。
	ctx = askuser.WithCollector(ctx, askuser.NewCollector())
	ctx = agenttool.WithWarnings(ctx, agenttool.NewWarnCollector())
	ctx = agenttool.WithLoopGuard(ctx, agenttool.NewLoopGuard(loopGuardThreshold()))
	// 与同步路径同理：流式也有人在屏幕前等
	ctx = inference.WithPriority(ctx, inference.PriorityHigh)

	//调用存储函数
	a.AddMessage(userQuestion, userName, true, true)

	// 记忆压缩检查（锁外调用 LLM）
	a.compressIfNeeded(ctx)

	modelName := a.model.GetModelName()
	modelType := a.GetModelType()

	// 与同步路径同一套追踪。两条路径都要有，否则指标只覆盖一半流量——
	// 而前端默认就是流式，漏掉它等于没有指标。
	ctx, turn := trace.StartTurn(ctx, a.SessionID, userName, modelName, modelType)
	override := trace.Status("")
	defer func() {
		st := trace.StatusOf(err)
		if override != "" {
			st = override
		}
		turn.Finish(ctx, st)
	}()

	// 缓存命名空间必须带用户名：RAG 的答案取决于该用户自己的文档，
	// 不带用户就是跨租户串答案（详见 cache.Key 的注释）。
	cacheKey := cachepkg.Key{User: userName, Model: modelType + "|" + modelName}
	start := time.Now()

	if ok, used, limit := metrics.CheckQuota(userName, 0); !ok {
		metrics.RecordAIRequest(metrics.AIRequest{Model: modelName, ModelType: modelType, User: userName,
			Status: "quota_exceeded", Source: "llm"})
		override = trace.StatusRejected
		return nil, metrics.QuotaExceeded(used, limit)
	}

	firstTurn := a.messageCount() <= 1
	if firstTurn {
		// 流式路径只用 Lookup：流是一段一段吐出来的，没法像同步路径那样
		// 用 singleflight 把并发请求合并成一次回源（会互相抢同一个流）。
		if hit := cachepkg.GetCoordinator().Lookup(ctx, cacheKey, userQuestion); hit.OK {
			metrics.RecordAIRequest(metrics.AIRequest{Model: modelName, ModelType: modelType, User: userName,
				Status: "ok", Source: hit.Level, Latency: time.Since(start)})
			trace.Cache(ctx, string(hit.Level))
			// 缓存答案没有"打字机"过程，这里分批吐出，前端体验一致
			streamCachedText(hit.Answer, cb)
			a.AddMessage(hit.Answer, userName, false, true)
			return &model.Message{SessionID: a.SessionID, UserName: userName, Content: hit.Answer, IsUser: false}, nil
		}
	}

	var content string
	var usage *schema.TokenUsage
	// 注意是 = 而不是 :=：err 是具名返回值，跟踪的收尾逻辑靠它判断结局，
	// 用 := 会遮蔽掉外层那个，defer 里看到的就永远是 nil。
	err = resilience.DoErr(resilience.ModelKey(modelName), func() error {
		var e error
		content, usage, e = a.model.StreamResponse(ctx, a.schemaMessages(), cb)
		return e
	})

	a.setWarnings(warningMessages(agenttool.WarningsFrom(ctx)))

	// 澄清请求优先于错误：（与同步路径同理）ask_user 是用工具报错中断本轮的
	if req := a.finishClarify(ctx, userName, modelType, modelName, start, firstTurn); req != nil {
		return &model.Message{SessionID: a.SessionID, UserName: userName, Content: req.AsText(), IsUser: false}, nil
	}

	if err != nil {
		recordAIFailure(userName, modelName, modelType, start, err)
		return nil, err
	}
	a.recordUsage(userName, modelName, modelType, usage, time.Since(start), "ok", "llm")

	//转化成model.Message
	modelMsg := &model.Message{
		SessionID: a.SessionID,
		UserName:  userName,
		Content:   content,
		IsUser:    false,
	}

	//调用存储函数
	a.AddMessage(modelMsg.Content, userName, false, true)

	if firstTurn {
		cachepkg.GetCoordinator().Store(ctx, cacheKey, userQuestion, sourceOf(modelName, content, usage))
	}

	return modelMsg, nil
}
