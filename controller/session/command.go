package session

import (
	"net/http"

	"deeptalk/common/code"
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

	sessionID, answer, modelType, resultCode := service.CreateSessionAndSendMessage(
		c.Request.Context(), c.GetString("userName"), req.UserQuestion, req.ModelType,
	)
	if resultCode != code.CodeSuccess {
		c.JSON(http.StatusOK, res.CodeOf(resultCode))
		return
	}

	res.Success()
	res.AiInformation = answer
	res.SessionID = sessionID
	res.ModelType = modelType
	c.JSON(http.StatusOK, res)
}

func ChatSend(c *gin.Context) {
	req := new(ChatSendRequest)
	res := new(ChatSendResponse)
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
		return
	}

	answer, resultCode := service.ChatSend(
		c.Request.Context(), c.GetString("userName"), req.SessionID, req.UserQuestion, req.ModelType,
	)
	if resultCode != code.CodeSuccess {
		c.JSON(http.StatusOK, res.CodeOf(resultCode))
		return
	}

	res.Success()
	res.AiInformation = answer
	c.JSON(http.StatusOK, res)
}
