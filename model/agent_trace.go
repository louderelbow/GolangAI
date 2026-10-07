package model

import "time"

// AgentTrace 一轮 Agent 对话的完整轨迹。
//
// 一步一行，还是一轮一行？选了**一轮一行 + 轨迹 JSON**。
//
// 理由是职责划分：聚合统计（工具成功率、步数分布、澄清触发率）已经交给
// Prometheus 了，数据库这边只需要"按 trace_id 把这一轮原样放出来"。
// 一步一行会让每轮写 6~10 行，换来的查询能力却和 Prometheus 重复。
//
// 另外留了几个标量列（status / step_count / tool_calls / duration_ms）用于筛选，
// 免得为了"找最近失败的几轮"去解析 JSON。
type AgentTrace struct {
	ID         string    `gorm:"primaryKey;type:varchar(36)" json:"id"`
	SessionID  string    `gorm:"index;type:varchar(36)" json:"session_id"`
	UserName   string    `gorm:"index;type:varchar(20)" json:"username"`
	Model      string    `gorm:"type:varchar(64)" json:"model"`
	ModelType  string    `gorm:"type:varchar(16)" json:"model_type"`
	Status     string    `gorm:"index;type:varchar(16)" json:"status"`
	StepCount  int       `json:"step_count"`
	ToolCalls  int       `json:"tool_calls"`
	DurationMs int64     `json:"duration_ms"`
	Steps      string    `gorm:"type:mediumtext" json:"steps"`
	CreatedAt  time.Time `gorm:"index" json:"created_at"`
}

func (AgentTrace) TableName() string { return "agent_trace" }
