package middleware

import (
	"context"
	"deeptalk/common/code"
	"deeptalk/controller"
	myredis "deeptalk/common/redis"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// ======================== Token Bucket 限流中间件 ========================
//
// 设计思路：
//   - 主方案: Redis Token Bucket，分布式环境下多实例共享计数
//   - 降级方案: 本地 sync.Map 内存限流，Redis 不可用时不阻塞业务
//   - 只对 AI 聊天接口生效（/AI/chat/send 和 /AI/chat/send-stream）
//   - 每用户独立桶：容量 10 token，每秒补充 2 token
//   - 超限返回 429 + 友好提示

const (
	rateLimitCapacity  = 10   // 桶容量（突发峰值允许 10 个请求）
	rateLimitRefill    = 2    // 每秒补充 token 数（测试时可临时改为 5/1 验证限流）
	rateLimitKeyPrefix = "ratelimit:"
)

// 测试用：临时降低阈值验证限流效果
const (
	testCapacity = 3   // 测试容量
	testRefill   = 1   // 测试每秒补充数
)

// bucketState 存储在 Redis 或本地内存中的桶状态
type bucketState struct {
	Tokens   float64 `json:"tokens"`
	LastTime int64   `json:"last_time"` // Unix 纳秒
}

// RateLimit 返回一个 gin 限流中间件
// 必须在 JWT 中间件之后使用（需要从 context 取 userName）
func RateLimit() gin.HandlerFunc {
	localStore := &sync.Map{} // 本地兜底存储

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

		key := rateLimitKeyPrefix + userNameStr
		now := time.Now()
		nowNano := now.UnixNano()

		// 先尝试 Redis
		if myredis.Rdb != nil {
			if allow := redisTokenBucket(c.Request.Context(), key, nowNano); allow {
				c.Next()
				return
			}
			// Redis 限流生效：拒绝请求
			log.Printf("[RateLimit] Redis 限流触发: user=%s\n", userNameStr)
			res := new(controller.Response)
			c.JSON(http.StatusTooManyRequests, res.CodeOf(code.CodeRateLimited))
			c.Abort()
			return
		}

		// Redis 不可用 → 降级本地内存限流
		if allow := localTokenBucket(localStore, key, nowNano); allow {
			c.Next()
			return
		}
		log.Printf("[RateLimit] 本地限流触发: user=%s\n", userNameStr)
		res := new(controller.Response)
		c.JSON(http.StatusTooManyRequests, res.CodeOf(code.CodeRateLimited))
		c.Abort()
	}
}

// redisTokenBucket Redis 分布式 Token Bucket
// 使用 Lua 脚本保证"计算补充 → 判断 → 扣减"的原子性
func redisTokenBucket(ctx context.Context, key string, nowNano int64) bool {
	script := `
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
			redis.call('EXPIRE', KEYS[1], 60)  -- 60 秒不用自动清理
			return 1
		else
			return 0
		end
	`

	result, err := myredis.Rdb.Eval(ctx, script, []string{key},
		rateLimitCapacity, rateLimitRefill, nowNano).Int()
	if err != nil {
		// Lua 脚本执行异常（如 Redis 连接断开），降级放行
		fmt.Printf("[RateLimit] Redis 异常，降级放行: %v\n", err)
		return true
	}
	return result == 1
}

// localTokenBucket 本地内存 Token Bucket（Redis 不可用时兜底）
// 注意：单机有效，多实例部署时各算各的
func localTokenBucket(store *sync.Map, key string, nowNano int64) bool {
	val, _ := store.Load(key)
	var state bucketState
	if val != nil {
		state = val.(bucketState)
	} else {
		state = bucketState{
			Tokens:   rateLimitCapacity,
			LastTime: nowNano,
		}
	}

	// 计算补充
	elapsed := float64(nowNano-state.LastTime) / 1e9
	if elapsed > 0 {
		state.Tokens += elapsed * rateLimitRefill
		if state.Tokens > rateLimitCapacity {
			state.Tokens = rateLimitCapacity
		}
	}
	state.LastTime = nowNano

	// 判断 + 扣减
	if state.Tokens >= 1.0 {
		state.Tokens -= 1.0
		store.Store(key, state)
		return true
	}

	// 超限了，但还是要存回当前状态（让后续请求感知到超限）
	store.Store(key, state)
	return false
}
