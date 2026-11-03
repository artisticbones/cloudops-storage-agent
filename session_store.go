package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// CheckpointStore persists the latest recovery snapshot keyed by run ID.
type CheckpointStore interface {
	Save(context.Context, Checkpoint) error
	Load(context.Context, string) (Checkpoint, error)
}

var ErrCheckpointNotFound = errors.New("checkpoint not found")

// MemoryCheckpointStore is a concurrency-safe in-memory CheckpointStore.
// It is intentionally process-local; a durable store belongs to a later task.
type MemoryCheckpointStore struct {
	mu          sync.RWMutex
	checkpoints map[string]Checkpoint
}

func NewMemoryCheckpointStore() *MemoryCheckpointStore {
	return &MemoryCheckpointStore{checkpoints: make(map[string]Checkpoint)}
}

var _ CheckpointStore = (*MemoryCheckpointStore)(nil)

func (s *MemoryCheckpointStore) Save(ctx context.Context, checkpoint Checkpoint) error {
	if err := checkpointContextErr(ctx); err != nil {
		return err
	}
	if checkpoint.Run.ID == "" {
		return fmt.Errorf("checkpoint ID cannot be empty")
	}
	checkpoint.ID = checkpoint.Run.ID
	checkpoint.SavedAt = checkpoint.SavedAt.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkpointContextErr(ctx); err != nil {
		return err
	}
	cloned, err := cloneCheckpoint(checkpoint)
	if err != nil {
		return err
	}
	s.checkpoints[checkpoint.Run.ID] = cloned
	return nil
}

func (s *MemoryCheckpointStore) Load(ctx context.Context, runID string) (Checkpoint, error) {
	if err := checkpointContextErr(ctx); err != nil {
		return Checkpoint{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := checkpointContextErr(ctx); err != nil {
		return Checkpoint{}, err
	}
	checkpoint, ok := s.checkpoints[runID]
	if !ok {
		return Checkpoint{}, fmt.Errorf("%w: %s", ErrCheckpointNotFound, runID)
	}
	cloned, err := cloneCheckpoint(checkpoint)
	if err != nil {
		return Checkpoint{}, err
	}
	return cloned, nil
}

func checkpointContextErr(ctx context.Context) error {
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
