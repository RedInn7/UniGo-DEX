package workerpool

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// === 功能测试 ===

func TestPoolBasic(t *testing.T) {
	pool := New(Config{WorkerCount: 4, QueueSize: 100})
	defer pool.Shutdown()

	var count atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		pool.Submit(func() {
			count.Add(1)
			wg.Done()
		})
	}

	wg.Wait()

	if count.Load() != 100 {
		t.Fatalf("expected 100 tasks completed, got %d", count.Load())
	}

	// 完成计数在任务函数返回之后才累加，wg.Wait 返回时最后一个可能还没记上，这里等它追平
	deadline := time.Now().Add(2 * time.Second)
	for pool.Stats().Completed != 100 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := pool.Stats().Completed; got != 100 {
		t.Fatalf("expected 100 completed in stats, got %d", got)
	}
}

func TestPoolRejectWhenFull(t *testing.T) {
	// 小队列 + 慢任务 → 快速填满
	pool := New(Config{WorkerCount: 1, QueueSize: 2})
	defer pool.Shutdown()

	blocker := make(chan struct{})
	pool.Submit(func() { <-blocker }) // 阻塞唯一的 worker

	// 填满队列
	pool.Submit(func() {})
	pool.Submit(func() {})

	// 第 4 个任务应被拒绝
	if pool.Submit(func() {}) {
		t.Fatal("expected task to be rejected when queue is full")
	}

	close(blocker) // 释放
}

func TestPoolShutdownDrainsQueue(t *testing.T) {
	pool := New(Config{WorkerCount: 2, QueueSize: 50})

	var count atomic.Int64
	for i := 0; i < 50; i++ {
		pool.Submit(func() {
			time.Sleep(1 * time.Millisecond)
			count.Add(1)
		})
	}

	pool.Shutdown()

	if count.Load() != 50 {
		t.Fatalf("expected all 50 tasks drained on shutdown, got %d", count.Load())
	}
}

// === Benchmark：池化 vs 朴素 goroutine 对比 ===
//
// 运行方式：go test -bench=. -benchmem -count=3
//
// 对比维度：
//   1. 每次操作耗时（ns/op）
//   2. 每次操作内存分配（B/op）
//   3. 每次操作 alloc 次数（allocs/op）
//   4. 峰值 goroutine 数量

// BenchmarkNaiveGoroutine 朴素方案：每个任务直接 go func()
//
// 问题：
//   - N 个任务 = N 个 goroutine 同时存在
//   - 每个 goroutine 分配独立的栈（初始 2KB，可能增长到 MB 级）
//   - runtime 调度器需要管理大量 goroutine 的 G-M-P 调度
//   - GC 扫描所有 goroutine 栈，STW 时间与 goroutine 数成正比
func BenchmarkNaiveGoroutine(b *testing.B) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		for j := 0; j < 1000; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// 模拟轻量计算（路由搜索的单步）
				x := 0
				for k := 0; k < 100; k++ {
					x += k
				}
				_ = x
			}()
		}
		wg.Wait()
	}
}

// BenchmarkPooledGoroutine 池化方案：固定 Worker 数量复用
//
// 优势：
//   - goroutine 数量恒定（= WorkerCount），不随任务数增长
//   - 无 goroutine 创建/销毁开销
//   - 栈空间复用，内存分配大幅减少
//   - GC 扫描范围固定，STW 时间稳定
func BenchmarkPooledGoroutine(b *testing.B) {
	b.ReportAllocs()

	pool := New(Config{WorkerCount: runtime.NumCPU(), QueueSize: 2000})
	defer pool.Shutdown()

	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		for j := 0; j < 1000; j++ {
			wg.Add(1)
			pool.Submit(func() {
				defer wg.Done()
				x := 0
				for k := 0; k < 100; k++ {
					x += k
				}
				_ = x
			})
		}
		wg.Wait()
	}
}

// BenchmarkGoroutineMemory 对比内存使用
//
// 通过 runtime.ReadMemStats 测量：
//   - 朴素方案：峰值 goroutine 数 = 并发任务数
//   - 池化方案：峰值 goroutine 数 = WorkerCount（固定）
func BenchmarkGoroutineMemory(b *testing.B) {
	b.Run("Naive_10000_tasks", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var wg sync.WaitGroup
			for j := 0; j < 10000; j++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					time.Sleep(10 * time.Microsecond)
				}()
			}
			peakGoroutines := runtime.NumGoroutine()
			wg.Wait()
			_ = peakGoroutines
		}
	})

	b.Run("Pool_10000_tasks", func(b *testing.B) {
		pool := New(Config{WorkerCount: 100, QueueSize: 20000})
		defer pool.Shutdown()

		for i := 0; i < b.N; i++ {
			var wg sync.WaitGroup
			for j := 0; j < 10000; j++ {
				wg.Add(1)
				pool.Submit(func() {
					defer wg.Done()
					time.Sleep(10 * time.Microsecond)
				})
			}
			peakGoroutines := runtime.NumGoroutine()
			wg.Wait()
			_ = peakGoroutines
		}
	})
}

// 队列满时 SubmitAndWaitResult 也必须给出结果，不能让调用方永远等下去
func TestSubmitAndWaitResultWhenQueueFull(t *testing.T) {
	p := New(Config{WorkerCount: 1, QueueSize: 1})
	block := make(chan struct{})
	p.Submit(func() { <-block }) // 占住唯一的 worker
	for p.Submit(func() {}) {    // 填满队列
	}

	select {
	case got := <-SubmitAndWaitResult(p, func() int { return 42 }):
		if got != 42 {
			t.Fatalf("got %d, want 42", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SubmitAndWaitResult never delivered a result for a rejected task")
	}
	close(block)
	p.Shutdown()
}
