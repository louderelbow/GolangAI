package cache

import (
	"strings"
	"sync"
	"time"
)

// ======================== 缓存穿透防护 ========================
//
// 穿透指"查询一个必然不存在的数据，每次请求都穿过缓存打到后端"。
// 本项目的形态是：模型明确回答"无法找到相关信息"（拒答），而用户反复问同一句。
//
// 方案：**空结果也缓存，但 TTL 短得多**。
//
//	正常答案 TTL 30min：知识不会每分钟变
//	空结果  TTL  30s：文档可能刚上传，30 秒后重试是合理的
//
// 关于规格里提的布隆过滤器——这里没有实现，因为它在这个场景下会起反作用。
// 布隆过滤器的前提是"key 空间有界、且查不存在的 key 占比高"，而这里 key 是
// 用户问题：几乎每条都是新的，过滤器会把所有**首次提问**都判成"一定不存在"，
// 从而跳过整个缓存层，让缓存彻底失效。
// 真正有界的是**文档**而不是问题，所以拦截点应该在检索层，不在答案缓存层。

type negativeEntry struct {
	answer  string
	expires time.Time
}

// negativeCache 空结果缓存：把拒答也记下来，短期内不再重复跑检索 + 模型。
type negativeCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]negativeEntry
}

func newNegativeCache(ttl time.Duration) *negativeCache {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &negativeCache{ttl: ttl, m: make(map[string]negativeEntry, 256)}
}

// Get 命中空结果冷却期时返回当时的拒答文案。
func (n *negativeCache) Get(modelKey, question string) (string, bool) {
	key := exactKey(modelKey, question)

	n.mu.Lock()
	defer n.mu.Unlock()

	e, ok := n.m[key]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expires) {
		delete(n.m, key)
		return "", false
	}
	return e.answer, true
}

// Store 记录一次空结果。
func (n *negativeCache) Store(modelKey, question, answer string) {
	key := exactKey(modelKey, question)

	n.mu.Lock()
	defer n.mu.Unlock()

	// 顺手清过期项，避免这张表只增不减
	if len(n.m) > 0 {
		now := time.Now()
		for k, v := range n.m {
			if now.After(v.expires) {
				delete(n.m, k)
			}
		}
	}
	n.m[key] = negativeEntry{
		answer: answer,
		// 空结果的 TTL 也加抖动：文档批量上传后，这批"没答案"的记录
		// 如果不打散，会在同一秒集体失效、流量一起涌向模型
		expires: time.Now().Add(ttlWithJitter(n.ttl, 0.2)),
	}
}

// Clear 清掉某条记录（例如用户刚上传新文档，之前"没有答案"的判断不再成立）。
func (n *negativeCache) Clear(modelKey, question string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.m, exactKey(modelKey, question))
}

// Purge 清空（上传新文档后调用）。
func (n *negativeCache) Purge() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.m = make(map[string]negativeEntry, 256)
}

// Len 当前条数（可观测性）。
func (n *negativeCache) Len() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.m)
}

// refusalMarkers 判定"模型明确表示没找到答案"。
//
// 与 RAG 提示词里要求模型输出的措辞保持一致；判定得保守一点（宁可漏判），
// 因为把正常答案误判成空结果会被缓存 30 秒，代价比漏判大。
var refusalMarkers = []string{
	"无法找到相关信息", "没有相关信息", "未找到相关信息", "无法回答",
	"无法从文档", "文档中没有", "资料中没有",
}

// IsRefusal 判断回答是否为"文档里没有答案"的拒答。
func IsRefusal(answer string) bool {
	a := strings.TrimSpace(answer)
	if a == "" {
		return true
	}
	// 长回答一般包含实质内容，不做拒答判定，避免误伤
	if len([]rune(a)) > 120 {
		return false
	}
	for _, m := range refusalMarkers {
		if strings.Contains(a, m) {
			return true
		}
	}
	return false
}
