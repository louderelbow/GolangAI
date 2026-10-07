package session

import (
	"context"
	"log"

	"deeptalk/common/code"
	"deeptalk/internal/agent/askuser"
)

// ChatSend 处理一次对话。
//
// 返回值里的 *askuser.Request 非 nil 表示**本轮 Agent 选择反问用户**：
// 这时 answer 为空，且 resultCode 为 CodeNeedClarify——它不是失败，
// 而是"等你补充信息再继续"。
func ChatSend(
	ctx context.Context,
	userName, sessionID, question, requestedModelType string,
	cl *ClarifyContext,
) (answer string, clarify *askuser.Request, resultCode code.Code) {
	modelType, resultCode := resolveBoundModelType(userName, sessionID)
	if resultCode != code.CodeSuccess {
		return "", nil, resultCode
	}
	logIgnoredModelType(requestedModelType, modelType, sessionID)

	helper, resultCode := getHelper(userName, sessionID, modelType)
	if resultCode != code.CodeSuccess {
		return "", nil, resultCode
	}

	response, err := helper.GenerateResponse(ctx, userName, augmentQuestion(question, cl))

	// 澄清请求优先于错误判断：Agent 是靠"让 ask_user 工具报错"来中断本轮的，
	// 先看错误会把一次正常的反问误报成模型失败。
	if req := helper.TakeClarify(); req != nil {
		return "", req, code.CodeNeedClarify
	}
	if err != nil {
		log.Printf("[session] generate response: %v", err)
		return "", nil, mapAIError(err)
	}
	return response.Content, nil, code.CodeSuccess
}

func logIgnoredModelType(requested, bound, sessionID string) {
	if requested != "" && requested != bound {
		log.Printf("[session] ignore modelType=%s, session=%s is bound to %s", requested, sessionID, bound)
	}
}
