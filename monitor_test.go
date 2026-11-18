package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestIncidentSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "monitor.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.db.Close() })
	if _, err := store.add(ctx, "Test", "https://example.com/", 15); err != nil {
		t.Fatal(err)
	}
	if err := store.record(ctx, 1, Check{At: 10, Problem: "HTTP 503"}); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []Check{{At: 25, Problem: "HTTP 503"}, {At: 40, OK: true, Code: 200}} {
		if err := store.record(ctx, 1, check); err != nil {
			t.Fatal(err)
		}
	}
	incidents, err := store.incidents(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 || incidents[0].Started != 10 || !incidents[0].Resolved.Valid || incidents[0].Resolved.Int64 != 40 {
		t.Fatalf("expected one incident from 10 to 40, got %+v", incidents)
	}
}

func TestMonitorLimitsConcurrentChecks(t *testing.T) {
	var requests atomic.Int32
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	}))
	defer fixture.Close()
	store, err := openStore(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	for _, suffix := range []string{"/first", "/second", "/third"} {
		if _, err := store.add(context.Background(), suffix, fixture.URL+suffix, 15); err != nil {
			t.Fatal(err)
		}
	}
	monitor := Monitor{store: store, client: fixture.Client(), timeout: 6 * time.Second, parallel: 2}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		monitor.run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("did not start two checks")
		}
	}
	select {
	case <-started:
		t.Fatal("started a third check with both slots occupied")
	case <-time.After(1100 * time.Millisecond):
	}
	close(release)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("third check did not start after a slot became free")
	}
	if count := requests.Load(); count != 3 {
		t.Fatalf("expected three HTTP requests, got %d", count)
	}
}

func TestCheckTimeout(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer fixture.Close()
	monitor := Monitor{client: fixture.Client(), timeout: 60 * time.Millisecond}
	check := monitor.check(context.Background(), fixture.URL)
	if check.OK || check.Problem != "Истёк тайм-аут" {
		t.Fatalf("expected a timed out check, got %+v", check)
	}
}
