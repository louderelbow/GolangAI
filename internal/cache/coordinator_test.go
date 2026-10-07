package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"deeptalk/internal/infra/config"
)

// setupConfig 用一份"关掉语义缓存"的配置跑测试：
// L2 每次查询都要调 embedding，测试里既慢又依赖外部服务。
// L1/L2 的编排逻辑与 L2 内部实现无关，关掉 L2 不影响结论。
func setupConfig(t *testing.T) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file))) // internal/cache/x_test.go -> 模块根
	raw, err := os.ReadFile(filepath.Join(root, "config", "config.toml.example"))
	if err != nil {
		t.Fatalf("读取示例配置失败: %v", err)
	}
	// 关掉语义缓存，避免测试触发真实 embedding 调用。
	// 用正则而不是字面替换：示例配置的行尾可能是 CRLF，字面 "\n" 匹配不上。
	re := regexp.MustCompile(`(?m)^(\[semanticCache\]\s*\r?\n\s*enabled\s*=\s*)true`)
	patched := re.ReplaceAllString(string(raw), "${1}false")
	if patched == string(raw) {
		t.Fatal("未能关闭 semanticCache，示例配置格式可能已变")
	}
	p := filepath.Join(t.TempDir(), "cache_test.toml")
	if err := os.WriteFile(p, []byte(patched), 0o600); err != nil {
		t.Fatalf("写测试配置失败: %v", err)
	}
	t.Setenv("DEEPTALK_CONFIG", p)
}

// ==================== L1 精确缓存 ====================

// TestExactCacheHitOnRepeatedPrompt 验收"精确缓存命中"：
// 同一 prompt 连续请求 3 次，第 2/3 次必须命中 L1 且不再回源。
func TestExactCacheHitOnRepeatedPrompt(t *testing.T) {
	setupConfig(t)

	c := newExactCache(10)
	ttl := time.Minute

	if _, _, _, ok := c.Get("m1", "年假几天"); ok {
		t.Fatal("空缓存不该命中")
	}
	c.Set("m1", "年假几天", "10 天", 100, 500, ttl, 0)

	for i := 2; i <= 3; i++ {
		ans, tokens, cost, ok := c.Get("m1", "年假几天")
		if !ok {
			t.Fatalf("第 %d 次请求应命中 L1", i)
		}
		if ans != "10 天" {
			t.Fatalf("第 %d 次答案不对: %q", i, ans)
		}
		if tokens != 100 || cost != 500 {
			t.Fatalf("第 %d 次应带上回源当时的用量: tokens=%d cost=%d", i, tokens, cost)
		}
	}
}

// TestExactCacheKeyedByModel 同一个问题在不同模型下必须互不干扰，
// 否则 RAG 的答案会被 Agent 复用。
func TestExactCacheKeyedByModel(t *testing.T) {
	c := newExactCache(10)
	c.Set("2|qwen-turbo", "年假几天", "RAG 的答案", 1, 1, time.Minute, 0)
	c.Set("6|qwen-turbo", "年假几天", "Agent 的答案", 1, 1, time.Minute, 0)

	if ans, _, _, _ := c.Get("2|qwen-turbo", "年假几天"); ans != "RAG 的答案" {
		t.Fatalf("RAG 答案不对: %q", ans)
	}
	if ans, _, _, _ := c.Get("6|qwen-turbo", "年假几天"); ans != "Agent 的答案" {
		t.Fatalf("Agent 答案不对: %q", ans)
	}
}

// TestExactCacheKeySeparator "a"+"bc" 与 "ab"+"c" 不能撞成同一个 key。
func TestExactCacheKeySeparator(t *testing.T) {
	if exactKey("a", "bc") == exactKey("ab", "c") {
		t.Fatal("模型名与问题之间必须用分隔符，否则会串答案")
	}
}

func TestExactCacheExpires(t *testing.T) {
	c := newExactCache(10)
	c.Set("m1", "q", "a", 1, 1, 1*time.Millisecond, 0)
	time.Sleep(5 * time.Millisecond)
	if _, _, _, ok := c.Get("m1", "q"); ok {
		t.Fatal("过期条目不该命中")
	}
	if c.Len() != 0 {
		t.Fatalf("过期条目应被顺手删除，实际还剩 %d 条", c.Len())
	}
}

func TestExactCacheEvictsWhenFull(t *testing.T) {
	c := newExactCache(3)
	for _, q := range []string{"q1", "q2", "q3", "q4"} {
		c.Set("m1", q, "a-"+q, 1, 1, time.Minute, 0)
	}
	if c.Len() != 3 {
		t.Fatalf("应被限制在 3 条，实际 %d", c.Len())
	}
	if _, _, _, ok := c.Get("m1", "q1"); ok {
		t.Fatal("最旧的一条应被淘汰")
	}
	if _, _, _, ok := c.Get("m1", "q4"); !ok {
		t.Fatal("最新的一条应还在")
	}
}

// ==================== 雪崩防护 ====================

// TestTTLJitterSpreadsExpiry 验收"雪崩防护"：
// 同一批写入的 key 不能在同一个时刻集体过期。
func TestTTLJitterSpreadsExpiry(t *testing.T) {
	const base = 100 * time.Second

	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := ttlWithJitter(base, 0.2)
		if d < 80*time.Second || d > 120*time.Second {
			t.Fatalf("抖动超出 ±20%% 范围: %s", d)
		}
		seen[d] = true
	}
	if len(seen) < 50 {
		t.Fatalf("200 次采样只出现 %d 个不同值，抖动基本没起作用", len(seen))
	}

	// 没有抖动时必须原样返回
	if got := ttlWithJitter(base, 0); got != base {
		t.Fatalf("jitter=0 时应返回原值，实际 %s", got)
	}
}

// ==================== 穿透防护 ====================

func TestIsRefusal(t *testing.T) {
	refusals := []string{
		"",
		"   ",
		"无法找到相关信息",
		"根据现有资料，未找到相关信息。",
		"文档中没有关于该问题的说明",
	}
	for _, r := range refusals {
		if !IsRefusal(r) {
			t.Errorf("%q 应判定为拒答", r)
		}
	}

	normal := []string{
		"年假为 10 天。",
		"公司年假制度规定：入职满一年享受 10 天年假，满三年 15 天。",
		// 长回答即使含关键词也不该被判成拒答，避免误伤
		strings.Repeat("这是一段有实质内容的回答。", 20) + "文档中没有",
	}
	for _, n := range normal {
		if IsRefusal(n) {
			t.Errorf("%q 不该判定为拒答", n)
		}
	}
}

// TestNegativeCacheBlocksRepeatedRefusal 验收"穿透防护"：
// 同一个问题反复问、而文档里确实没有答案时，短期内不再回源。
func TestNegativeCacheBlocksRepeatedRefusal(t *testing.T) {
	n := newNegativeCache(50 * time.Millisecond)

	if _, ok := n.Get("m1", "不存在的问题"); ok {
		t.Fatal("未记录前不该命中")
	}
	n.Store("m1", "不存在的问题", "无法找到相关信息")

	ans, ok := n.Get("m1", "不存在的问题")
	if !ok {
		t.Fatal("记录后应命中")
	}
	if ans != "无法找到相关信息" {
		t.Fatalf("应返回当时的拒答文案，实际 %q", ans)
	}

	time.Sleep(80 * time.Millisecond)
	if _, ok := n.Get("m1", "不存在的问题"); ok {
		t.Fatal("冷却期过后不该继续拦截——文档可能已经补上了")
	}
}

func TestNegativeCacheClear(t *testing.T) {
	n := newNegativeCache(time.Minute)
	n.Store("m1", "q", "无法找到相关信息")
	n.Clear("m1", "q")
	if _, ok := n.Get("m1", "q"); ok {
		t.Fatal("Clear 后不该命中")
	}
}

// ==================== 击穿防护 ====================

// TestCoordinatorMergesConcurrentMiss 验收"击穿防护"：
// 热点 key 失效瞬间，并发 100 个请求只应真正回源 1 次。
func TestCoordinatorMergesConcurrentMiss(t *testing.T) {
	setupConfig(t)

	c := &Coordinator{
		exact: newExactCache(100),
		neg:   newNegativeCache(time.Minute),
	}

	var calls int32
	gen := func() (Source, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(30 * time.Millisecond) // 模拟一次真实的模型调用
		return Source{Answer: "答案", Tokens: 10, CostMicros: 20}, nil
	}

	const n = 100
	var wg sync.WaitGroup
	results := make([]Hit, n)
	errs := make([]error, n)
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = c.Do(context.Background(), Key{User: "u1", Model: "m1"}, "同一个问题", gen)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("并发 %d 个请求应只回源 1 次，实际 %d 次", n, got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个请求报错: %v", i, errs[i])
		}
		if results[i].Answer != "答案" {
			t.Fatalf("第 %d 个请求答案不对: %q", i, results[i].Answer)
		}
	}
}

// TestCoordinatorDoCachesResult 回源结果要写进缓存，第二次直接命中。
func TestCoordinatorDoCachesResult(t *testing.T) {
	setupConfig(t)

	c := &Coordinator{exact: newExactCache(100), neg: newNegativeCache(time.Minute)}

	var calls int32
	gen := func() (Source, error) {
		atomic.AddInt32(&calls, 1)
		return Source{Answer: "答案", Tokens: 7, CostMicros: 9}, nil
	}

	first, err := c.Do(context.Background(), Key{User: "u1", Model: "m1"}, "q", gen)
	if err != nil {
		t.Fatalf("第一次回源失败: %v", err)
	}
	if first.Level != LevelMiss {
		t.Fatalf("第一次应是回源，实际 level=%s", first.Level)
	}

	second, err := c.Do(context.Background(), Key{User: "u1", Model: "m1"}, "q", gen)
	if err != nil {
		t.Fatalf("第二次失败: %v", err)
	}
	if second.Level != LevelExact {
		t.Fatalf("第二次应命中 L1，实际 level=%s", second.Level)
	}
	if second.Tokens != 7 || second.CostMicros != 9 {
		t.Fatalf("命中时应带上回源当时的用量: tokens=%d cost=%d", second.Tokens, second.CostMicros)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("只应回源 1 次，实际 %d 次", got)
	}
}

// TestCoordinatorDoSkipsCacheForRefusal 拒答走的是空结果冷却期，
// 不该占用正常答案的位置。
func TestCoordinatorDoSkipsCacheForRefusal(t *testing.T) {
	setupConfig(t)

	c := &Coordinator{exact: newExactCache(100), neg: newNegativeCache(time.Minute)}

	gen := func() (Source, error) { return Source{Answer: "无法找到相关信息"}, nil }
	if _, err := c.Do(context.Background(), Key{User: "u1", Model: "m1"}, "q", gen); err != nil {
		t.Fatalf("回源失败: %v", err)
	}

	if _, _, _, ok := c.exact.Get("u1|m1", "q"); ok {
		t.Fatal("拒答不该写进 L1 精确缓存")
	}
	if _, ok := c.neg.Get("u1|m1", "q"); !ok {
		t.Fatal("拒答应写进空结果冷却期")
	}
}

func TestCoordinatorDoPropagatesError(t *testing.T) {
	setupConfig(t)

	c := &Coordinator{exact: newExactCache(100), neg: newNegativeCache(time.Minute)}
	want := errors.New("upstream boom")

	_, err := c.Do(context.Background(), Key{User: "u1", Model: "m1"}, "q", func() (Source, error) { return Source{}, want })
	if !errors.Is(err, want) {
		t.Fatalf("应把回源错误透传出去，实际 %v", err)
	}
}

// TestCoordinatorDoesNotLeakAcrossUsers 缓存绝不能跨用户命中。
//
// 这是**隔离边界**，不是命中率优化：RAG 的答案取决于该用户自己的文档，
// 缓存键里不带用户时，A 问出来的答案会被 B 的同一句话命中。
// 修复前正是如此（键只有 modelType|modelName）。
func TestCoordinatorDoesNotLeakAcrossUsers(t *testing.T) {
	setupConfig(t)

	c := &Coordinator{exact: newExactCache(100), neg: newNegativeCache(time.Minute)}
	keyOf := func(user string) Key { return Key{User: user, Model: "2|qwen"} }

	var calls int32
	alice := func() (Source, error) {
		atomic.AddInt32(&calls, 1)
		return Source{Answer: "A 的文档里写着 10 天"}, nil
	}

	first, err := c.Do(context.Background(), keyOf("alice"), "我的年假几天", alice)
	if err != nil {
		t.Fatalf("A 回源失败: %v", err)
	}
	if first.Level != LevelMiss {
		t.Fatalf("A 首次应回源，实际 level=%s", first.Level)
	}

	if again, _ := c.Do(context.Background(), keyOf("alice"), "我的年假几天", alice); again.Level != LevelExact {
		t.Fatalf("同一用户重复提问应命中 L1，实际 level=%s", again.Level)
	}

	bob, err := c.Do(context.Background(), keyOf("bob"), "我的年假几天", func() (Source, error) {
		atomic.AddInt32(&calls, 1)
		return Source{Answer: "B 的文档里写着 5 天"}, nil
	})
	if err != nil {
		t.Fatalf("B 回源失败: %v", err)
	}
	if bob.Level == LevelExact || bob.Level == LevelSemantic {
		t.Fatalf("B 不该命中 A 的缓存，实际 level=%s", bob.Level)
	}
	if bob.Answer != "B 的文档里写着 5 天" {
		t.Fatalf("B 拿到了别人的答案: %q", bob.Answer)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("两个用户各回源一次，应共 2 次，实际 %d 次", got)
	}
}

// TestCoordinatorRejectsEmptyUser 拿不到用户身份时整条缓存链路必须跳过。
//
// fail closed：身份不明就不缓存，绝不退化成"所有人共用一个命名空间"——
// 那正是修复前的行为。
func TestCoordinatorRejectsEmptyUser(t *testing.T) {
	setupConfig(t)

	c := &Coordinator{exact: newExactCache(100), neg: newNegativeCache(time.Minute)}

	var calls int32
	gen := func() (Source, error) {
		atomic.AddInt32(&calls, 1)
		return Source{Answer: "答案"}, nil
	}
	anon := Key{Model: "2|qwen"}

	for i := 0; i < 3; i++ {
		hit, err := c.Do(context.Background(), anon, "q", gen)
		if err != nil {
			t.Fatalf("第 %d 次回源失败: %v", i, err)
		}
		if hit.Level != LevelMiss {
			t.Fatalf("匿名请求不该命中缓存，实际 level=%s", hit.Level)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("匿名请求每次都要回源，实际回源 %d 次", got)
	}
	if n := c.exact.Len(); n != 0 {
		t.Fatalf("匿名请求不该写进缓存，实际 %d 条", n)
	}
}

// TestInvalidateAllClearsLocal 文档更新后本地缓存必须清空。
func TestInvalidateAllClearsLocal(t *testing.T) {
	setupConfig(t)

	c := newExactCache(10)
	c.Set("m1", "q", "a", 1, 1, time.Minute, 0)
	neg := newNegativeCache(time.Minute)
	neg.Store("m1", "q2", "无法找到相关信息")

	c.Purge()
	neg.Purge()

	if c.Len() != 0 || neg.Len() != 0 {
		t.Fatalf("Purge 后应清空: exact=%d negative=%d", c.Len(), neg.Len())
	}
}

// TestCacheConfigDefaults 配置默认值必须补全，否则零值会让缓存立即过期或容量为 0。
func TestCacheConfigDefaults(t *testing.T) {
	setupConfig(t)
	cfg := config.GetConfig().GetCache()

	if !cfg.IsEnabled() {
		t.Error("cache.enabled 未解析")
	}
	if cfg.ExactTTLSeconds != 300 {
		t.Errorf("exactTTLSeconds = %d, want 300", cfg.ExactTTLSeconds)
	}
	if cfg.ExactMaxEntries != 10000 {
		t.Errorf("exactMaxEntries = %d, want 10000", cfg.ExactMaxEntries)
	}
	if cfg.JitterRatio != 0.2 {
		t.Errorf("jitterRatio = %v, want 0.2", cfg.JitterRatio)
	}
	if cfg.Penetration.EmptyResultTTLSeconds != 30 {
		t.Errorf("emptyResultTTLSeconds = %d, want 30", cfg.Penetration.EmptyResultTTLSeconds)
	}

	// 零值也要能补全
	var zero config.CacheConfig
	got := zero.WithDefaults()
	if !got.IsEnabled() {
		t.Error("未配置 [cache] 段时应默认开启——用普通 bool 会让漏配等于静默关闭")
	}
	if got.ExactTTLSeconds <= 0 || got.ExactMaxEntries <= 0 || got.JitterRatio <= 0 {
		t.Fatalf("零值未补全: %+v", got)
	}
}
