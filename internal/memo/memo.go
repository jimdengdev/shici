// Package memo 提供带 TTL 抖动、单飞回源、SWR 与 LRU 容量控制的进程内缓存。
//
// 站点内容极少变化，采用「长 TTL + 写入失效 + 手动清空（SIGUSR1）」策略：
//   - 命中未过期：直接返回；
//   - 命中已过期：先返回旧值（stale-while-revalidate），后台异步回源；
//   - 未命中：singleflight 合并并发请求，只回源一次；
//   - 后台写入 / SIGUSR1：FlushAll() 一键清空所有已登记缓存。
//
// 容量用「条数 + 字节」双上限做 LRU 淘汰，长 TTL 下不会无限增长。
package memo

import (
	"container/list"
	"log"
	"math/rand"
	"sync"
	"time"
)

// ── 全局登记：统一清缓存入口 ────────────────────────────

var (
	regMu    sync.Mutex
	flushers []Flusher
)

// Flusher 可被统一清空的缓存。
type Flusher interface {
	Name() string
	Flush()
}

// Register 把缓存登记到全局清空列表（New 自动调用）。
func Register(f Flusher) {
	regMu.Lock()
	defer regMu.Unlock()
	flushers = append(flushers, f)
}

// FlushAll 清空所有已登记缓存（后台写入 / SIGUSR1 时调用）。
func FlushAll() {
	regMu.Lock()
	fs := append([]Flusher(nil), flushers...)
	regMu.Unlock()
	for _, f := range fs {
		f.Flush()
	}
	if len(fs) > 0 {
		log.Printf("[cache] 已清空 %d 个缓存", len(fs))
	}
}

// ── 通用缓存 ───────────────────────────────────────────

type entry[K comparable, V any] struct {
	key     K
	value   V
	expires time.Time
	size    int64
	elem    *list.Element
}

// call 单飞：同 key 的并发回源只执行一次。
type call[V any] struct {
	wg  sync.WaitGroup
	val V
	err error
}

// Cache 通用缓存。
type Cache[K comparable, V any] struct {
	name   string
	ttl    time.Duration
	maxN   int
	maxB   int64
	sizeOf func(V) int64

	mu    sync.Mutex
	items map[K]*entry[K, V]
	lru   *list.List // 队首 = 最近使用
	bytes int64
	calls map[K]*call[V]
}

// New 创建并登记一个缓存。
//
//	name   日志/诊断用名称
//	ttl    基础过期时间（Set 时自动加 ≤12.5% 抖动）
//	maxN   条数上限（0 = 不限）
//	maxB   字节上限（0 = 不限；sizeOf 为 nil 时忽略）
//	sizeOf 条目大小估算函数（仅 maxB>0 时需要）
func New[K comparable, V any](name string, ttl time.Duration, maxN int, maxB int64, sizeOf func(V) int64) *Cache[K, V] {
	c := &Cache[K, V]{
		name: name, ttl: ttl, maxN: maxN, maxB: maxB, sizeOf: sizeOf,
		items: make(map[K]*entry[K, V]),
		lru:   list.New(),
		calls: make(map[K]*call[V]),
	}
	Register(c)
	return c
}

func (c *Cache[K, V]) Name() string { return c.name }

// Get 带回源读取。
//
//   - 未过期：直接返回；
//   - 已过期但存在：返回旧值，同时后台异步回源（SWR，调用方零等待）；
//   - 不存在：singleflight 回源（并发同 key 只执行一次 load）。
func (c *Cache[K, V]) Get(key K, load func() (V, error)) (V, error) {
	c.mu.Lock()
	if e, ok := c.items[key]; ok {
		c.lru.MoveToFront(e.elem)
		v := e.value
		expired := time.Now().After(e.expires)
		c.mu.Unlock()
		if !expired {
			return v, nil
		}
		go c.load(key, load)
		return v, nil
	}
	c.mu.Unlock()
	return c.load(key, load)
}

func (c *Cache[K, V]) load(key K, loadFn func() (V, error)) (V, error) {
	c.mu.Lock()
	if cl, ok := c.calls[key]; ok {
		c.mu.Unlock()
		cl.wg.Wait()
		return cl.val, cl.err
	}
	cl := &call[V]{}
	cl.wg.Add(1)
	c.calls[key] = cl
	c.mu.Unlock()

	cl.val, cl.err = loadFn()
	if cl.err == nil {
		c.Set(key, cl.val)
	}
	c.mu.Lock()
	delete(c.calls, key)
	c.mu.Unlock()
	cl.wg.Done()
	return cl.val, cl.err
}

// Peek 只读未过期值（不回源）。
func (c *Cache[K, V]) Peek(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok && time.Now().Before(e.expires) {
		c.lru.MoveToFront(e.elem)
		return e.value, true
	}
	var zero V
	return zero, false
}

// Set 写入条目。
func (c *Cache[K, V]) Set(key K, value V) {
	var size int64
	if c.sizeOf != nil {
		size = c.sizeOf(value)
	}
	exp := time.Now().Add(c.ttl + time.Duration(rand.Int63n(int64(c.ttl/8)+1)))

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.bytes += size - e.size
		e.value, e.size, e.expires = value, size, exp
		c.lru.MoveToFront(e.elem)
	} else {
		e := &entry[K, V]{key: key, value: value, expires: exp, size: size}
		e.elem = c.lru.PushFront(e)
		c.items[key] = e
		c.bytes += size
	}
	c.evictLocked()
}

// evictLocked 超过条数/字节上限时从队尾（最久未用）淘汰。持锁调用。
func (c *Cache[K, V]) evictLocked() {
	for (c.maxN > 0 && len(c.items) > c.maxN) || (c.maxB > 0 && c.bytes > c.maxB) {
		back := c.lru.Back()
		if back == nil {
			return
		}
		e := back.Value.(*entry[K, V])
		c.lru.Remove(back)
		delete(c.items, e.key)
		c.bytes -= e.size
	}
}

// Flush 清空全部条目。
func (c *Cache[K, V]) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[K]*entry[K, V])
	c.lru.Init()
	c.bytes = 0
}

// Len 当前条目数（诊断用）。
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
