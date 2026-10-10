package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// PprofServer exposes standard-library pprof endpoints on a separate,
// explicitly configured HTTP listener. Keep this listener local and private.
type PprofServer struct {
	Server   *http.Server
	Listener net.Listener
}

func (s *PprofServer) Addr() string {
	if s == nil || s.Listener == nil {
		return ""
	}
	return s.Listener.Addr().String()
}

// startPprofServer starts pprof only when addr is non-empty. The server is
// shut down automatically when ctx is canceled.
func startPprofServer(ctx context.Context, addr string) (*PprofServer, error) {
	if addr == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen pprof address %q: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	for _, name := range []string{"allocs", "block", "goroutine", "heap", "mutex", "threadcreate"} {
		mux.Handle("/debug/pprof/"+name, pprof.Handler(name))
	}

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	result := &PprofServer{Server: server, Listener: listener}

	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Printf("pprof server stopped: %v", serveErr)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
			log.Printf("pprof server shutdown: %v", shutdownErr)
		}
	}()

	return result, nil
}
