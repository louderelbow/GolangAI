package chat

import (
	"log"
	"time"

	"deeptalk/internal/infra/metrics"
)

// StartJanitor 启动空闲会话清理协程
func (m *AIHelperManager) StartJanitor(idleTTL, interval time.Duration) {
	if idleTTL <= 0 {
		idleTTL = DefaultIdleTTL
	}
	if interval <= 0 {
		interval = DefaultJanitorInterval
	}

	go func() {
		m.reportStats()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			m.evictIdle(idleTTL)
		}
	}()
	log.Printf("[AIHelperManager] janitor started: idleTTL=%s interval=%s", idleTTL, interval)
}

// reportStats 上报活跃会话数（可观测）
func (m *AIHelperManager) reportStats() {
	_, sessions := m.Stats()
	metrics.SetGauge(metrics.MetricActiveSession, nil, float64(sessions))
}

// evictIdle 释放长时间未使用的会话（下次请求会按需从数据库重新加载）
func (m *AIHelperManager) evictIdle(idleTTL time.Duration) {
	deadline := time.Now().Add(-idleTTL).UnixNano()

	m.mu.Lock()
	evicted := 0
	for userName, sessionHelpers := range m.helpers {
		for sessionID, helper := range sessionHelpers {
			if helper.lastUsedNano() < deadline {
				delete(sessionHelpers, sessionID)
				evicted++
			}
		}
		if len(sessionHelpers) == 0 {
			delete(m.helpers, userName)
		}
	}
	users, sessions := len(m.helpers), 0
	for _, sessionHelpers := range m.helpers {
		sessions += len(sessionHelpers)
	}
	m.mu.Unlock()

	// 活跃会话数上报（/metrics 可观测）
	metrics.SetGauge(metrics.MetricActiveSession, nil, float64(sessions))

	if evicted > 0 {
		log.Printf("[AIHelperManager] evicted %d idle sessions, now users=%d sessions=%d", evicted, users, sessions)
	}
}
