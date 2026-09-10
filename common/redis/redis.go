package redis

import (
	"context"
	"deeptalk/config"
	"fmt"
	"strconv"
	"strings"
	"time"

	redisCli "github.com/redis/go-redis/v9"
)

var Rdb *redisCli.Client

var ctx = context.Background()

func Init() {
	conf := config.GetConfig()
	host := conf.RedisConfig.RedisHost
	port := conf.RedisConfig.RedisPort
	password := conf.RedisConfig.RedisPassword
	db := conf.RedisDb
	addr := host + ":" + strconv.Itoa(port)

	Rdb = redisCli.NewClient(&redisCli.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
		Protocol: 2, // 使用 Protocol 2 避免 maint_notifications 警告

		// 超时/重试必须收紧：go-redis 默认 MaxRetries=3、DialTimeout=5s，
		// Redis 一旦不可用，每个请求都会白白多等 1~2 秒才降级，把整站延迟拖垮。
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolTimeout:  2 * time.Second,
		MaxRetries:   0,
	})
}

func SetCaptchaForEmail(email, captcha string) error {
	key := GenerateCaptcha(email)
	expire := 2 * time.Minute
	return Rdb.Set(ctx, key, captcha, expire).Err()
}

// TryAcquire 一次性的 SET NX EX，用于"发送冷却"这类幂等控制
// 返回 true 表示抢到了（本次允许执行）
func TryAcquire(key string, ttl time.Duration) (bool, error) {
	return Rdb.SetNX(ctx, key, "1", ttl).Result()
}

// QuotaIncr 每日 token 配额计数（多实例共享）
// delta <= 0 表示只读取当前值
func QuotaIncr(user, day string, delta int64) (int64, error) {
	if Rdb == nil {
		return 0, fmt.Errorf("redis not initialized")
	}
	key := fmt.Sprintf("ai:quota:%s:%s", user, day)

	if delta <= 0 {
		v, err := Rdb.Get(ctx, key).Int64()
		if err == redisCli.Nil {
			return 0, nil
		}
		return v, err
	}

	v, err := Rdb.IncrBy(ctx, key, delta).Result()
	if err != nil {
		return 0, err
	}
	// 两天过期：跨天自动重置，不需要定时任务
	Rdb.Expire(ctx, key, 48*time.Hour)
	return v, nil
}

func CheckCaptchaForEmail(email, userInput string) (bool, error) {
	key := GenerateCaptcha(email)

	storedCaptcha, err := Rdb.Get(ctx, key).Result()
	if err != nil {
		if err == redisCli.Nil {

			return false, nil
		}

		return false, err
	}

	// 不区分大小写判断字符串是否相同
	if strings.EqualFold(storedCaptcha, userInput) {

		// 验证成功后删除 key
		if err := Rdb.Del(ctx, key).Err(); err != nil {

		} else {

		}
		return true, nil
	}

	return false, nil
}

// InitRedisIndex 初始化 Redis 索引，支持按文件名区分
func InitRedisIndex(ctx context.Context, filename string, dimension int) error {
	indexName := GenerateIndexName(filename)

	// 检查索引是否存在
	_, err := Rdb.Do(ctx, "FT.INFO", indexName).Result()
	if err == nil {
		fmt.Println("索引已存在，跳过创建")
		return nil
	}

	// 如果索引不存在，创建新索引
	if !strings.Contains(err.Error(), "Unknown index name") {
		return fmt.Errorf("检查索引失败: %w", err)
	}

	fmt.Println("正在创建 Redis 索引...")

	prefix := GenerateIndexNamePrefix(filename)

	// 创建索引（content 字段启用中文分词）
	createArgs := []interface{}{
		"FT.CREATE", indexName,
		"ON", "HASH",
		"PREFIX", "1", prefix,
		"LANGUAGE", "chinese",
		"SCHEMA",
		"content", "TEXT",
		"metadata", "TEXT",
		"vector", "VECTOR", "FLAT",
		"6",
		"TYPE", "FLOAT32",
		"DIM", dimension,
		"DISTANCE_METRIC", "COSINE",
	}

	if err := Rdb.Do(ctx, createArgs...).Err(); err != nil {
		return fmt.Errorf("创建索引失败: %w", err)
	}

	fmt.Println("索引创建成功！")
	return nil
}

// DeleteRedisIndex 删除 Redis 索引，支持按文件名区分
func DeleteRedisIndex(ctx context.Context, filename string) error {
	indexName := GenerateIndexName(filename)

	// 删除索引
	if err := Rdb.Do(ctx, "FT.DROPINDEX", indexName).Err(); err != nil {
		return fmt.Errorf("删除索引失败: %w", err)
	}

	fmt.Println("索引删除成功！")
	return nil
}
