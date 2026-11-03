package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

type AgentService interface {
	CreateSession(context.Context) (Session, error)
	StartRun(context.Context, string, string) (Run, error)
	CancelRun(context.Context, string) error
	GetRun(context.Context, string) (Run, error)
}

type serviceRun struct {
	run    Run
	ctx    context.Context
	cancel context.CancelFunc
}

type agentService struct {
	agent     *Agent
	queue     RunQueue
	limiter   Limiter
	mu        sync.RWMutex
	sessions  map[string]bool
	runs      map[string]*serviceRun
	events    map[string]*eventBroker
	closed    bool
	closeDone chan struct{}
	wg        sync.WaitGroup
}

// AgentServiceOptions enables admission controls without changing the legacy
// NewAgentService constructor. A nil Queue or Limiter leaves that boundary
// disabled.
type AgentServiceOptions struct {
	Queue   RunQueue
	Limiter Limiter
}

var _ AgentService = (*agentService)(nil)

// NewAgentService exposes one Agent as a small, process-local service. The
// service deliberately keeps storage in memory; durable storage remains the
// responsibility of CheckpointStore and later tasks.
func NewAgentService(agent *Agent) *agentService {
	return NewAgentServiceWithOptions(agent, AgentServiceOptions{})
}

func NewAgentServiceWithOptions(agent *Agent, options AgentServiceOptions) *agentService {
	s := &agentService{
		agent:     agent,
		queue:     options.Queue,
		limiter:   options.Limiter,
		sessions:  make(map[string]bool),
		runs:      make(map[string]*serviceRun),
		events:    make(map[string]*eventBroker),
		closeDone: make(chan struct{}),
	}
	if s.queue != nil {
		s.wg.Add(1)
		go s.runQueueWorker()
	}
	return s
}

func (s *agentService) runQueueWorker() {
	defer s.wg.Done()
	for {
		run, err := s.queue.Dequeue(context.Background())
		if err != nil {
			if errors.Is(err, ErrRunQueueClosed) {
				return
			}
			continue
		}
		s.mu.RLock()
		record := s.runs[run.ID]
		s.mu.RUnlock()
		if record == nil {
			continue
		}
		if s.limiter != nil {
			if err := s.limiter.Allow(record.ctx, run.SessionID); err != nil {
				s.mu.Lock()
				record.run.Status = RunFailed
				record.run.Error = err.Error()
				s.mu.Unlock()
				continue
			}
		}
		s.executeRun(run.ID, record)
		if releaser, ok := s.limiter.(interface{ Release(string) }); ok {
			releaser.Release(run.SessionID)
		}
	}
}

func (s *agentService) executeRun(runID string, record *serviceRun) {
	_, _ = s.agent.RunWithEvents(contextFromServiceRun(record), record.run.Input, runID, func(event RunEvent) {
		_ = s.publish(runID, event)
	})
}

func contextFromServiceRun(record *serviceRun) context.Context {
	// serviceRun retains the cancellation function for the public CancelRun
	// API. The worker uses a context stored alongside it when configured.
	if record.ctx != nil {
		return record.ctx
	}
	return context.Background()
}

func (s *agentService) startRunDirect(run Run, record *serviceRun) {
	defer s.wg.Done()
	defer record.cancel()
	if s.limiter != nil {
		if err := s.limiter.Allow(record.ctx, run.SessionID); err != nil {
			s.mu.Lock()
			record.run.Status = RunFailed
			record.run.Error = err.Error()
			s.mu.Unlock()
			return
		}
		defer func() {
			if releaser, ok := s.limiter.(interface{ Release(string) }); ok {
				releaser.Release(run.SessionID)
			}
		}()
	}
	s.executeRun(run.ID, record)
}

func (s *agentService) CreateSession(ctx context.Context) (Session, error) {
	if err := serviceContextErr(ctx); err != nil {
		return Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Session{}, errors.New("agent service is closed")
	}
	if s.agent == nil {
		return Session{}, errors.New("agent is nil")
	}
	session := s.agent.Session()
	s.sessions[session.ID] = true
	return session, nil
}

func (s *agentService) StartRun(ctx context.Context, sessionID, input string) (Run, error) {
	if err := serviceContextErr(ctx); err != nil {
		return Run{}, err
	}
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(input) == "" {
		return Run{}, &AgentError{Kind: InvalidArgument, Message: "session ID and input are required"}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Run{}, errors.New("agent service is closed")
	}
	if !s.sessions[sessionID] {
		s.mu.Unlock()
		return Run{}, fmt.Errorf("session not found: %s", sessionID)
	}
	if s.agent == nil {
		s.mu.Unlock()
		return Run{}, errors.New("agent is nil")
	}
	run := NewRun(sessionID)
	run.Input = strings.TrimSpace(input)
	// StartRun is asynchronous: the request context only validates the
	// admission call and must not be tied to the run's lifetime.
	runCtx, cancel := context.WithCancel(context.Background())
	record := &serviceRun{run: run, ctx: runCtx, cancel: cancel}
	s.runs[run.ID] = record
	s.events[run.ID] = newEventBroker()
	s.mu.Unlock()

	if s.queue != nil {
		if err := s.queue.Enqueue(ctx, run); err != nil {
			cancel()
			s.mu.Lock()
			delete(s.runs, run.ID)
			delete(s.events, run.ID)
			s.mu.Unlock()
			return Run{}, err
		}
		return run, nil
	}

	s.wg.Add(1)
	go func() {
		s.startRunDirect(run, record)
	}()
	return run, nil
}

func (s *agentService) publish(runID string, event RunEvent) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("agent service is closed")
	}
	record := s.runs[runID]
	broker := s.events[runID]
	if record == nil || broker == nil {
		s.mu.Unlock()
		return eventRunNotFound(runID)
	}
	if event.Type == EventRunCompleted {
		record.run.Status = event.Status
		record.run.Step = event.Step
		record.run.Result = event.Result
		record.run.Error = event.Error
		record.run.UpdatedAt = event.At
	}
	s.mu.Unlock()
	broker.publish(event)
	return nil
}

func (s *agentService) CancelRun(ctx context.Context, runID string) error {
	if err := serviceContextErr(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("agent service is closed")
	}
	record, ok := s.runs[runID]
	if !ok {
		s.mu.Unlock()
		return eventRunNotFound(runID)
	}
	if record.run.Status == RunCompleted || record.run.Status == RunFailed || record.run.Status == RunInterrupted {
		s.mu.Unlock()
		return nil
	}
	record.cancel()
	s.mu.Unlock()
	return nil
}

func (s *agentService) GetRun(ctx context.Context, runID string) (Run, error) {
	if err := serviceContextErr(ctx); err != nil {
		return Run{}, err
	}
	s.mu.RLock()
	record, ok := s.runs[runID]
	if ok {
		run := record.run
		s.mu.RUnlock()
		return run, nil
	}
	s.mu.RUnlock()
	return Run{}, eventRunNotFound(runID)
}

func (s *agentService) ReadEvents(ctx context.Context, runID string, after uint64) ([]RunEvent, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	broker, ok := s.events[runID]
	s.mu.RUnlock()
	if !ok {
		return nil, eventRunNotFound(runID)
	}
	return broker.read(after), nil
}

func (s *agentService) Close() error {
	s.mu.Lock()
	if s.closed {
		closeDone := s.closeDone
		s.mu.Unlock()
		<-closeDone
		return nil
	}
	s.closed = true
	cancels := make([]context.CancelFunc, 0, len(s.runs))
	for _, record := range s.runs {
		cancels = append(cancels, record.cancel)
	}
	brokers := make([]*eventBroker, 0, len(s.events))
	for _, broker := range s.events {
		brokers = append(brokers, broker)
	}
	s.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	if s.queue != nil {
		_ = s.queue.Close()
	}
	for _, broker := range brokers {
		broker.close()
	}
	s.wg.Wait()
	close(s.closeDone)
	return nil
}

func (s *agentService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path)
	if len(parts) == 1 && parts[0] == "sessions" && r.Method == http.MethodPost {
		session, err := s.CreateSession(r.Context())
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, session)
		return
	}
	if len(parts) == 3 && parts[0] == "sessions" && parts[2] == "runs" && r.Method == http.MethodPost {
		var request struct {
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeServiceError(w, &AgentError{Kind: InvalidArgument, Message: "invalid JSON body", Err: err})
			return
		}
		run, err := s.StartRun(r.Context(), parts[1], request.Input)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, run)
		return
	}
	if len(parts) >= 2 && parts[0] == "runs" {
		runID := parts[1]
		if len(parts) == 2 && r.Method == http.MethodGet {
			run, err := s.GetRun(r.Context(), runID)
			if err != nil {
				writeServiceError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, run)
			return
		}
		if len(parts) == 2 && r.Method == http.MethodDelete {
			if err := s.CancelRun(r.Context(), runID); err != nil {
				writeServiceError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if len(parts) == 3 && parts[2] == "events" && r.Method == http.MethodGet {
			s.serveEvents(w, r, runID)
			return
		}
	}
	http.NotFound(w, r)
}

func (s *agentService) serveEvents(w http.ResponseWriter, r *http.Request, runID string) {
	after, err := parseAfter(r)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	s.mu.RLock()
	broker, ok := s.events[runID]
	s.mu.RUnlock()
	if !ok {
		writeServiceError(w, eventRunNotFound(runID))
		return
	}
	stream, unsubscribe, err := broker.subscribe(r.Context(), after)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer unsubscribe()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeServiceError(w, errors.New("streaming is not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	for event := range stream {
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Sequence, data)
		flusher.Flush()
	}
}

func serviceContextErr(ctx context.Context) error {
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

func splitPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

func parseAfter(r *http.Request) (uint64, error) {
	value, ok := "", false
	if values, exists := r.URL.Query()["after"]; exists {
		value, ok = values[0], true
	} else if header := r.Header.Get("Last-Event-ID"); header != "" {
		value, ok = header, true
	}
	if !ok {
		return 0, nil
	}
	after, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, &AgentError{Kind: InvalidArgument, Message: "invalid after sequence", Err: err}
	}
	return after, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeServiceError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if strings.Contains(err.Error(), "not found") {
		status = http.StatusNotFound
	}
	if errors.Is(err, context.Canceled) {
		status = http.StatusRequestTimeout
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
