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
	AiInformation string          `json:"Information,omitempty"`
	SessionID     string          `json:"sessionId,omitempty"`
	ModelType     string          `json:"modelType,omitempty"`
	Clarify       *ClarifyPayload `json:"clarify,omitempty"`
	Warnings      []string        `json:"warnings,omitempty"`
	controller.Response
}

// ======================== 澄清式追问 ========================
//
// 这是**新增**字段，不改动任何已有字段：
// 旧客户端不认识 clarify / clarifyAnswer，会忽略它们，
// 只是拿不到澄清能力，不会因此报错。

// ClarifyOption 澄清提问的一个可选项。
type ClarifyOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
}

// ClarifyPayload 澄清提问：模型认为信息不足时返回，
// 由前端弹出选择框让用户挑一个。
type ClarifyPayload struct {
	Question string          `json:"question"`
	Options  []ClarifyOption `json:"options"`
	Reason   string          `json:"reason,omitempty"`
}

// Warnings 是本轮的工具告警（工具失败 / 被熔断）。
//
// 它不是错误：工具失败已被回填成 observation，本轮照常有回答，
// 前端只该弹一个轻提示（"这次的回答可能不完整"），不要渲染成失败。
// 与 clarify 一样是**新增可选字段**，旧客户端忽略即可。

// ClarifyAnswer 用户对澄清提问的回答，随下一轮请求回传。
//
// 让前端把原问题一起带回来（而不是靠服务端记状态）：
// 服务端就不必为"未完成的提问"维护会话级状态，
// 多实例部署、页面刷新都不会丢。
type ClarifyAnswer struct {
	Question string `json:"question"` // 当时问的是什么
	Label    string `json:"label"`    // 用户选的那一项；Custom=true 时是用户自己输入的原文
	// Custom 表示 Label 是用户自己敲的，而不是点选的某个选项。
	//
	// 只影响拼进上下文时的措辞（"选择了" vs "补充说明"），
	// 服务端不据此改变任何行为。旧客户端不传即为 false，行为不变。
	Custom bool `json:"custom,omitempty"`
}

type ChatSendRequest struct {
	UserQuestion  string         `json:"question" binding:"required,max=4000"`
	ModelType     string         `json:"modelType" binding:"omitempty,max=8"`
	SessionID     string         `json:"sessionId" binding:"required,max=64"`
	ClarifyAnswer *ClarifyAnswer `json:"clarifyAnswer,omitempty"`
}

type ChatSendResponse struct {
	AiInformation string          `json:"Information,omitempty"`
	Clarify       *ClarifyPayload `json:"clarify,omitempty"`
	Warnings      []string        `json:"warnings,omitempty"`
	controller.Response
}

type ChatHistoryRequest struct {
	SessionID string `json:"sessionId" binding:"required,max=64"`
}

type ChatHistoryResponse struct {
	History []model.History `json:"history"`
	controller.Response
}
