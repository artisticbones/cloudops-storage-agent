package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	EventRunStarted   = "run_started"
	EventModelStarted = "model_started"
	EventToolStarted  = "tool_started"
	EventToolFinished = "tool_finished"
	EventRunCompleted = "run_completed"
)

// RunEvent is the durable, client-facing part of an Agent run's lifecycle.
// Sequence is assigned by the service and is strictly increasing per run.
type RunEvent struct {
	Sequence  uint64    `json:"sequence"`
	Type      string    `json:"type"`
	RunID     string    `json:"runId"`
	SessionID string    `json:"sessionId"`
	Step      int       `json:"step,omitempty"`
	Tool      string    `json:"tool,omitempty"`
	Status    RunStatus `json:"status,omitempty"`
	Result    string    `json:"result,omitempty"`
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at"`
}

type eventBroker struct {
	mu          sync.Mutex
	events      []RunEvent
	subscribers map[int]*eventSubscription
	nextID      int
	terminal    bool
	closed      bool
}

type eventSubscription struct {
	broker *eventBroker
	ch     chan RunEvent
	stop   chan struct{}
	once   sync.Once
}

func newEventBroker() *eventBroker {
	return &eventBroker{subscribers: make(map[int]*eventSubscription)}
}

func (b *eventBroker) publish(event RunEvent) RunEvent {
	b.mu.Lock()
	event.Sequence = uint64(len(b.events) + 1)
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	b.events = append(b.events, event)
	if event.Type == EventRunCompleted {
		b.terminal = true
	}
	for _, subscriber := range b.subscribers {
		select {
		case subscriber.ch <- event:
		default:
			// The complete history remains available through ReadEvents, so a
			// slow subscriber can reconnect with its last received sequence.
		}
	}
	var subscribers []*eventSubscription
	if b.terminal {
		subscribers = make([]*eventSubscription, 0, len(b.subscribers))
		for _, subscriber := range b.subscribers {
			subscribers = append(subscribers, subscriber)
		}
	}
	b.mu.Unlock()
	for _, subscriber := range subscribers {
		subscriber.unsubscribe()
	}
	return event
}

func (b *eventBroker) read(after uint64) []RunEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make([]RunEvent, 0, len(b.events))
	for _, event := range b.events {
		if event.Sequence > after {
			result = append(result, event)
		}
	}
	return result
}

func (b *eventBroker) subscribe(ctx context.Context, after uint64) (<-chan RunEvent, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	b.mu.Lock()
	if b.terminal || b.closed {
		channel := make(chan RunEvent, len(b.events))
		for _, event := range b.events {
			if event.Sequence > after {
				channel <- event
			}
		}
		close(channel)
		b.mu.Unlock()
		return channel, func() {}, nil
	}
	// Reserve room for the complete replay plus a small live-event window. This
	// keeps replay writes non-blocking even when the caller has not started
	// consuming the stream yet, so the broker mutex is never held waiting on a
	// slow subscriber.
	channel := make(chan RunEvent, len(b.events)+128)
	for _, event := range b.events {
		if event.Sequence > after {
			channel <- event
		}
	}
	if err := ctx.Err(); err != nil {
		close(channel)
		b.mu.Unlock()
		return channel, func() {}, nil
	}
	id := b.nextID
	b.nextID++
	subscription := &eventSubscription{
		broker: b,
		ch:     channel,
		stop:   make(chan struct{}),
	}
	b.subscribers[id] = subscription
	b.mu.Unlock()
	if done := ctx.Done(); done != nil {
		go func() {
			select {
			case <-done:
				subscription.unsubscribe()
			case <-subscription.stop:
			}
		}()
	}
	return channel, subscription.unsubscribe, nil
}

func (s *eventSubscription) unsubscribe() {
	s.once.Do(func() {
		s.broker.mu.Lock()
		for id, current := range s.broker.subscribers {
			if current == s {
				delete(s.broker.subscribers, id)
				break
			}
		}
		close(s.ch)
		s.broker.mu.Unlock()
		close(s.stop)
	})
}

func (b *eventBroker) close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	subscribers := make([]*eventSubscription, 0, len(b.subscribers))
	for _, subscriber := range b.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	b.mu.Unlock()
	for _, subscriber := range subscribers {
		subscriber.unsubscribe()
	}
}

func eventRunNotFound(runID string) error {
	return fmt.Errorf("run not found: %s", runID)
}
