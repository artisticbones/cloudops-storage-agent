package main

import (
	"context"
	"fmt"
	"sync"
)

type IdempotencyStore interface {
	Get(context.Context, string) (string, bool, error)
	Put(context.Context, string, string) error
}

// IdempotencyExecutor atomically performs the check/execute/complete
// sequence. The bool result is true when value was already completed.
type IdempotencyExecutor interface {
	ExecuteOnce(context.Context, string, func(context.Context) (string, error)) (string, bool, error)
}

// MemoryIdempotencyStore is process-local and intended as the default runtime
// store. A durable implementation can be injected without changing Agent.
type MemoryIdempotencyStore struct {
	mu     sync.Mutex
	values map[string]string
	locks  map[string]*sync.Mutex
}

var fallbackIdempotencyLocks sync.Map

func NewMemoryIdempotencyStore() *MemoryIdempotencyStore {
	return &MemoryIdempotencyStore{values: make(map[string]string), locks: make(map[string]*sync.Mutex)}
}

var _ IdempotencyStore = (*MemoryIdempotencyStore)(nil)
var _ IdempotencyExecutor = (*MemoryIdempotencyStore)(nil)

func (s *MemoryIdempotencyStore) Get(ctx context.Context, key string) (string, bool, error) {
	if err := idempotencyContextErr(ctx); err != nil {
		return "", false, err
	}
	if key == "" {
		return "", false, fmt.Errorf("idempotency key cannot be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[key]
	return value, ok, nil
}

func (s *MemoryIdempotencyStore) Put(ctx context.Context, key, value string) error {
	if err := idempotencyContextErr(ctx); err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("idempotency key cannot be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	return nil
}

func (s *MemoryIdempotencyStore) ExecuteOnce(ctx context.Context, key string, fn func(context.Context) (string, error)) (string, bool, error) {
	if err := idempotencyContextErr(ctx); err != nil {
		return "", false, err
	}
	if key == "" {
		return "", false, fmt.Errorf("idempotency key cannot be empty")
	}
	if fn == nil {
		return "", false, fmt.Errorf("idempotency callback cannot be nil")
	}

	keyLock := s.lockFor(key)
	keyLock.Lock()
	defer keyLock.Unlock()

	s.mu.Lock()
	value, ok := s.values[key]
	s.mu.Unlock()
	if ok {
		return value, true, nil
	}
	value, err := fn(ctx)
	if err != nil {
		return "", false, err
	}
	if err := idempotencyContextErr(ctx); err != nil {
		return "", false, err
	}
	s.mu.Lock()
	s.values[key] = value
	s.mu.Unlock()
	return value, false, nil
}

func (s *MemoryIdempotencyStore) lockFor(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lock, ok := s.locks[key]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	s.locks[key] = lock
	return lock
}

func lockFallbackIdempotencyKey(key string) func() {
	lockValue, _ := fallbackIdempotencyLocks.LoadOrStore(key, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func idempotencyContextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
