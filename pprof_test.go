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
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("read pprof response: %v", err)
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
