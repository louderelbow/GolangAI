package session

import (
	"context"
	"log"

	"deeptalk/common/code"
	sessiondao "deeptalk/dao/session"
	"deeptalk/model"

	"github.com/google/uuid"
)

func CreateSessionAndSendMessage(ctx context.Context, userName, question, requestedModelType string) (string, string, string, code.Code) {
	created, modelType, resultCode := createSession(userName, question, requestedModelType)
	if resultCode != code.CodeSuccess {
		return "", "", "", resultCode
	}

	helper, resultCode := getHelper(userName, created.ID, modelType)
	if resultCode != code.CodeSuccess {
		return "", "", "", resultCode
	}
	response, err := helper.GenerateResponse(ctx, userName, question)
	if err != nil {
		log.Printf("[session] generate first response: %v", err)
		return "", "", "", mapAIError(err)
	}
	return created.ID, response.Content, modelType, code.CodeSuccess
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
