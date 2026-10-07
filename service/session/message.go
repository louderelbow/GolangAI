package session

import (
	"context"
	"log"

	"deeptalk/common/code"
	"deeptalk/internal/agent/askuser"
	"deeptalk/internal/infra/metrics"
)

// ChatSend 处理一次对话。
//
// 返回值里的 *askuser.Request 非 nil 表示**本轮 Agent 选择反问用户**：
// 这时 answer 为空，且 resultCode 为 CodeNeedClarify——它不是失败，
// 而是"等你补充信息再继续"。
//
// warnings 是本轮的工具告警（工具失败 / 被熔断）。它不是失败：
// 工具错误已被回填成 observation 让本轮继续，这里只是让用户知道
// "这次的回答可能不完整"。失败、澄清、成功三条路径都会带上它。
func ChatSend(
	ctx context.Context,
	userName, sessionID, question, requestedModelType string,
	cl *ClarifyContext,
) (answer string, clarify *askuser.Request, warnings []string, resultCode code.Code) {
	modelType, resultCode := resolveBoundModelType(userName, sessionID)
	if resultCode != code.CodeSuccess {
		return "", nil, nil, resultCode
	}
	logIgnoredModelType(requestedModelType, modelType, sessionID)

	helper, resultCode := getHelper(userName, sessionID, modelType)
	if resultCode != code.CodeSuccess {
		return "", nil, nil, resultCode
	}

	// 澄清的"答"与"问"跨越两轮，没法落在同一条轨迹里，所以这里直接记指标。
	// 也就是这一处允许绕过 trace.Flush 的口径收口：
	// asked 在上一轮、answered 在这一轮，而 asked - answered 正是流失率。
	if cl != nil {
		metrics.CountAgentClarify("answered")
	}

	response, err := helper.GenerateResponse(ctx, userName, augmentQuestion(question, cl))

	// 两个"本轮附加信息"都必须取走，否则会漏到下一轮：
	// 澄清漏了会把下一次普通回答误判成又需要澄清，告警漏了会让用户看到过期提示。
	clarifyReq := helper.TakeClarify()
	warnings = helper.TakeWarnings()

	// 澄清请求优先于错误判断：Agent 是靠"让 ask_user 工具报错"来中断本轮的，
	// 先看错误会把一次正常的反问误报成模型失败。
	if clarifyReq != nil {
		return "", clarifyReq, warnings, code.CodeNeedClarify
	}
	if err != nil {
		log.Printf("[session] generate response: %v", err)
		return "", nil, warnings, mapAIError(err)
	}
	return response.Content, nil, warnings, code.CodeSuccess
}

func logIgnoredModelType(requested, bound, sessionID string) {
	if requested != "" && requested != bound {
		log.Printf("[session] ignore modelType=%s, session=%s is bound to %s", requested, sessionID, bound)
	}
}
