// Package trace 存取 Agent 轨迹。
//
// 与 dao/message 的差别：消息是产品数据（用户要看），轨迹是诊断数据
// （开发要看）。所以这里的查询一律**带用户维度**，且允许自动清理——
// 它是可再生的观测数据，不该无限增长。
package trace

import (
	"deeptalk/internal/infra/mysql"
	"deeptalk/model"
)

// Create 写入一条轨迹。
func Create(t *model.AgentTrace) error {
	return mysql.DB.Create(t).Error
}

// Get 按 ID 取一条，且必须属于该用户。
//
// 一定要带 userName：轨迹里有会话内容摘要与工具参数，
// 只按 ID 查会让任何人都能拿别人的轨迹，这跟直接读别人的聊天记录没区别。
func Get(id, userName string) (*model.AgentTrace, error) {
	var t model.AgentTrace
	err := mysql.DB.Where("id = ? AND user_name = ?", id, userName).First(&t).Error
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListBySession 取某个会话最近的若干轮轨迹（倒序）。
func ListBySession(sessionID, userName string, limit int) ([]model.AgentTrace, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var out []model.AgentTrace
	err := mysql.DB.
		Where("session_id = ? AND user_name = ?", sessionID, userName).
		Order("created_at DESC").
		Limit(limit).
		Find(&out).Error
	return out, err
}

// ListFailures 取最近的失败轮次，用于"最近哪里不对"的巡检。
func ListFailures(userName string, limit int) ([]model.AgentTrace, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var out []model.AgentTrace
	err := mysql.DB.
		Where("user_name = ? AND status <> ?", userName, "ok").
		Order("created_at DESC").
		Limit(limit).
		Find(&out).Error
	return out, err
}

// PurgeBefore 清理旧轨迹，返回删除行数。
//
// 轨迹是**可再生的观测数据**：指标已经在 Prometheus 里留了长期趋势，
// 单条轨迹的价值随时间快速衰减。所以给它一个有界的生命周期，
// 而不是像消息那样永久保留。
func PurgeBefore(cutoff any) (int64, error) {
	res := mysql.DB.Where("created_at < ?", cutoff).Delete(&model.AgentTrace{})
	return res.RowsAffected, res.Error
}
