package metrics

import (
	"deeptalk/config"
	"fmt"
	"math"
	"sync"
	"time"
)

// ======================== 费用计算 ========================

// CostMicros 单次请求费用（微元，1 元 = 1,000,000 微元），用整数避免浮点累计误差
func CostMicros(model string, promptTokens, completionTokens, cachedTokens int) int64 {
	cfg := config.GetConfig()

	promptPrice := cfg.AiPricingConfig.DefaultPromptPrice
	if p, ok := cfg.AiPricingConfig.PromptPrice[model]; ok {
		promptPrice = p
	}
	completionPrice := cfg.AiPricingConfig.DefaultCompletionPrice
	if p, ok := cfg.AiPricingConfig.CompletionPrice[model]; ok {
		completionPrice = p
	}

	// 命中前缀缓存的输入 token 通常按更低价计费（DeepSeek 约 1/10），这里用 0.1 系数近似
	const cachedDiscount = 0.1
	billablePrompt := float64(promptTokens)
	if cachedTokens > 0 && cachedTokens <= promptTokens {
		billablePrompt = float64(promptTokens-cachedTokens) + float64(cachedTokens)*cachedDiscount
	}

	yuan := billablePrompt/1e6*promptPrice + float64(completionTokens)/1e6*completionPrice
	// 四舍五入而不是截断：否则每次请求都少算一点，累计起来会与账单对不上
	return int64(math.Round(yuan * 1e6))
}

// ======================== 每用户每日 token 配额 ========================

// 说明：配额优先用 Redis 计数（多实例共享、重启不丢）；
// Redis 不可用时退回进程内计数（单实例有效，重启清零）。
const quotaKeyPrefix = "ai:quota:%s:%s"

var (
	localQuotaMu    sync.Mutex
	localQuotaDay   string
	localQuotaUsage = map[string]int64{}
)

// quotaStore 可替换的计数后端（便于测试）
var quotaStore func(user, day string, delta int64) (int64, error)

// SetQuotaStore 注入配额计数后端（默认由 redisEnabled 决定）
func SetQuotaStore(store func(user, day string, delta int64) (int64, error)) {
	quotaStore = store
}

// CheckQuota 校验并累加用户当日 token 用量
// tokens = 0 表示只查询当前用量（不累加），用于请求发起前的预检
// 返回 (是否允许, 当日已用 token, 配额上限)
func CheckQuota(user string, tokens int) (bool, int64, int) {
	limit := config.GetConfig().AiPricingConfig.DailyTokenQuota
	if limit <= 0 {
		return true, 0, 0
	}

	day := time.Now().Format("2006-01-02")

	var used int64
	switch {
	case tokens <= 0:
		// 只查不扣
		if quotaStore != nil {
			if v, err := quotaStore(user, day, 0); err == nil {
				used = v
			}
		} else {
			used = localQuotaGet(user, day)
		}
	case quotaStore != nil:
		if v, err := quotaStore(user, day, int64(tokens)); err == nil {
			used = v
		} else {
			used = localQuotaAdd(user, day, int64(tokens))
		}
	default:
		used = localQuotaAdd(user, day, int64(tokens))
	}

	if used >= int64(limit) {
		return false, used, limit
	}
	return true, used, limit
}

func localQuotaGet(user, day string) int64 {
	localQuotaMu.Lock()
	defer localQuotaMu.Unlock()
	if localQuotaDay != day {
		return 0
	}
	return localQuotaUsage[user]
}

func localQuotaAdd(user, day string, delta int64) int64 {
	localQuotaMu.Lock()
	defer localQuotaMu.Unlock()
	if localQuotaDay != day {
		localQuotaUsage = make(map[string]int64)
		localQuotaDay = day
	}
	localQuotaUsage[user] += delta
	return localQuotaUsage[user]
}

// QuotaExceeded 构造配额超限错误信息
func QuotaExceeded(used int64, limit int) error {
	return fmt.Errorf("今日 token 配额已用尽（已用 %d / 上限 %d），请明天再试", used, limit)
}

// QuotaKey Redis 配额 key（供外部注入的 store 使用）
func QuotaKey(user, day string) string {
	return fmt.Sprintf(quotaKeyPrefix, user, day)
}
