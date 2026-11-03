package main

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestStartPprofServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server, err := startPprofServer(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startPprofServer() error = %v", err)
	}
	if server == nil || server.Addr() == "" {
		t.Fatal("startPprofServer() returned an invalid server")
	}

	var response *http.Response
	for attempt := 0; attempt < 20; attempt++ {
		response, err = http.Get("http://" + server.Addr() + "/debug/pprof/")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET pprof index error = %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("read pprof response: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close pprof response: %v", err)
	}
	for _, profile := range []string{"heap", "goroutine", "allocs"} {
		response, err := http.Get("http://" + server.Addr() + "/debug/pprof/" + profile)
		if err != nil {
			t.Fatalf("GET pprof %s error = %v", profile, err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatalf("close pprof %s response: %v", profile, err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("pprof %s status = %d", profile, response.StatusCode)
		}
	}
}

func TestStartPprofServerDisabled(t *testing.T) {
	server, err := startPprofServer(context.Background(), "")
	if err != nil {
		t.Fatalf("startPprofServer() error = %v", err)
	}
	if server != nil {
		t.Fatal("disabled pprof should not create a server")
	}
}

func TestStartPprofServerStopsAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := startPprofServer(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startPprofServer() error = %v", err)
	}

	response, err := http.Get("http://" + server.Addr() + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET pprof index error = %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close pprof response: %v", err)
	}

	cancel()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	for attempt := 0; attempt < 20; attempt++ {
		response, err = client.Get("http://" + server.Addr() + "/debug/pprof/")
		if response != nil {
			_ = response.Body.Close()
		}
		if err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pprof server still accepted requests after context cancellation")
}
