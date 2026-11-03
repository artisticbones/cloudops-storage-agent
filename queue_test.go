package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBoundedRunQueueWaitsForSpaceAndPreservesOrder(t *testing.T) {
	q, err := NewBoundedRunQueue(1)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	first := NewRun("session")
	second := NewRun("session")
	if err := q.Enqueue(context.Background(), first); err != nil {
		t.Fatal(err)
	}

	enqueued := make(chan error, 1)
	go func() { enqueued <- q.Enqueue(context.Background(), second) }()
	select {
	case err := <-enqueued:
		t.Fatalf("Enqueue completed while queue was full: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	got, err := q.Dequeue(context.Background())
	if err != nil || got.ID != first.ID {
		t.Fatalf("Dequeue() = %+v, %v; want first run", got, err)
	}
	if err := <-enqueued; err != nil {
		t.Fatal(err)
	}
	got, err = q.Dequeue(context.Background())
	if err != nil || got.ID != second.ID {
		t.Fatalf("Dequeue() = %+v, %v; want second run", got, err)
	}
}

func TestBoundedRunQueueCancellationAndCloseWakeWaiters(t *testing.T) {
	q, err := NewBoundedRunQueue(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), NewRun("session")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	enqueueErr := make(chan error, 1)
	go func() { enqueueErr <- q.Enqueue(ctx, NewRun("session")) }()
	cancel()
	if err := <-enqueueErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("Enqueue cancellation error = %v, want context.Canceled", err)
	}
	if _, err := q.Dequeue(context.Background()); err != nil {
		t.Fatal(err)
	}

	dequeueCtx, dequeueCancel := context.WithCancel(context.Background())
	dequeueCancel()
	if _, err := q.Dequeue(dequeueCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Dequeue cancellation error = %v, want context.Canceled", err)
	}

	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), NewRun("session")); !errors.Is(err, ErrRunQueueClosed) {
		t.Fatalf("Enqueue after Close() = %v, want ErrRunQueueClosed", err)
	}
	if _, err := q.Dequeue(context.Background()); !errors.Is(err, ErrRunQueueClosed) {
		t.Fatalf("Dequeue after Close() = %v, want ErrRunQueueClosed", err)
	}
}
