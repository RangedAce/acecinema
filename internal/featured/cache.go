package featured

import (
	"sync"
	"time"
)

type cacheEntry struct {
	items     []Item
	expiresAt time.Time
}

type cacheStore struct {
	mu    sync.Mutex
	items map[string]cacheEntry
}

func newCache() *cacheStore {
	return &cacheStore{items: make(map[string]cacheEntry)}
}

func (c *cacheStore) Get(key string, now time.Time) ([]Item, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[key]
	if !ok {
		return nil, false
	}
	if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
		delete(c.items, key)
		return nil, false
	}
	out := make([]Item, len(entry.items))
	copy(out, entry.items)
	return out, true
}

func (c *cacheStore) Set(key string, items []Item, ttl time.Duration, now time.Time) {
	if ttl <= 0 {
		return
	}
	out := make([]Item, len(items))
	copy(out, items)
	c.mu.Lock()
	c.items[key] = cacheEntry{
		items:     out,
		expiresAt: now.Add(ttl),
	}
	c.mu.Unlock()
}
