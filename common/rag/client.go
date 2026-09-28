package rag

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"deeptalk/config"

	embeddingArk "github.com/cloudwego/eino-ext/components/embedding/ark"
	"github.com/cloudwego/eino/components/embedding"
)

// ======================== 客户端复用 ========================
//
// 背景（压测 + CPU profile 发现的）：
// 原本 NewRAGQuery 每个请求都会重建一次 embedding 客户端和 retriever，而
// arkruntime.NewClientWithApiKey 内部会去读磁盘上的 volcengine 共享配置文件
// （session.loadSharedConfigIniFiles → ini.OpenFile → os.Open）。
//
// 20 秒 26 RPS 的 CPU profile 里，这块连同"每次新建 client 导致无法复用连接"
// 一共吃掉了 20%+ 的 CPU：
//
//	rag.NewRAGQuery                   9.02%
//	os.ReadDir                        4.77%
//	ark.NewEmbedder                  4.24%
//	volcengine session.loadShared...  3.98%
//	net/http.(*Transport).dialConn   12.47%   ← 连接池每次都是新的，keep-alive 完全失效
//
// 所以这里做两件事：
//  1. embedding 客户端进程内只建一次，共享复用；
//  2. RAGQuery（含 retriever）按用户名缓存——它依赖的向量索引名来自
//     uploads/<用户名>/ 下的文件名，用户重新上传前不会变，上传/删除时由
//     InvalidateUser 主动失效，避免查到旧索引。

var (
	sharedEmbedderOnce sync.Once
	sharedEmbedder     embedding.Embedder
	sharedEmbedderErr  error

	embedHTTPClientOnce sync.Once
	embedHTTPClient     *http.Client
)

// SharedEmbedder 返回进程内共享的 embedding 客户端（懒初始化，只建一次）。
// 并发调用安全；构建失败会被缓存，需要排查时重启进程或调用 ResetSharedEmbedder。
func SharedEmbedder() (embedding.Embedder, error) {
	sharedEmbedderOnce.Do(func() {
		sharedEmbedder, sharedEmbedderErr = newEmbedder()
	})
	return sharedEmbedder, sharedEmbedderErr
}

func newEmbedder() (embedding.Embedder, error) {
	cfg := config.GetConfig()
	apiKey := cfg.RagModelConfig.RagApiKey
	if apiKey == "" {
		apiKey = os.Getenv("ALIYUN_API_KEY")
	}
	if apiKey == "" {
		apiKey = os.Getenv("DEEPSEEK_API_KEY")
	}

	embedder, err := embeddingArk.NewEmbedder(context.Background(), &embeddingArk.EmbeddingConfig{
		BaseURL: cfg.RagModelConfig.RagBaseUrl,
		APIKey:  apiKey,
		Model:   cfg.RagModelConfig.RagEmbeddingModel,
		// 必须显式传 HTTPClient：不传的话 arkruntime 会自己造 Transport，
		// main 里对 http.DefaultTransport 的连接池调优对它无效，
		// 高并发下每个请求都会重新 TCP 握手（profile 里 dialConn 占 10%+）。
		HTTPClient: sharedEmbedHTTPClient(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create embedder: %w", err)
	}
	return embedder, nil
}

// sharedEmbedHTTPClient 给 embedding 用的 HTTP 客户端：连接池开大 + 设置超时。
// 显式写死这些值，不依赖 main 里对 http.DefaultTransport 的调优顺序。
func sharedEmbedHTTPClient() *http.Client {
	embedHTTPClientOnce.Do(func() {
		var transport *http.Transport
		if base, ok := http.DefaultTransport.(*http.Transport); ok {
			transport = base.Clone()
		} else {
			transport = &http.Transport{}
		}
		transport.MaxIdleConns = 200
		transport.MaxIdleConnsPerHost = 100
		transport.IdleConnTimeout = 90 * time.Second
		transport.ForceAttemptHTTP2 = true

		embedHTTPClient = &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second, // 上游 embedding 接口的兜底超时
		}
	})
	return embedHTTPClient
}

// ---------------- RAGQuery 缓存（按用户名） ----------------

var (
	queryCacheMu sync.RWMutex
	queryCache   = make(map[string]*RAGQuery)
)

// InvalidateUser 让某个用户的知识库缓存失效。
//
// 必须在"改变该用户向量索引"之后调用：上传新文档、删除文档。
// 索引名由文件名决定（rag_docs:<文件名>:idx），换了文件就是换了索引，
// 不失效会继续查旧索引——属于静默的错误答案，比报错更难查。
func InvalidateUser(username string) {
	queryCacheMu.Lock()
	delete(queryCache, username)
	queryCacheMu.Unlock()
}

// ResetRAGCache 清空全部缓存（测试用）
func ResetRAGCache() {
	queryCacheMu.Lock()
	queryCache = make(map[string]*RAGQuery)
	queryCacheMu.Unlock()
}

// cachedRAGQuery 取缓存的 RAGQuery；miss 时用 build 构建并写入
func cachedRAGQuery(username string, build func() (*RAGQuery, error)) (*RAGQuery, error) {
	queryCacheMu.RLock()
	cached := queryCache[username]
	queryCacheMu.RUnlock()
	if cached != nil {
		return cached, nil
	}

	built, err := build()
	if err != nil {
		// 失败不缓存：用户可能只是还没上传文件，上传后再问应该能成功
		return nil, err
	}

	queryCacheMu.Lock()
	defer queryCacheMu.Unlock()
	// 双检：并发构建时以先写入的为准
	if existing, ok := queryCache[username]; ok {
		return existing, nil
	}
	queryCache[username] = built
	return built, nil
}
