package logger

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// 写入的内容最终都要落到下游 writer（异步不等于丢）
func TestAsyncWriterFlushes(t *testing.T) {
	var mu sync.Mutex
	var sink bytes.Buffer
	out := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return sink.Write(p)
	})

	w := NewAsyncWriter(out, 64, 256, 10*time.Millisecond)
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write 不应报错: %v", err)
	}
	if _, err := w.Write([]byte("world\n")); err != nil {
		t.Fatalf("Write 不应报错: %v", err)
	}

	w.Close(2 * time.Second)

	mu.Lock()
	got := sink.String()
	mu.Unlock()
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Fatalf("Close 后应把内容都刷出去，实际 %q", got)
	}
}

// 攒够 bufSize 就应该立刻写一次，而不是死等 ticker
func TestAsyncWriterFlushesOnBufferFull(t *testing.T) {
	var mu sync.Mutex
	var sink bytes.Buffer
	out := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return sink.Write(p)
	})

	// flushEvery 设得很大，确保不是靠定时器刷出去的
	w := NewAsyncWriter(out, 64, 16, time.Hour)
	defer w.Close(time.Second)

	if _, err := w.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := sink.Len()
		mu.Unlock()
		if n >= 16 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("缓冲区满了之后应立即写出，实际没有")
}

// 队列满时必须丢弃并计数，绝不能阻塞调用方（日志不能把业务拖死）
func TestAsyncWriterDropsInsteadOfBlocking(t *testing.T) {
	// 下游故意卡住，让后台协程消费不动
	blocked := make(chan struct{})
	out := writerFunc(func(p []byte) (int, error) {
		<-blocked
		return len(p), nil
	})
	defer close(blocked)

	w := NewAsyncWriter(out, 8, 8, time.Hour)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			_, _ = w.Write([]byte("x"))
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("下游卡住时 Write 仍应立刻返回，不能阻塞")
	}

	if w.Dropped() == 0 {
		t.Fatal("队列满后应有丢弃计数")
	}
	t.Logf("下游卡住、写入 1000 条，丢弃 %d 条（预期行为）", w.Dropped())
}

// Close 之后再写不应 panic，也不能把数据丢掉
func TestAsyncWriterWriteAfterClose(t *testing.T) {
	var mu sync.Mutex
	var sink bytes.Buffer
	out := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return sink.Write(p)
	})

	w := NewAsyncWriter(out, 8, 16, time.Hour)
	w.Close(time.Second)

	if _, err := w.Write([]byte("late\n")); err != nil {
		t.Fatalf("Close 后写入不应报错: %v", err)
	}
	mu.Lock()
	got := sink.String()
	mu.Unlock()
	if !strings.Contains(got, "late") {
		t.Fatalf("Close 后写入应同步落盘，实际 %q", got)
	}

	// 重复 Close 不应 panic
	w.Close(time.Second)
	w.Close(time.Second)
}

// 并发写入不能丢数据（没超过队列容量时）
func TestAsyncWriterConcurrentWrites(t *testing.T) {
	var mu sync.Mutex
	var sink bytes.Buffer
	out := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return sink.Write(p)
	})

	const writers, perWriter = 20, 100
	w := NewAsyncWriter(out, 4096, 4096, 10*time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				_, _ = w.Write([]byte("a"))
			}
		}()
	}
	wg.Wait()
	w.Close(2 * time.Second)

	if d := w.Dropped(); d != 0 {
		t.Fatalf("容量足够时不应丢日志，实际丢 %d 条", d)
	}
	mu.Lock()
	n := sink.Len()
	mu.Unlock()
	if n != writers*perWriter {
		t.Fatalf("预期写入 %d 字节，实际 %d", writers*perWriter, n)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
