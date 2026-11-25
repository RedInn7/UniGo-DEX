package cache

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetSetAndExpiry(t *testing.T) {
	c := NewCache(30 * time.Millisecond)
	if _, ok := c.Get("a"); ok {
		t.Fatal("empty cache should miss")
	}
	c.Set("a", 1)
	if v, ok := c.Get("a"); !ok || v.(int) != 1 {
		t.Fatalf("got %v %v", v, ok)
	}
	c.SetWithTTL("b", 2, time.Hour)
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.Get("a"); ok {
		t.Fatal("expired entry should miss")
	}
	if v, ok := c.Get("b"); !ok || v.(int) != 2 {
		t.Fatal("entry with a longer TTL should survive")
	}
	c.Delete("b")
	if _, ok := c.Get("b"); ok {
		t.Fatal("deleted entry should miss")
	}
}

// 不断换 key 写入时条目数不超过上限，最早写入的先被淘汰
func TestMaxEntries(t *testing.T) {
	c := NewCacheWithLimit(time.Hour, 100)
	for i := 0; i < 1000; i++ {
		c.Set(fmt.Sprintf("k%d", i), i)
	}
	if c.Len() != 100 {
		t.Fatalf("len = %d, want 100", c.Len())
	}
	if _, ok := c.Get("k899"); ok {
		t.Fatal("old entry should have been evicted")
	}
	if v, ok := c.Get("k999"); !ok || v.(int) != 999 {
		t.Fatal("newest entry should be present")
	}
	if _, ok := c.Get("k900"); !ok {
		t.Fatal("the 100 most recent entries should be kept")
	}

	// 覆盖已有的 key 不占新名额
	for i := 0; i < 500; i++ {
		c.Set("k950", i)
	}
	if c.Len() != 100 {
		t.Fatalf("len after overwrites = %d, want 100", c.Len())
	}
}

// 同一个 key 的并发加载只执行一次，其余请求复用结果
func TestGetOrLoadSingleflight(t *testing.T) {
	c := NewCache(time.Hour)
	var loads atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.GetOrLoad("key", func() (interface{}, error) {
				loads.Add(1)
				time.Sleep(20 * time.Millisecond)
				return "value", nil
			})
			if err != nil || v.(string) != "value" {
				t.Errorf("got %v %v", v, err)
			}
		}()
	}
	wg.Wait()
	if loads.Load() != 1 {
		t.Fatalf("loader ran %d times, want 1", loads.Load())
	}

	// 加载失败不缓存，下次重新加载
	boom := errors.New("boom")
	if _, err := c.GetOrLoad("bad", func() (interface{}, error) { return nil, boom }); err != boom {
		t.Fatalf("err = %v", err)
	}
	if v, err := c.GetOrLoad("bad", func() (interface{}, error) { return 7, nil }); err != nil || v.(int) != 7 {
		t.Fatalf("retry after failure: %v %v", v, err)
	}
}
