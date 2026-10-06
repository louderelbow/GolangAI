// Package session 处理会话与对话相关的 HTTP 请求。
package session

import (
	"deeptalk/controller"
	"deeptalk/model"
)

type GetUserSessionsResponse struct {
	controller.Response
	Sessions []model.SessionInfo `json:"sessions,omitempty"`
}

type CreateSessionAndSendMessageRequest struct {
	UserQuestion string `json:"question" binding:"required,max=4000"`
	ModelType    string `json:"modelType" binding:"required,max=8"`
}

type CreateSessionAndSendMessageResponse struct {
	AiInformation string `json:"Information,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
	ModelType     string `json:"modelType,omitempty"`
	controller.Response
}

type ChatSendRequest struct {
	UserQuestion string `json:"question" binding:"required,max=4000"`
	ModelType    string `json:"modelType" binding:"omitempty,max=8"`
	SessionID    string `json:"sessionId" binding:"required,max=64"`
}

type ChatSendResponse struct {
	AiInformation string `json:"Information,omitempty"`
	controller.Response
}

type ChatHistoryRequest struct {
	SessionID string `json:"sessionId" binding:"required,max=64"`
}

type ChatHistoryResponse struct {
	History []model.History `json:"history"`
	controller.Response
}
