package workerpool

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// pool.go — 高性能 Goroutine 池
//
// === 为什么需要 Goroutine 池？ ===
//
// Go 的 goroutine 虽然轻量（初始栈仅 2KB），但在高并发场景下，
// 无限制地创建 goroutine 仍然会导致：
//
//   1. 调度开销：runtime 需要管理百万级 goroutine 的调度
//   2. 内存压力：每个 goroutine 的栈会动态增长，大量 goroutine 累积占用显著内存
//   3. GC 压力：goroutine 引用的对象延迟回收，GC STW 时间增加
//
// Goroutine 池通过复用固定数量的 worker，将 "每请求一个 goroutine"
// 转变为 "N 个 worker 消费任务队列"，显著降低调度和内存开销。
//
// === 设计 ===
//
//   ┌──────────┐     任务入队      ┌────────────────┐
//   │ 调用方    │ ──────────────→  │  taskQueue      │
//   │ Submit() │                   │ (有界 Channel)   │
//   └──────────┘                   └───────┬────────┘
//                                          │
//                    ┌─────────────────────┼─────────────────────┐
//                    │                     │                     │
//               ┌────▼─────┐         ┌────▼─────┐         ┌────▼─────┐
//               │ Worker 1 │         │ Worker 2 │   ...   │ Worker N │
//               │ (复用)    │         │ (复用)    │         │ (复用)    │
//               └──────────┘         └──────────┘         └──────────┘
//
// 对比朴素方案（go func()）：
//   - 朴素方案：每个任务创建一个新 goroutine，任务完成后 goroutine 销毁
//   - 池化方案：Worker 长驻，从 Channel 取任务，执行完继续等待下一个

// Task 表示一个可执行的任务
type Task func()

// Pool goroutine 池
type Pool struct {
	workerCount int
	taskQueue   chan Task
	wg          sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc

	// 统计指标
	submitted  atomic.Int64 // 提交的任务数
	completed  atomic.Int64 // 完成的任务数
	rejected   atomic.Int64 // 拒绝的任务数（队列满）
	totalWaitNs atomic.Int64 // 任务在队列中的累计等待时间（纳秒）
}

// Config 池配置
type Config struct {
	// WorkerCount Worker 数量
	WorkerCount int
	// QueueSize 任务队列容量（有界 Channel）
	QueueSize int
}

// DefaultConfig 默认配置
func DefaultConfig() Config {
	return Config{
		WorkerCount: 200,
		QueueSize:   1000,
	}
}

// New 创建 goroutine 池
func New(cfg Config) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		workerCount: cfg.WorkerCount,
		taskQueue:   make(chan Task, cfg.QueueSize),
		ctx:         ctx,
		cancel:      cancel,
	}

	// 启动固定数量的 Worker
	p.wg.Add(cfg.WorkerCount)
	for i := 0; i < cfg.WorkerCount; i++ {
		go p.worker(i)
	}

	return p
}

// worker 工作协程：循环从队列取任务执行
//
// 优先从 taskQueue 取任务执行，只有在 taskQueue 关闭且清空后才退出。
// 这保证了 Shutdown() 时队列中的任务全部被执行完毕。
func (p *Pool) worker(id int) {
	defer p.wg.Done()

	for task := range p.taskQueue {
		task()
		p.completed.Add(1)
	}
}

// Submit 提交任务到池中
//
// 如果队列已满，返回 false（非阻塞）
// 调用方可以决定：降级处理 / 直接 go func() / 返回错误
func (p *Pool) Submit(task Task) bool {
	p.submitted.Add(1)

	select {
	case p.taskQueue <- task:
		return true
	default:
		// 队列已满，拒绝任务
		p.rejected.Add(1)
		return false
	}
}

// SubmitWait 提交任务并阻塞等待队列有空位
//
// 带超时保护，避免无限等待
func (p *Pool) SubmitWait(ctx context.Context, task Task) bool {
	p.submitted.Add(1)

	select {
	case p.taskQueue <- task:
		return true
	case <-ctx.Done():
		p.rejected.Add(1)
		return false
	case <-p.ctx.Done():
		p.rejected.Add(1)
		return false
	}
}

// SubmitAndWaitResult 提交任务并等待结果
//
// 返回一个 channel，结果通过 channel 传递
// 用于需要等待返回值的场景
//
// 队列已满、任务被拒绝时在调用方协程里直接执行，否则等结果的一方会永远收不到值。
func SubmitAndWaitResult[T any](p *Pool, fn func() T) <-chan T {
	ch := make(chan T, 1)
	if !p.Submit(func() { ch <- fn() }) {
		ch <- fn()
	}
	return ch
}

// Shutdown 优雅关闭
//
// 1. 关闭任务队列（停止接受新任务）
// 2. Worker 会消费完队列中剩余的任务
// 3. 所有 Worker 因 channel 关闭退出
// 4. 最后取消 context（清理其他资源）
//
// 注意：必须先 close(taskQueue) 再 cancel()，否则 Worker 会因 ctx.Done()
// 先触发而提前退出，导致队列中的任务未被执行。
func (p *Pool) Shutdown() {
	close(p.taskQueue)
	p.wg.Wait()
	p.cancel()
}

// ShutdownWithTimeout 带超时的优雅关闭
func (p *Pool) ShutdownWithTimeout(timeout time.Duration) bool {
	p.cancel()
	close(p.taskQueue)

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Stats 池统计信息
type Stats struct {
	WorkerCount int
	QueueSize   int
	QueueUsed   int
	Submitted   int64
	Completed   int64
	Rejected    int64
}

// Stats 获取统计信息
func (p *Pool) Stats() Stats {
	return Stats{
		WorkerCount: p.workerCount,
		QueueSize:   cap(p.taskQueue),
		QueueUsed:   len(p.taskQueue),
		Submitted:   p.submitted.Load(),
		Completed:   p.completed.Load(),
		Rejected:    p.rejected.Load(),
	}
}
