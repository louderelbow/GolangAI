package session

import (
	"net/http"

	"deeptalk/common/code"
	"deeptalk/internal/agent/askuser"
	service "deeptalk/service/session"

	"github.com/gin-gonic/gin"
)

func CreateSessionAndSendMessage(c *gin.Context) {
	req := new(CreateSessionAndSendMessageRequest)
	res := new(CreateSessionAndSendMessageResponse)
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
		return
	}

	result, resultCode := service.CreateSessionAndSendMessage(
		c.Request.Context(), c.GetString("userName"), req.UserQuestion, req.ModelType,
	)

	// 澄清不是失败：用同一个响应结构 + 专用状态码告诉前端"该弹选择框了"
	if resultCode == code.CodeNeedClarify {
		res.CodeOf(code.CodeNeedClarify)
		res.SessionID = result.SessionID
		res.ModelType = result.ModelType
		res.Clarify = clarifyPayload(result.Clarify)
		c.JSON(http.StatusOK, res)
		return
	}
	if resultCode != code.CodeSuccess {
		c.JSON(http.StatusOK, res.CodeOf(resultCode))
		return
	}

	res.Success()
	res.AiInformation = result.Answer
	res.SessionID = result.SessionID
	res.ModelType = result.ModelType
	c.JSON(http.StatusOK, res)
}

func ChatSend(c *gin.Context) {
	req := new(ChatSendRequest)
	res := new(ChatSendResponse)
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
		return
	}

	answer, clarify, resultCode := service.ChatSend(
		c.Request.Context(), c.GetString("userName"), req.SessionID, req.UserQuestion, req.ModelType,
		clarifyContext(req.ClarifyAnswer),
	)
	if resultCode == code.CodeNeedClarify {
		res.CodeOf(code.CodeNeedClarify)
		res.Clarify = clarifyPayload(clarify)
		c.JSON(http.StatusOK, res)
		return
	}
	if resultCode != code.CodeSuccess {
		c.JSON(http.StatusOK, res.CodeOf(resultCode))
		return
	}

	res.Success()
	res.AiInformation = answer
	c.JSON(http.StatusOK, res)
}

// clarifyContext 把请求里的澄清回答转成 service 层的入参。
// 两者字段一样但分属不同层：service 不该反向依赖 controller 的 DTO。
func clarifyContext(a *ClarifyAnswer) *service.ClarifyContext {
	if a == nil {
		return nil
	}
	return &service.ClarifyContext{Question: a.Question, Label: a.Label}
}

// clarifyPayload 把 Agent 抛出的澄清请求转成对外的响应结构。
func clarifyPayload(req *askuser.Request) *ClarifyPayload {
	if req == nil {
		return nil
	}
	out := &ClarifyPayload{
		Question: req.Question,
		Reason:   req.Reason,
		Options:  make([]ClarifyOption, 0, len(req.Options)),
	}
	for _, o := range req.Options {
		out.Options = append(out.Options, ClarifyOption{ID: o.ID, Label: o.Label, Hint: o.Hint})
	}
	return out
}
