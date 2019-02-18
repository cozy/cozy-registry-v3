// Package lru implements a redis cache.
package lru

import (
	"time"

	"github.com/go-redis/redis"
)

// Cache is an redis cache.
type RedisCache struct {
	TTL   time.Duration
	cache *redis.Client
}

// New creates a new Cache.
// If maxEntries is zero, the cache has no limit and it's assumed
// that eviction is done by the caller.
func NewRedisCache(ttl time.Duration, client *redis.Client) *RedisCache {
	return &RedisCache{
		TTL:   ttl,
		cache: client,
	}
}

// Add adds a value to the cache.
func (c *RedisCache) Add(key Key, value Value) {
	c.cache.Set(key.String(), []byte(value), c.TTL)
}

// Get looks up a key's value from the cache.
func (c *RedisCache) Get(key Key) (value Value, ok bool) {
	if val, err := c.cache.Get(key.String()).Result(); err == nil {
		return []byte(val), true
	}
	return nil, false
}

// Remove removes the provided key from the cache.
func (c *RedisCache) Remove(key Key) {
	c.cache.Del(key.String())
}

var _ Cache = (*RedisCache)(nil)
