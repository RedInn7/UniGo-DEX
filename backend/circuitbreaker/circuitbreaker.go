package circuitbreaker

import (
	"errors"
	"sync"
	"time"
)

// circuitbreaker.go — 断路器模式（Circuit Breaker）
//
// === 为什么需要断路器？ ===
//
// 后端频繁调用外部服务（以太坊 RPC 节点、The Graph、Chainlink 预言机）。
// 当外部服务故障时，如果不加保护：
//   - 每个请求都会等待超时（通常 30s），线程被阻塞
//   - 请求堆积 → 线程池耗尽 → 整个后端不可用（级联故障）
//
// 断路器通过 "快速失败" 阻止对故障服务的请求，保护后端自身的可用性。
//
// === 三态状态机 ===
//
//   ┌──────────┐  失败次数 ≥ 阈值  ┌──────────┐
//   │  Closed  │ ─────────────────→ │   Open   │
//   │ （正常）  │                    │ （熔断）  │
//   └──────────┘                    └────┬─────┘
//        ↑                               │ 超过冷却时间
//        │ 探测成功                        ↓
//   ┌────┴─────┐                    ┌──────────┐
//   │ 恢复正常  │ ←───────────────── │ HalfOpen │
//   └──────────┘     探测失败 →     │（半开探测）│
//                    回到 Open      └──────────┘
//
//   Closed:   正常放行所有请求，统计失败次数
//   Open:     拒绝所有请求（快速失败），避免打垮下游
//   HalfOpen: 放行少量探测请求，成功则恢复，失败则重新熔断

// State 断路器状态
type State int

const (
	StateClosed   State = iota // 正常
	StateOpen                  // 熔断
	StateHalfOpen              // 半开探测
)

// String 实现 Stringer
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// ErrCircuitOpen 断路器打开时返回的错误
var ErrCircuitOpen = errors.New("circuit breaker is open")

// Config 断路器配置
type Config struct {
	// MaxFailures Closed → Open 的失败次数阈值
	MaxFailures int

	// Timeout Open 状态的冷却时间（之后进入 HalfOpen）
	Timeout time.Duration

	// HalfOpenMaxRequests HalfOpen 状态允许的最大探测请求数
	HalfOpenMaxRequests int

	// OnStateChange 状态变化回调（用于日志/监控）
	OnStateChange func(from, to State)
}

// DefaultConfig 返回默认配置
func DefaultConfig() Config {
	return Config{
		MaxFailures:         5,
		Timeout:             30 * time.Second,
		HalfOpenMaxRequests: 1,
	}
}

// CircuitBreaker 断路器
type CircuitBreaker struct {
	config Config

	state           State
	failures        int       // 连续失败计数
	lastFailureTime time.Time // 最后一次失败的时间
	halfOpenCount   int       // HalfOpen 状态已放行的请求数

	mu sync.Mutex
}

// New 创建断路器
func New(config Config) *CircuitBreaker {
	return &CircuitBreaker{
		config: config,
		state:  StateClosed,
	}
}

// Execute 通过断路器执行操作
//
// 使用方式：
//
//	result, err := cb.Execute(func() (interface{}, error) {
//	    return rpcClient.CallContext(ctx, "eth_blockNumber")
//	})
//
// 行为：
//   - Closed:   执行 fn，成功则重置计数，失败则累加
//   - Open:     直接返回 ErrCircuitOpen（快速失败，不执行 fn）
//   - HalfOpen: 限量放行，成功则回到 Closed，失败则回到 Open
func (cb *CircuitBreaker) Execute(fn func() (interface{}, error)) (interface{}, error) {
	cb.mu.Lock()

	switch cb.state {
	case StateOpen:
		// 检查是否已超过冷却时间
		if time.Since(cb.lastFailureTime) > cb.config.Timeout {
			// 转入 HalfOpen
			cb.setState(StateHalfOpen)
			cb.halfOpenCount = 0
		} else {
			cb.mu.Unlock()
			return nil, ErrCircuitOpen
		}

	case StateHalfOpen:
		// 限制探测请求数
		if cb.halfOpenCount >= cb.config.HalfOpenMaxRequests {
			cb.mu.Unlock()
			return nil, ErrCircuitOpen
		}
		cb.halfOpenCount++
	}

	cb.mu.Unlock()

	// 执行实际操作
	result, err := fn()

	cb.mu.Lock()
	defer cb.mu.Unlock()

	if err != nil {
		cb.onFailure()
		return nil, err
	}

	cb.onSuccess()
	return result, nil
}

// onSuccess 请求成功时的处理
func (cb *CircuitBreaker) onSuccess() {
	switch cb.state {
	case StateClosed:
		// 重置失败计数
		cb.failures = 0

	case StateHalfOpen:
		// 探测成功，恢复正常
		cb.failures = 0
		cb.setState(StateClosed)
	}
}

// onFailure 请求失败时的处理
func (cb *CircuitBreaker) onFailure() {
	switch cb.state {
	case StateClosed:
		cb.failures++
		cb.lastFailureTime = time.Now()
		if cb.failures >= cb.config.MaxFailures {
			// 失败次数达到阈值，熔断
			cb.setState(StateOpen)
		}

	case StateHalfOpen:
		// 探测失败，重新熔断
		cb.lastFailureTime = time.Now()
		cb.setState(StateOpen)
	}
}

// setState 切换状态（触发回调）
func (cb *CircuitBreaker) setState(to State) {
	from := cb.state
	cb.state = to
	if cb.config.OnStateChange != nil {
		cb.config.OnStateChange(from, to)
	}
}

// State 获取当前状态（线程安全）
func (cb *CircuitBreaker) State() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// Failures 获取当前失败计数
func (cb *CircuitBreaker) Failures() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.failures
}

// Reset 手动重置断路器到 Closed 状态
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.halfOpenCount = 0
	cb.setState(StateClosed)
}

// RPCCircuitBreaker 封装了断路器的 RPC 客户端
//
// 使用示例：
//
//	rpcCB := NewRPCCircuitBreaker("http://localhost:8545", DefaultConfig())
//	blockNum, err := rpcCB.Call("eth_blockNumber")
//	if errors.Is(err, ErrCircuitOpen) {
//	    // 节点故障，使用缓存数据或备用节点
//	}
type RPCCircuitBreaker struct {
	endpoint string
	cb       *CircuitBreaker
}

// NewRPCCircuitBreaker 创建 RPC 断路器
func NewRPCCircuitBreaker(endpoint string, config Config) *RPCCircuitBreaker {
	return &RPCCircuitBreaker{
		endpoint: endpoint,
		cb:       New(config),
	}
}

// Call 通过断路器执行 RPC 调用
func (r *RPCCircuitBreaker) Call(method string, args ...interface{}) (interface{}, error) {
	return r.cb.Execute(func() (interface{}, error) {
		// 实际项目中：使用 go-ethereum 的 ethclient
		// client, err := ethclient.Dial(r.endpoint)
		// return client.CallContract(ctx, msg, blockNum)
		_ = method
		_ = args
		return nil, nil // 占位
	})
}

// State 获取断路器状态
func (r *RPCCircuitBreaker) State() State {
	return r.cb.State()
}
