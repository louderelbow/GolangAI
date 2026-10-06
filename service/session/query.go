package session

import (
	"context"
	"log"

	"deeptalk/common/code"
	"deeptalk/dao/message"
	sessiondao "deeptalk/dao/session"
	"deeptalk/internal/llm"
	"deeptalk/model"
	"deeptalk/service/chat"
)

func GetUserSessionsByUserName(_ context.Context, userName string) ([]model.SessionInfo, error) {
	sessions, err := sessiondao.GetSessionsByUserName(userName)
	if err != nil {
		return nil, err
	}
	items := make([]model.SessionInfo, 0, len(sessions))
	for _, stored := range sessions {
		modelType := stored.ModelType
		if modelType == "" {
			modelType = llm.DefaultModelType
		}
		items = append(items, model.SessionInfo{SessionID: stored.ID, Title: stored.Title, ModelType: modelType})
	}
	return items, nil
}

func GetChatHistory(_ context.Context, userName, sessionID string) ([]model.History, code.Code) {
	modelType, resultCode := resolveBoundModelType(userName, sessionID)
	if resultCode != code.CodeSuccess {
		return nil, resultCode
	}

	manager := chat.GetGlobalManager()
	if helper, exists := manager.GetAIHelper(userName, sessionID); exists {
		return toHistory(helper.GetMessages()), code.CodeSuccess
	}

	messages, err := message.GetMessagesBySessionID(sessionID)
	if err != nil {
		log.Printf("[session] load history session=%s: %v", sessionID, err)
		return nil, code.CodeServerBusy
	}
	if len(messages) == 0 {
		if _, resultCode = getHelper(userName, sessionID, modelType); resultCode != code.CodeSuccess {
			return nil, resultCode
		}
	}
	items := make([]*model.Message, 0, len(messages))
	for i := range messages {
		items = append(items, &messages[i])
	}
	return toHistory(items), code.CodeSuccess
}

func toHistory(messages []*model.Message) []model.History {
	history := make([]model.History, 0, len(messages))
	for _, item := range messages {
		history = append(history, model.History{IsUser: item.IsUser, Content: item.Content})
	}
	return history
}
