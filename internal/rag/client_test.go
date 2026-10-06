package rag

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// 缓存命中时不应该再构建第二次
func TestCachedRAGQueryCachesOnSuccess(t *testing.T) {
	ResetRAGCache()
	t.Cleanup(ResetRAGCache)

	var builds int32
	build := func() (*RAGQuery, error) {
		atomic.AddInt32(&builds, 1)
		return &RAGQuery{filename: "doc.md", indexName: "rag_docs:doc.md:idx"}, nil
	}

	first, err := cachedRAGQuery("alice", build)
	if err != nil {
		t.Fatalf("第一次构建不应失败: %v", err)
	}
	second, err := cachedRAGQuery("alice", build)
	if err != nil {
		t.Fatalf("第二次取缓存不应失败: %v", err)
	}

	if builds != 1 {
		t.Fatalf("预期只构建一次，实际 %d 次", builds)
	}
	if first != second {
		t.Fatal("第二次应返回同一个实例（缓存没生效）")
	}
}

// 构建失败不能被缓存：用户可能只是还没上传文件，上传后再问应该能成功。
// 这一条很重要——缓存了失败结果会导致"上传了文档也永远检索不到"。
func TestCachedRAGQueryDoesNotCacheFailure(t *testing.T) {
	ResetRAGCache()
	t.Cleanup(ResetRAGCache)

	var builds int32
	boom := errors.New("no uploaded file found")
	build := func() (*RAGQuery, error) {
		n := atomic.AddInt32(&builds, 1)
		if n == 1 {
			return nil, boom
		}
		return &RAGQuery{filename: "doc.md"}, nil
	}

	if _, err := cachedRAGQuery("bob", build); !errors.Is(err, boom) {
		t.Fatalf("第一次应返回原始错误，实际 %v", err)
	}
	q, err := cachedRAGQuery("bob", build)
	if err != nil {
		t.Fatalf("第二次应重试并成功，实际 %v", err)
	}
	if q == nil {
		t.Fatal("第二次应返回实例")
	}
	if builds != 2 {
		t.Fatalf("失败后应重试，预期构建 2 次，实际 %d 次", builds)
	}
}

// 用户重新上传文档后必须失效，否则会继续查旧索引（静默返回错误答案）
func TestInvalidateUserForcesRebuild(t *testing.T) {
	ResetRAGCache()
	t.Cleanup(ResetRAGCache)

	var builds int32
	build := func() (*RAGQuery, error) {
		n := atomic.AddInt32(&builds, 1)
		return &RAGQuery{filename: "v" + string(rune('0'+n)) + ".md"}, nil
	}

	first, _ := cachedRAGQuery("carol", build)
	InvalidateUser("carol")
	second, _ := cachedRAGQuery("carol", build)

	if builds != 2 {
		t.Fatalf("失效后应重新构建，预期 2 次，实际 %d 次", builds)
	}
	if first == second {
		t.Fatal("失效后应拿到新实例")
	}
	if first.filename == second.filename {
		t.Fatalf("失效后索引应指向新文件，实际都是 %s", first.filename)
	}

	// 只失效指定用户，其他人的缓存不该受影响
	InvalidateUser("dave")
	if _, err := cachedRAGQuery("carol", build); err != nil {
		t.Fatal(err)
	}
	if builds != 2 {
		t.Fatalf("失效别的用户不应影响 carol，预期仍为 2 次，实际 %d 次", builds)
	}
}

// 并发首次访问只应构建一次（否则压测时每个并发请求都会读一次磁盘）
func TestCachedRAGQueryConcurrentBuildOnce(t *testing.T) {
	ResetRAGCache()
	t.Cleanup(ResetRAGCache)

	var builds int32
	build := func() (*RAGQuery, error) {
		atomic.AddInt32(&builds, 1)
		return &RAGQuery{filename: "doc.md"}, nil
	}

	const goroutines = 50
	var wg sync.WaitGroup
	results := make([]*RAGQuery, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			q, err := cachedRAGQuery("eve", build)
			if err != nil {
				t.Errorf("并发调用不应失败: %v", err)
				return
			}
			results[i] = q
		}(i)
	}
	wg.Wait()

	// 允许少量并发构建（无 singleflight），但必须都拿到同一个最终实例
	for i := 1; i < goroutines; i++ {
		if results[i] != results[0] {
			t.Fatal("并发下所有调用者必须拿到同一个缓存实例")
		}
	}
	if builds > goroutines {
		t.Fatalf("构建次数异常: %d", builds)
	}
	t.Logf("并发 %d 次首次访问，实际构建 %d 次", goroutines, builds)
}
