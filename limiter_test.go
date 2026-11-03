package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTokenBucketLimiterHonorsBurstAndCancellation(t *testing.T) {
	limiter, err := NewTokenBucketLimiter(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := limiter.Allow(context.Background(), "global"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = limiter.Allow(ctx, "global")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Allow() = %v, want context deadline", err)
	}
	if !strings.Contains(err.Error(), "token bucket") {
		t.Fatalf("second Allow() = %v, want explanatory token bucket error", err)
	}
}

func TestSemaphoreLimiterLimitsConcurrentAdmissionsAndReleases(t *testing.T) {
	limiter, err := NewSemaphoreLimiter(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := limiter.Allow(context.Background(), "run-1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := limiter.Allow(ctx, "run-2"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Allow() = %v, want context deadline", err)
	}
	limiter.Release("run-1")
	if err := limiter.Allow(context.Background(), "run-2"); err != nil {
		t.Fatal(err)
	}
}

func TestSemaphoreLimiterCloseAndReleaseConcurrently(t *testing.T) {
	limiter, err := NewSemaphoreLimiter(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := limiter.Allow(context.Background(), "run-1"); err != nil {
		t.Fatal(err)
	}

	const releases = 100
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < releases; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("Release panicked after concurrent Close: %v", recovered)
				}
			}()
			limiter.Release("run-1")
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if err := limiter.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	}()
	close(start)
	wg.Wait()

	for i := 0; i < releases; i++ {
		limiter.Release("run-1")
	}
	if err := limiter.Allow(context.Background(), "run-1"); !errors.Is(err, ErrLimiterClosed) {
		t.Fatalf("Allow() after Close = %v, want %v", err, ErrLimiterClosed)
	}
}

func TestMultiLimiterEnforcesGlobalUserAndToolLimits(t *testing.T) {
	global, _ := NewSemaphoreLimiter(2)
	users, _ := NewKeyedSemaphoreLimiter(1)
	tools, _ := NewKeyedSemaphoreLimiter(1)
	limiter := NewMultiLimiter(global, users, tools)

	if err := limiter.Allow(context.Background(), "alice|search"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := limiter.Allow(ctx, "alice|write"); err == nil {
		t.Fatal("second run for same user was admitted")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := limiter.Allow(ctx, "bob|search"); err == nil {
		t.Fatal("second run for same tool was admitted")
	}
	limiter.Release("alice|search")
	if err := limiter.Allow(context.Background(), "bob|write"); err != nil {
		t.Fatalf("admission after release = %v", err)
	}
}
