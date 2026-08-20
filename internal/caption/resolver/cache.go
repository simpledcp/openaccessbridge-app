package resolver

import (
	"container/list"
	"strings"
	"sync"
)

// Cache is a bounded, in-memory, LRU transient cache for fetched (post-unwrap)
// timed-text XML (§5.3). It exists only in RAM and is Cleared at reel and show
// boundaries — nothing is ever persisted (guardrail #5). A maximum byte size caps
// memory so a long multi-language feature cannot exhaust RAM.
type Cache struct {
	mu    sync.Mutex
	max   int64
	used  int64
	items map[string]*list.Element
	order *list.List // front = most recently used
}

type cacheEntry struct {
	key  string
	data []byte
}

// NewCache builds a cache with a maximum total byte size. maxBytes <= 0 means
// unbounded (not recommended on the appliance).
func NewCache(maxBytes int64) *Cache {
	return &Cache{
		max:   maxBytes,
		items: make(map[string]*list.Element),
		order: list.New(),
	}
}

// Get returns the cached bytes for key, marking it most-recently-used.
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheEntry).data, true
}

// Put stores bytes for key, evicting least-recently-used entries until the total
// fits within max. An item larger than the entire budget is not cached (it is
// fetched on demand each time instead).
func (c *Cache) Put(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	size := int64(len(data))
	if c.max > 0 && size > c.max {
		// Too big to cache; drop any existing entry for the key.
		c.removeLocked(key)
		return
	}
	if el, ok := c.items[key]; ok {
		old := el.Value.(*cacheEntry)
		c.used += size - int64(len(old.data))
		old.data = data
		c.order.MoveToFront(el)
	} else {
		el := c.order.PushFront(&cacheEntry{key: key, data: data})
		c.items[key] = el
		c.used += size
	}
	c.evictLocked()
}

func (c *Cache) evictLocked() {
	if c.max <= 0 {
		return
	}
	for c.used > c.max {
		back := c.order.Back()
		if back == nil {
			return
		}
		ent := back.Value.(*cacheEntry)
		c.used -= int64(len(ent.data))
		c.order.Remove(back)
		delete(c.items, ent.key)
	}
}

func (c *Cache) removeLocked(key string) {
	if el, ok := c.items[key]; ok {
		ent := el.Value.(*cacheEntry)
		c.used -= int64(len(ent.data))
		c.order.Remove(el)
		delete(c.items, key)
	}
}

// RetainOnly drops every entry whose key does not start with one of the given
// prefixes. This is the reel-boundary half of the §5.3 eviction policy: the reels
// still inside the fetch-ahead window keep their assets — evicting those would
// force a re-fetch at exactly the moment the boundary is crossed — while every
// reel already played, and every other show's content, is dropped. An empty
// prefix list clears the cache.
func (c *Cache) RetainOnly(prefixes []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.items {
		keep := false
		for _, p := range prefixes {
			if strings.HasPrefix(key, p) {
				keep = true
				break
			}
		}
		if !keep {
			c.removeLocked(key)
		}
	}
}

// Clear empties the cache (called at show boundaries — §5.3).
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*list.Element)
	c.order.Init()
	c.used = 0
}

// Used returns the current cache size in bytes.
func (c *Cache) Used() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// Len returns the number of cached items.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
