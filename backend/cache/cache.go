package cache

import (
	"sync"
	"time"

	"unigo-dex/metrics"
)

// cache.go — 缓存层（带 TTL 和 Singleflight 请求合并）
//
// === 缓存击穿防护 ===
//
// 热点 Key 过期的瞬间，大量并发请求会同时发现缓存未命中，一起去做同一次昂贵的计算
// （路由搜索、链上 TWAP 查询）。Singleflight 的做法与 golang.org/x/sync/singleflight 相同：
// 同一个 Key 同时只允许 1 个请求真正去加载，其余请求等它完成后复用结果。
//
// === 存储 ===
//
// 内存缓存（map + 读写锁 + TTL），条目数有上限，后台协程定期清理过期条目。
// 命中 / 未命中次数计入 cache_hits_total / cache_misses_total 指标。

// CacheEntry 缓存条目
type CacheEntry struct {
	Value     interface{}
	ExpiresAt time.Time
}

// DefaultMaxEntries 缓存条目数的默认上限
const DefaultMaxEntries = 10000

// Cache 内存缓存（带 TTL、条目数上限和 Singleflight）
type Cache struct {
	mu   sync.RWMutex
	data map[string]*CacheEntry
	ttl  time.Duration

	// 条目数上限：缓存 key 里带着请求参数（如报价金额），不设上限的话，
	// 一直换参数的请求会让缓存无限增长。超过上限时按写入顺序淘汰最早的条目。
	maxEntries int
	order      []string // 各 key 首次写入的顺序，队头最早

	// Singleflight：防止缓存击穿
	// 同一个 Key 的并发请求只允许一个去加载数据
	flights   map[string]*flight
	flightsMu sync.Mutex
}

// flight 表示一个正在进行的数据加载请求
type flight struct {
	wg  sync.WaitGroup
	val interface{}
	err error
}

// NewCache 创建缓存实例，条目数上限为 DefaultMaxEntries
func NewCache(ttl time.Duration) *Cache {
	return NewCacheWithLimit(ttl, DefaultMaxEntries)
}

// NewCacheWithLimit 创建缓存实例并指定条目数上限
func NewCacheWithLimit(ttl time.Duration, maxEntries int) *Cache {
	c := &Cache{
		data:       make(map[string]*CacheEntry),
		ttl:        ttl,
		maxEntries: maxEntries,
		flights:    make(map[string]*flight),
	}

	// 启动后台清理过期条目的协程
	go c.cleanup()

	return c
}

// Get 获取缓存值
func (c *Cache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	entry, ok := c.data[key]
	c.mu.RUnlock()

	// 过期的条目当作不存在，由后台清理或被新值覆盖
	if !ok || time.Now().After(entry.ExpiresAt) {
		metrics.CacheMissesTotal.Inc()
		return nil, false
	}
	metrics.CacheHitsTotal.Inc()
	return entry.Value, true
}

// Set 设置缓存值
func (c *Cache) Set(key string, value interface{}) {
	c.SetWithTTL(key, value, c.ttl)
}

// SetWithTTL 设置缓存值（自定义 TTL）
func (c *Cache) SetWithTTL(key string, value interface{}, ttl time.Duration) {
	entry := &CacheEntry{Value: value, ExpiresAt: time.Now().Add(ttl)}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.data[key]; !exists {
		c.order = append(c.order, key)
	}
	c.data[key] = entry

	// 超过上限：从最早写入的开始淘汰（队列里已经被删掉的 key 直接跳过）
	for len(c.data) > c.maxEntries && len(c.order) > 0 {
		delete(c.data, c.order[0])
		c.order = c.order[1:]
	}
}

// Len 当前缓存的条目数
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}

// GetOrLoad 获取缓存值，如果不存在则通过 loader 加载
//
// 核心特性：Singleflight 请求合并
//
// 场景：100 个并发请求同时查询 "ETH/USDC" 价格，且缓存已过期
// 没有 Singleflight：100 个请求全部打到 The Graph（击穿）
// 有 Singleflight：只有 1 个请求打到 The Graph，其余 99 个等待复用结果
//
// 实现原理：
//  1. 检查是否有正在进行的相同 Key 的加载请求（flight）
//  2. 如果有，等待其完成并复用结果
//  3. 如果没有，创建新的 flight，执行 loader，通知所有等待者
func (c *Cache) GetOrLoad(key string, loader func() (interface{}, error)) (interface{}, error) {
	// 1. 先检查缓存
	if val, ok := c.Get(key); ok {
		return val, nil
	}

	// 2. Singleflight：检查是否有进行中的加载
	c.flightsMu.Lock()
	if f, ok := c.flights[key]; ok {
		c.flightsMu.Unlock()
		// 等待已有的加载完成
		f.wg.Wait()
		return f.val, f.err
	}

	// 3. 创建新的 flight
	f := &flight{}
	f.wg.Add(1)
	c.flights[key] = f
	c.flightsMu.Unlock()

	// 4. 执行实际的数据加载
	f.val, f.err = loader()
	if f.err == nil {
		// 加载成功，写入缓存
		c.Set(key, f.val)
	}

	// 5. 通知所有等待者
	f.wg.Done()

	// 6. 清理 flight
	c.flightsMu.Lock()
	delete(c.flights, key)
	c.flightsMu.Unlock()

	return f.val, f.err
}

// Delete 删除缓存条目
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, key)
}

// cleanup 后台定期清理过期条目
func (c *Cache) cleanup() {
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		c.mu.Lock()
		for key, entry := range c.data {
			if now.After(entry.ExpiresAt) {
				delete(c.data, key)
			}
		}
		// 淘汰队列里只留还在缓存中的 key
		live := c.order[:0]
		for _, key := range c.order {
			if _, ok := c.data[key]; ok {
				live = append(live, key)
			}
		}
		c.order = live
		c.mu.Unlock()
	}
}
