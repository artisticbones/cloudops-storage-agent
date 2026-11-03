package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var ErrRunQueueClosed = errors.New("run queue is closed")

// RunQueue is the admission boundary for asynchronous runs.
type RunQueue interface {
	Enqueue(context.Context, Run) error
	Dequeue(context.Context) (Run, error)
	Close() error
}

// BoundedRunQueue is a FIFO queue with no background goroutines. Waiters are
// woken by state-change notifications, context cancellation, or Close.
type BoundedRunQueue struct {
	mu     sync.Mutex
	items  []Run
	cap    int
	notify chan struct{}
	closed bool
	done   chan struct{}
}

var _ RunQueue = (*BoundedRunQueue)(nil)

func NewBoundedRunQueue(capacity int) (*BoundedRunQueue, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("run queue capacity must be positive")
	}
	return &BoundedRunQueue{cap: capacity, notify: make(chan struct{}), done: make(chan struct{})}, nil
}

// NewRunQueue is the concise constructor used by service adapters.
func NewRunQueue(capacity int) (*BoundedRunQueue, error) { return NewBoundedRunQueue(capacity) }

func (q *BoundedRunQueue) Enqueue(ctx context.Context, run Run) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return ErrRunQueueClosed
		}
		if len(q.items) < q.cap {
			q.items = append(q.items, run)
			q.signalLocked()
			q.mu.Unlock()
			return nil
		}
		notify := q.notify
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-q.done:
			return ErrRunQueueClosed
		case <-notify:
		}
	}
}

func (q *BoundedRunQueue) Dequeue(ctx context.Context) (Run, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			run := q.items[0]
			q.items[0] = Run{}
			q.items = q.items[1:]
			q.signalLocked()
			q.mu.Unlock()
			return run, nil
		}
		if q.closed {
			q.mu.Unlock()
			return Run{}, ErrRunQueueClosed
		}
		notify := q.notify
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			return Run{}, ctx.Err()
		case <-q.done:
			return Run{}, ErrRunQueueClosed
		case <-notify:
		}
	}
}

func (q *BoundedRunQueue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	q.closed = true
	close(q.done)
	q.signalLocked()
	return nil
}

func (q *BoundedRunQueue) signalLocked() {
	close(q.notify)
	q.notify = make(chan struct{})
}
