// Package trace 暴露 Agent 轨迹的查询接口。
//
// 这些接口是"可观测性"的前半截：Prometheus 告诉你整体在变差，
// 轨迹告诉你**这一轮到底怎么了**。少了后半截，指标只能用来发警报，
// 不能用来定位。
package trace

import (
	"encoding/json"
	"net/http"

	daotrace "deeptalk/dao/trace"
	"deeptalk/model"

	"github.com/gin-gonic/gin"
)

// Get 按 trace_id 取一轮轨迹的完整步骤。
func Get(c *gin.Context) {
	row, err := daotrace.Get(c.Param("id"), c.GetString("userName"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": "轨迹不存在"})
		return
	}
	c.JSON(http.StatusOK, toView(row, true))
}

// List 列出轨迹，用于"最近发生了什么"。
//
// 查询范围永远限定在当前登录用户：轨迹里含会话内容摘要与工具参数，
// 越权读别人的轨迹等同于越权读别人的聊天记录。
func List(c *gin.Context) {
	user := c.GetString("userName")
	sessionID := c.Query("sessionId")

	var (
		rows []model.AgentTrace
		err  error
	)
	switch {
	case sessionID != "" && c.Query("failures") != "1":
		rows, err = daotrace.ListBySession(sessionID, user, 20)
	default:
		// 没指定会话、或明确要失败列表时，给最近的失败轮次——
		// 那才是打开这个接口的人想看的东西。
		rows, err = daotrace.ListFailures(user, 20)
	}
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": err.Error()})
		return
	}

	out := make([]gin.H, 0, len(rows))
	for i := range rows {
		out = append(out, toView(&rows[i], false))
	}
	c.JSON(http.StatusOK, gin.H{"traces": out})
}

// toView 把落库行转成响应。
//
// withSteps=false 时不带步骤明细：列表页只需要"哪几轮不对劲"，
// 把每轮的完整流水账都塞进去，20 条就能把响应撑到几百 KB。
func toView(row *model.AgentTrace, withSteps bool) gin.H {
	v := gin.H{
		"id":         row.ID,
		"sessionId":  row.SessionID,
		"model":      row.Model,
		"modelType":  row.ModelType,
		"status":     row.Status,
		"stepCount":  row.StepCount,
		"toolCalls":  row.ToolCalls,
		"durationMs": row.DurationMs,
		"createdAt":  row.CreatedAt,
	}
	if !withSteps {
		return v
	}

	// 步骤存的是 JSON 原文：它就是 Agent 每一步的流水账，
	// 让前端按需渲染即可，服务端不必替它决定成什么形状。
	// 解析失败时原样带出，至少不丢信息。
	var steps any
	if err := json.Unmarshal([]byte(row.Steps), &steps); err != nil {
		steps = row.Steps
	}
	v["steps"] = steps
	return v
}
