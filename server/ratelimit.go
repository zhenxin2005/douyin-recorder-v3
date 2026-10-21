// 内存限流器：滑动窗口，按 key（设备/IP）计数
package main

import (
	"sync"
	"time"
)

type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	limit    int           // 窗口内允许的最大次数
	window   time.Duration // 窗口时长
}

type bucket struct {
	count     int
	windowEnd time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		buckets: map[string]*bucket{},
		limit:   limit,
		window:  window,
	}
	go rl.cleanupLoop()
	return rl
}

// Allow 判断 key 是否允许通过本次请求
func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[key]
	if !ok || now.After(b.windowEnd) {
		rl.buckets[key] = &bucket{count: 1, windowEnd: now.Add(rl.window)}
		return true
	}
	b.count++
	return b.count <= rl.limit
}

func (rl *rateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.window)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		now := time.Now()
		for k, v := range rl.buckets {
			if now.After(v.windowEnd) {
				delete(rl.buckets, k)
			}
		}
		rl.mu.Unlock()
	}
}
