package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Limiter interface {
	Allow(context.Context, string) error
}

var (
	ErrLimiterClosed = errors.New("limiter is closed")
	ErrRateLimited   = errors.New("rate limit exceeded")
	ErrConcurrency   = errors.New("concurrency limit exceeded")
)

// TokenBucketLimiter admits a burst immediately and replenishes tokens at a
// steady rate. It keeps one bounded bucket, so arbitrary request keys do not
// cause unbounded memory growth.
type TokenBucketLimiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	closed bool
	done   chan struct{}
}

var _ Limiter = (*TokenBucketLimiter)(nil)

func NewTokenBucketLimiter(ratePerSecond float64, burst int) (*TokenBucketLimiter, error) {
	if ratePerSecond <= 0 {
		return nil, fmt.Errorf("token bucket rate must be positive")
	}
	if burst <= 0 {
		return nil, fmt.Errorf("token bucket burst must be positive")
	}
	now := time.Now()
	return &TokenBucketLimiter{rate: ratePerSecond, burst: float64(burst), tokens: float64(burst), last: now, done: make(chan struct{})}, nil
}

func (l *TokenBucketLimiter) Allow(ctx context.Context, _ string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return ErrLimiterClosed
		}
		now := time.Now()
		l.tokens = minFloat(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		if wait <= 0 {
			wait = time.Nanosecond
		}
		done := l.done
		l.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return fmt.Errorf("token bucket: %w", ctx.Err())
		case <-done:
			stopTimer(timer)
			return ErrLimiterClosed
		case <-timer.C:
		}
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (l *TokenBucketLimiter) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	close(l.done)
	return nil
}

type semaphoreKey func(string) string

// SemaphoreLimiter limits all keys together. Release must be called after the
// admitted operation completes.
type SemaphoreLimiter struct {
	mu     sync.Mutex
	limit  int
	key    semaphoreKey
	active map[string]int
	notify chan struct{}
	closed bool
	done   chan struct{}
}

var _ Limiter = (*SemaphoreLimiter)(nil)

func NewSemaphoreLimiter(limit int) (*SemaphoreLimiter, error) {
	return newSemaphoreLimiter(limit, func(string) string { return "global" })
}

func NewKeyedSemaphoreLimiter(limit int) (*SemaphoreLimiter, error) {
	return newSemaphoreLimiter(limit, func(key string) string { return key })
}

func newSemaphoreLimiter(limit int, key semaphoreKey) (*SemaphoreLimiter, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("semaphore limit must be positive")
	}
	return &SemaphoreLimiter{limit: limit, key: key, active: make(map[string]int), notify: make(chan struct{}), done: make(chan struct{})}, nil
}

func (l *SemaphoreLimiter) Allow(ctx context.Context, rawKey string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	key := l.key(rawKey)
	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return ErrLimiterClosed
		}
		if l.active[key] < l.limit {
			l.active[key]++
			l.mu.Unlock()
			return nil
		}
		notify := l.notify
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return fmt.Errorf("concurrency limit: %w", ctx.Err())
		case <-l.done:
			return ErrLimiterClosed
		case <-notify:
		}
	}
}

func (l *SemaphoreLimiter) Release(rawKey string) {
	key := l.key(rawKey)
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	if l.active[key] > 1 {
		l.active[key]--
	} else {
		delete(l.active, key)
	}
	close(l.notify)
	l.notify = make(chan struct{})
	l.mu.Unlock()
}

func (l *SemaphoreLimiter) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	close(l.done)
	close(l.notify)
	return nil
}

// MultiLimiter combines global, user, and tool semaphore boundaries. Keys use
// the stable form "user|tool"; a missing separator applies the same key to all
// scopes. Failed partial admissions are rolled back.
type MultiLimiter struct {
	global Limiter
	users  Limiter
	tools  Limiter
}

func NewMultiLimiter(global, users, tools Limiter) *MultiLimiter {
	return &MultiLimiter{global: global, users: users, tools: tools}
}

func (l *MultiLimiter) Allow(ctx context.Context, key string) error {
	parts := strings.SplitN(key, "|", 2)
	user, tool := parts[0], parts[0]
	if len(parts) == 2 {
		tool = parts[1]
	}
	if l.global != nil {
		if err := l.global.Allow(ctx, key); err != nil {
			return err
		}
	}
	if l.users != nil {
		if err := l.users.Allow(ctx, user); err != nil {
			l.release(l.global, key)
			return err
		}
	}
	if l.tools != nil {
		if err := l.tools.Allow(ctx, tool); err != nil {
			l.release(l.users, user)
			l.release(l.global, key)
			return err
		}
	}
	return nil
}

func (l *MultiLimiter) Release(key string) {
	parts := strings.SplitN(key, "|", 2)
	user, tool := parts[0], parts[0]
	if len(parts) == 2 {
		tool = parts[1]
	}
	l.release(l.tools, tool)
	l.release(l.users, user)
	l.release(l.global, key)
}

func (l *MultiLimiter) release(limiter Limiter, key string) {
	if releaser, ok := limiter.(interface{ Release(string) }); ok {
		releaser.Release(key)
	}
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
