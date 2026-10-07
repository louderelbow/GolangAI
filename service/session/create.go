package session

import (
	"context"
	"log"

	"deeptalk/common/code"
	sessiondao "deeptalk/dao/session"
	"deeptalk/internal/agent/askuser"
	"deeptalk/model"

	"github.com/google/uuid"
)

// FirstTurnResult 新建会话并跑完第一轮的结果。
//
// 用结构体而不是继续堆返回值：本来就已经是四元组了，
// 再加澄清请求会变成五元组，调用方要数着位置读，很容易接错。
type FirstTurnResult struct {
	SessionID string
	Answer    string
	ModelType string
	// Clarify 非 nil 表示第一轮 Agent 就选择了反问用户（新会话同样可能缺信息）
	Clarify *askuser.Request
}

func CreateSessionAndSendMessage(ctx context.Context, userName, question, requestedModelType string) (FirstTurnResult, code.Code) {
	created, modelType, resultCode := createSession(userName, question, requestedModelType)
	if resultCode != code.CodeSuccess {
		return FirstTurnResult{}, resultCode
	}

	helper, resultCode := getHelper(userName, created.ID, modelType)
	if resultCode != code.CodeSuccess {
		return FirstTurnResult{}, resultCode
	}

	// 新会话不可能带"上一轮的澄清回答"，所以这里不需要 ClarifyContext
	response, err := helper.GenerateResponse(ctx, userName, question)

	// 澄清请求优先于错误（同 ChatSend）
	if req := helper.TakeClarify(); req != nil {
		return FirstTurnResult{SessionID: created.ID, ModelType: modelType, Clarify: req}, code.CodeNeedClarify
	}
	if err != nil {
		log.Printf("[session] generate first response: %v", err)
		return FirstTurnResult{}, mapAIError(err)
	}
	return FirstTurnResult{SessionID: created.ID, Answer: response.Content, ModelType: modelType}, code.CodeSuccess
}

func CreateStreamSessionOnly(_ context.Context, userName, question, requestedModelType string) (string, string, code.Code) {
	created, modelType, resultCode := createSession(userName, question, requestedModelType)
	if resultCode != code.CodeSuccess {
		return "", "", resultCode
	}
	return created.ID, modelType, code.CodeSuccess
}

func createSession(userName, question, requestedModelType string) (*model.Session, string, code.Code) {
	modelType, ok := normalizeModelType(requestedModelType)
	if !ok {
		return nil, "", code.CodeInvalidParams
	}
	created, err := sessiondao.CreateSession(&model.Session{
		ID:        uuid.New().String(),
		UserName:  userName,
		Title:     question,
		ModelType: modelType,
	})
	if err != nil {
		log.Printf("[session] create session: %v", err)
		return nil, "", code.CodeServerBusy
	}
	return created, modelType, code.CodeSuccess
}
