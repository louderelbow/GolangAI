package session

import (
	"net/http"

	"deeptalk/common/code"
	service "deeptalk/service/session"

	"github.com/gin-gonic/gin"
)

func GetUserSessionsByUserName(c *gin.Context) {
	res := new(GetUserSessionsResponse)
	items, err := service.GetUserSessionsByUserName(c.Request.Context(), c.GetString("userName"))
	if err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeServerBusy))
		return
	}

	res.Success()
	res.Sessions = items
	c.JSON(http.StatusOK, res)
}

func ChatHistory(c *gin.Context) {
	req := new(ChatHistoryRequest)
	res := new(ChatHistoryResponse)
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusOK, res.CodeOf(code.CodeInvalidParams))
		return
	}

	history, resultCode := service.GetChatHistory(c.Request.Context(), c.GetString("userName"), req.SessionID)
	if resultCode != code.CodeSuccess {
		c.JSON(http.StatusOK, res.CodeOf(resultCode))
		return
	}

	res.Success()
	res.History = history
	c.JSON(http.StatusOK, res)
}
