package aihelper

import (
	"context"
	"deeptalk/common/metrics"
	"deeptalk/model"
	"log"
	"sync"
	"time"
)

var ctx = context.Background()

// 默认的空闲淘汰参数
const (
	DefaultIdleTTL        = 30 * time.Minute // 会话空闲多久后从内存释放
	DefaultJanitorInterval = 5 * time.Minute // 清理协程的执行间隔
)

// historyLoader 会话历史加载器：内存里没有该会话时，从数据库把历史读回来
// 由启动时注入（见 main.go），测试环境可以保持为 nil
var (
	historyLoaderMu sync.RWMutex
	historyLoader   func(sessionID string) ([]*model.Message, error)
)

// SetHistoryLoader 注入历史加载器
func SetHistoryLoader(loader func(sessionID string) ([]*model.Message, error)) {
	historyLoaderMu.Lock()
	defer historyLoaderMu.Unlock()
	historyLoader = loader
}

func loadHistory(sessionID string) ([]*model.Message, error) {
	historyLoaderMu.RLock()
	loader := historyLoader
	historyLoaderMu.RUnlock()
	if loader == nil {
		return nil, nil
	}
	return loader(sessionID)
}

// AIHelperManager AI助手管理器，管理用户-会话-AIHelper的映射关系
type AIHelperManager struct {
	helpers map[string]map[string]*AIHelper // map[用户账号（唯一）]map[会话ID]*AIHelper
	mu      sync.RWMutex
}

// NewAIHelperManager 创建新的管理器实例
func NewAIHelperManager() *AIHelperManager {
	return &AIHelperManager{
		helpers: make(map[string]map[string]*AIHelper),
	}
}

// GetOrCreateAIHelper 获取或创建AIHelper
//
// 注意：真正的创建（建模型 + 读数据库历史）放在锁外执行——
// 这些操作包含网络与 DB IO，占着全局写锁会让所有用户的会话一起排队。
func (m *AIHelperManager) GetOrCreateAIHelper(userName string, sessionID string, modelType string, config map[string]interface{}) (*AIHelper, error) {
	// 快路径：读锁
	m.mu.RLock()
	if userHelpers, ok := m.helpers[userName]; ok {
		if helper, ok := userHelpers[sessionID]; ok {
			m.mu.RUnlock()
			helper.touch()
			return helper, nil
		}
	}
	m.mu.RUnlock()

	// 慢路径：锁外创建模型并回填历史
	factory := GetGlobalFactory()
	helper, err := factory.CreateAIHelper(ctx, modelType, sessionID, config)
	if err != nil {
		return nil, err
	}
	m.seedHistory(helper)

	m.mu.Lock()
	defer m.mu.Unlock()

	userHelpers, exists := m.helpers[userName]
	if !exists {
		userHelpers = make(map[string]*AIHelper)
		m.helpers[userName] = userHelpers
	}
	// 双检：并发下可能已被别的 goroutine 建好了
	if existing, ok := userHelpers[sessionID]; ok {
		return existing, nil
	}

	helper.touch()
	userHelpers[sessionID] = helper
	return helper, nil
}

// seedHistory 会话首次进入内存时，把数据库里的历史读回来
func (m *AIHelperManager) seedHistory(helper *AIHelper) {
	msgs, err := loadHistory(helper.SessionID)
	if err != nil {
		log.Printf("[AIHelperManager] load history failed: session=%s err=%v", helper.SessionID, err)
		return
	}
	if len(msgs) > 0 {
		helper.LoadHistory(msgs)
		log.Printf("[AIHelperManager] history loaded: session=%s messages=%d", helper.SessionID, len(msgs))
	}
}

// 获取指定用户的指定会话的AIHelper
func (m *AIHelperManager) GetAIHelper(userName string, sessionID string) (*AIHelper, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	userHelpers, exists := m.helpers[userName]
	if !exists {
		return nil, false
	}

	helper, exists := userHelpers[sessionID]
	return helper, exists
}

// StartJanitor 启动空闲会话清理协程
// 没有它的话，helper 会随着"用户数 × 会话数"无限增长，是明确的内存泄漏
func (m *AIHelperManager) StartJanitor(idleTTL, interval time.Duration) {
	if idleTTL <= 0 {
		idleTTL = DefaultIdleTTL
	}
	if interval <= 0 {
		interval = DefaultJanitorInterval
	}

	go func() {
		// 先把活跃会话数初始化出来，避免 /metrics 里缺这个 gauge
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

// Stats 当前内存中缓存的用户数/会话数（可观测性用）
func (m *AIHelperManager) Stats() (users int, sessions int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	users = len(m.helpers)
	for _, sessionHelpers := range m.helpers {
		sessions += len(sessionHelpers)
	}
	return users, sessions
}

// 全局管理器实例
var globalManager *AIHelperManager
var once sync.Once

// GetGlobalManager 获取全局管理器实例
func GetGlobalManager() *AIHelperManager {
	once.Do(func() {
		globalManager = NewAIHelperManager()
	})
	return globalManager
}
