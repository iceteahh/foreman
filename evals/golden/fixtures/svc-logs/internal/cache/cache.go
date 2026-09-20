// Package cache is a tiny in-memory key/value store.
package cache

// Cache maps a key to a price in cents.
//
// It is NOT safe for concurrent use: every method touches the map directly.
type Cache struct {
	m map[string]int64
}

// New returns an empty cache.
func New() *Cache { return &Cache{m: map[string]int64{}} }

// Get returns the cached value.
func (c *Cache) Get(k string) (int64, bool) {
	v, ok := c.m[k]
	return v, ok
}

// Set stores a value.
func (c *Cache) Set(k string, v int64) { c.m[k] = v }
