package middleware

import (
	"context"
	"deeptalk/common/code"
	myredis "deeptalk/common/redis"
	"deeptalk/controller"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// ======================== Token Bucket 限流中间件 ========================
//
// 设计思路：
//   - 主方案: Redis Token Bucket，分布式环境下多实例共享计数
//   - 降级方案: 本地 sync.Map 内存限流，Redis 不可用时不阻塞业务
//   - per-user: 容量 10 token，每秒补充 2 token
//   - per-ip:   容量 5 token，每 5 秒补充 1 token（用于未登录接口：验证码/登录/注册）
//   - 超限返回 429 + 友好提示

const (
	rateLimitCapacity  = 10   // 桶容量（突发峰值允许 10 个请求）
	rateLimitRefill    = 2    // 每秒补充 token 数
	rateLimitKeyPrefix = "ratelimit:"

	ipRateLimitCapacity = 5    // 未登录接口按 IP 限流：桶容量
	ipRateLimitRefill   = 0.2  // 每 5 秒补充 1 个
	ipRateLimitKeyPrefix = "ratelimit:ip:"
)

// bucketState 存储在 Redis 或本地内存中的桶状态
type bucketState struct {
	Tokens   float64 `json:"tokens"`
	LastTime int64   `json:"last_time"` // Unix 纳秒
}

// RateLimit 返回一个 gin 限流中间件（按登录用户）
// 必须在 JWT 中间件之后使用（需要从 context 取 userName）
func RateLimit() gin.HandlerFunc {
	limiter := newTokenBucket(rateLimitCapacity, rateLimitRefill, rateLimitKeyPrefix)

	return func(c *gin.Context) {
		// 从 JWT 中间件注入的 context 中获取用户名
		userName, exists := c.Get("userName")
		if !exists {
			c.Next()
			return
		}
		userNameStr, ok := userName.(string)
		if !ok || userNameStr == "" {
			c.Next()
			return
		}

		if !limiter.allow(c.Request.Context(), userNameStr) {
			rejectRateLimited(c, "user="+userNameStr)
			return
		}
		c.Next()
	}
}

// RateLimitByIP 按客户端 IP 限流（用于未登录接口，防刷验证码/暴力破解密码）
func RateLimitByIP() gin.HandlerFunc {
	limiter := newTokenBucket(ipRateLimitCapacity, ipRateLimitRefill, ipRateLimitKeyPrefix)

	return func(c *gin.Context) {
		if !limiter.allow(c.Request.Context(), c.ClientIP()) {
			rejectRateLimited(c, "ip="+c.ClientIP())
			return
		}
		c.Next()
	}
}

func rejectRateLimited(c *gin.Context, who string) {
	log.Printf("[RateLimit] 限流触发: %s path=%s", who, c.Request.URL.Path)
	res := new(controller.Response)
	c.JSON(http.StatusTooManyRequests, res.CodeOf(code.CodeRateLimited))
	c.Abort()
}

// tokenBucket 统一的令牌桶：先走 Redis，Redis 不可用时降级本地内存
type tokenBucket struct {
	capacity float64
	refill   float64
	prefix   string

	localMu    sync.Mutex // 保护本地桶的"读-改-写"（sync.Map 的 Load+Store 不是原子操作）
	localStore map[string]bucketState
}

func newTokenBucket(capacity, refill float64, prefix string) *tokenBucket {
	return &tokenBucket{
		capacity:   capacity,
		refill:     refill,
		prefix:     prefix,
		localStore: make(map[string]bucketState),
	}
}

func (b *tokenBucket) allow(ctx context.Context, id string) bool {
	key := b.prefix + id
	nowNano := time.Now().UnixNano()

	// Redis 故障期间直接走本地桶：既保证限流依然生效，也避免每个请求都去等一次连接超时
	if myredis.Rdb != nil && !redisCoolingDown() {
		allowed, ok := redisTokenBucket(ctx, key, nowNano, b.capacity, b.refill)
		if ok {
			return allowed
		}
	}
	return b.localTokenBucket(key, nowNano)
}

// redisCoolingDown Redis 刚失败过的一小段时间内不再尝试（熔断降级）
var redisDownUntil atomic.Int64

const redisCooldown = 5 * time.Second

func redisCoolingDown() bool {
	return time.Now().UnixNano() < redisDownUntil.Load()
}

func markRedisDown() {
	redisDownUntil.Store(time.Now().Add(redisCooldown).UnixNano())
}

// redisTokenBucket Redis 分布式 Token Bucket
// 使用 Lua 脚本保证"计算补充 → 判断 → 扣减"的原子性
// 第二个返回值 ok=false 表示 Redis 不可用（调用方应降级到本地桶，而不是放行）
func redisTokenBucket(ctx context.Context, key string, nowNano int64, capacity, refill float64) (bool, bool) {
	const script = `
		local state = redis.call('GET', KEYS[1])
		local capacity = tonumber(ARGV[1])
		local refillRate = tonumber(ARGV[2])
		local now = tonumber(ARGV[3])

		local tokens = capacity
		local lastTime = now

		if state ~= false then
			local decoded = cjson.decode(state)
			tokens = tonumber(decoded.tokens)
			lastTime = tonumber(decoded.last_time)

			-- 计算从上次到现在补充的 token 数（纳秒 → 秒）
			local elapsed = (now - lastTime) / 1e9
			if elapsed > 0 then
				tokens = math.min(capacity, tokens + elapsed * refillRate)
			end
		end

		-- 判断是否有足够 token
		if tokens >= 1.0 then
			tokens = tokens - 1.0
			redis.call('SET', KEYS[1], cjson.encode({tokens=tokens, last_time=now}))
			redis.call('EXPIRE', KEYS[1], 300)  -- 5 分钟不用自动清理
			return 1
		end
		-- 超限也要写回，让后续请求看到最新状态
		redis.call('SET', KEYS[1], cjson.encode({tokens=tokens, last_time=now}))
		redis.call('EXPIRE', KEYS[1], 300)
		return 0
	`

	result, err := myredis.Rdb.Eval(ctx, script, []string{key}, capacity, refill, nowNano).Int()
	if err != nil {
		// Redis 异常（连接断开/超时）：标记熔断并让调用方降级到本地限流，
		// 不能直接放行，否则 Redis 一挂限流就等于不存在
		log.Printf("[RateLimit] Redis 异常，降级本地限流: %v", err)
		markRedisDown()
		return false, false
	}
	return result == 1, true
}

// localTokenBucket 本地内存 Token Bucket（Redis 不可用时兜底）
// 注意：单机有效，多实例部署时各算各的；必须加锁保证读-改-写原子
func (b *tokenBucket) localTokenBucket(key string, nowNano int64) bool {
	b.localMu.Lock()
	defer b.localMu.Unlock()

	// 兜底路径也要防止 map 无限增长（极端情况下服务可能长时间无 Redis）
	if len(b.localStore) > 100000 {
		log.Printf("[RateLimit] local bucket map too large(%d), resetting", len(b.localStore))
		b.localStore = make(map[string]bucketState)
	}

	state, ok := b.localStore[key]
	if !ok {
		state = bucketState{
			Tokens:   b.capacity,
			LastTime: nowNano,
		}
	}

	// 计算补充
	elapsed := float64(nowNano-state.LastTime) / 1e9
	if elapsed > 0 {
		state.Tokens += elapsed * b.refill
		if state.Tokens > b.capacity {
			state.Tokens = b.capacity
		}
	}
	state.LastTime = nowNano

	if state.Tokens >= 1.0 {
		state.Tokens -= 1.0
		b.localStore[key] = state
		return true
	}

	b.localStore[key] = state
	return false
}
