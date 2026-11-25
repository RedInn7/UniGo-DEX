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
// 内存缓存（sync.Map + TTL），后台协程定期清理过期条目。
// 命中 / 未命中次数计入 cache_hits_total / cache_misses_total 指标。

// CacheEntry 缓存条目
type CacheEntry struct {
	Value     interface{}
	ExpiresAt time.Time
}

// Cache 内存缓存（带 TTL 和 Singleflight）
type Cache struct {
	data sync.Map
	ttl  time.Duration

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

// NewCache 创建缓存实例
func NewCache(ttl time.Duration) *Cache {
	c := &Cache{
		ttl:     ttl,
		flights: make(map[string]*flight),
	}

	// 启动后台清理过期条目的协程
	go c.cleanup()

	return c
}

// Get 获取缓存值
func (c *Cache) Get(key string) (interface{}, bool) {
	value, ok := c.data.Load(key)
	if !ok {
		metrics.CacheMissesTotal.Inc()
		return nil, false
	}

	entry := value.(*CacheEntry)
	if time.Now().After(entry.ExpiresAt) {
		// 已过期，删除
		c.data.Delete(key)
		metrics.CacheMissesTotal.Inc()
		return nil, false
	}

	metrics.CacheHitsTotal.Inc()
	return entry.Value, true
}

// Set 设置缓存值
func (c *Cache) Set(key string, value interface{}) {
	c.data.Store(key, &CacheEntry{
		Value:     value,
		ExpiresAt: time.Now().Add(c.ttl),
	})
}

// SetWithTTL 设置缓存值（自定义 TTL）
func (c *Cache) SetWithTTL(key string, value interface{}, ttl time.Duration) {
	c.data.Store(key, &CacheEntry{
		Value:     value,
		ExpiresAt: time.Now().Add(ttl),
	})
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
	c.data.Delete(key)
}

// cleanup 后台定期清理过期条目
func (c *Cache) cleanup() {
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()

	for range ticker.C {
		c.data.Range(func(key, value interface{}) bool {
			entry := value.(*CacheEntry)
			if time.Now().After(entry.ExpiresAt) {
				c.data.Delete(key)
			}
			return true
		})
	}
}
