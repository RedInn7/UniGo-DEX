package cache

import (
	"sync"
	"time"
)

// cache.go — 缓存层（含 Singleflight 请求合并 + 缓存预热）
//
// === 缓存雪崩防护 ===
//
// 当 Redis 宕机重启或大量 Key 同时过期，后端请求可能瞬间直击 The Graph 或底层节点。
// 为此做了两层防护：
//
//   1. Singleflight（请求合并）
//      使用类似 golang.org/x/sync/singleflight 的机制：
//      当多个请求同时查询同一 Key 且缓存失效时，仅允许 1 个请求实际访问后端，
//      其余请求等待并复用结果。从源头避免缓存击穿。
//
//   2. 缓存预热（Warmup）
//      系统启动阶段执行预热脚本，优先加载 Top N 交易对的流动性与价格数据，
//      降低冷启动阶段的突发流量压力。
//
// === 本地实现 ===
//
// 当前使用内存缓存（sync.Map + TTL），生产环境可替换为 Redis 客户端。
// 接口保持一致，替换时无需修改业务逻辑。

// CacheEntry 缓存条目
type CacheEntry struct {
	Value     interface{}
	ExpiresAt time.Time
}

// Cache 内存缓存（带 TTL 和 Singleflight）
type Cache struct {
	data    sync.Map
	ttl     time.Duration

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
		return nil, false
	}

	entry := value.(*CacheEntry)
	if time.Now().After(entry.ExpiresAt) {
		// 已过期，删除
		c.data.Delete(key)
		return nil, false
	}

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
//   1. 检查是否有正在进行的相同 Key 的加载请求（flight）
//   2. 如果有，等待其完成并复用结果
//   3. 如果没有，创建新的 flight，执行 loader，通知所有等待者
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

// WarmupConfig 缓存预热配置
type WarmupConfig struct {
	// TopPairs 预热的交易对列表
	TopPairs []struct {
		Token0 string
		Token1 string
		Fee    uint32
	}
}

// Warmup 缓存预热
//
// 在系统启动阶段执行：
//   1. 加载 Top N 交易对的池子状态
//   2. 预计算常用价格和流动性数据
//   3. 写入缓存，避免冷启动时的突发请求
//
// 预热策略：
//   - 优先加载交易量最大的交易对（如 ETH/USDC, ETH/USDT）
//   - 预热数据使用较长的 TTL（5 分钟 vs 正常 30 秒）
//   - 预热失败不阻塞启动流程
func (c *Cache) Warmup(config WarmupConfig, loader func(token0, token1 string, fee uint32) (interface{}, error)) int {
	loaded := 0
	for _, pair := range config.TopPairs {
		key := cacheKey(pair.Token0, pair.Token1, pair.Fee)
		val, err := loader(pair.Token0, pair.Token1, pair.Fee)
		if err != nil {
			continue
		}
		// 预热数据使用较长 TTL
		c.SetWithTTL(key, val, c.ttl*10)
		loaded++
	}
	return loaded
}

// cacheKey 生成缓存 Key
func cacheKey(token0, token1 string, fee uint32) string {
	return token0 + ":" + token1 + ":" + string(rune(fee))
}
