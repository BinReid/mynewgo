package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEditPreservesHistoryAndIgnoresOldCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.db.Close() })
	ctx := context.Background()
	if _, err := store.add(ctx, "First", "https://first.example/", 300); err != nil {
		t.Fatal(err)
	}
	old, err := store.site(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.recordSite(ctx, old, Check{At: now.Unix(), Problem: "HTTP 503"}); err != nil {
		t.Fatal(err)
	}
	if err := store.update(ctx, old.ID, "Renamed", "https://new.example/", 60); err != nil {
		t.Fatal(err)
	}
	updated, err := store.site(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Renamed" || updated.URL != "https://new.example/" || updated.Interval != 60 || updated.Last != nil {
		t.Fatalf("unexpected edited site: %+v", updated)
	}
	due, err := store.due(ctx, now)
	if err != nil || len(due) != 1 {
		t.Fatalf("new address must be checked immediately: %v, %v", due, err)
	}
	if err := store.recordSite(ctx, old, Check{At: now.Unix(), OK: true, Code: 200}); err != nil {
		t.Fatal(err)
	}
	checks, err := store.checks(ctx, 1, now)
	if err != nil || len(checks) != 1 || checks[0].OK {
		t.Fatalf("stale check was recorded: %+v, %v", checks, err)
	}
	incidents, err := store.incidents(ctx, 1)
	if err != nil || len(incidents) != 1 || incidents[0].Resolved.Valid {
		t.Fatalf("edit falsely resolved incident: %+v, %v", incidents, err)
	}
	if err := store.recordSite(ctx, updated, Check{At: now.Unix(), OK: true, Code: 200}); err != nil {
		t.Fatal(err)
	}
	checks, err = store.checks(ctx, 1, now)
	if err != nil || len(checks) != 2 {
		t.Fatalf("history lost: %+v, %v", checks, err)
	}
	updated, err = store.site(ctx, 1)
	if err != nil || updated.Last == nil || !updated.Last.OK {
		t.Fatalf("new status not saved: %+v, %v", updated, err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := store.site(ctx, 1)
	if err != nil || persisted.Name != "Renamed" || persisted.URL != "https://new.example/" || persisted.Interval != 60 || persisted.Last == nil || !persisted.Last.OK {
		t.Fatalf("edited settings lost after restart: %+v, %v", persisted, err)
	}
}

func TestDeleteWithInFlightCheckAndReusedID(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	ctx := context.Background()
	if _, err := store.add(ctx, "First", "https://first.example/", 15); err != nil {
		t.Fatal(err)
	}
	old, err := store.site(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TABLE site_order (site_id INTEGER REFERENCES sites(id), position INTEGER); INSERT INTO site_order VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	if err := store.recordSite(ctx, old, Check{At: time.Now().Unix(), Problem: "HTTP 503"}); err != nil {
		t.Fatal(err)
	}
	if err := store.delete(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.site(ctx, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("site still exists: %v", err)
	}
	if err := store.recordSite(ctx, old, Check{At: time.Now().Unix(), OK: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.add(ctx, "Replacement", "https://replacement.example/", 15); err != nil {
		t.Fatal(err)
	}
	if err := store.recordSite(ctx, old, Check{At: time.Now().Unix(), OK: true}); err != nil {
		t.Fatal(err)
	}
	replacement, err := store.site(ctx, 1)
	if err != nil || replacement.Last != nil || replacement.Generation == old.Generation {
		t.Fatalf("stale result reached replacement site: %+v, %v", replacement, err)
	}
	for _, table := range []string{"checks", "incidents", "site_order"} {
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("orphan rows in %s: %d, %v", table, count, err)
		}
	}
}

func TestEditAndDeleteThroughHTTP(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	for _, address := range []string{"https://first.example/", "https://second.example/"} {
		if _, err := store.add(context.Background(), address, address, 15); err != nil {
			t.Fatal(err)
		}
	}
	handler := newWeb(store, 8080)
	request := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	page := request("GET", "/sites/1/edit", nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Подтверждаю удаление") {
		t.Fatal(page.Body.String())
	}
	form := url.Values{"name": {"Renamed"}, "url": {"https://second.example/"}, "interval": {"60"}}
	duplicate := request("POST", "/sites/1/edit", form)
	if duplicate.Code != 422 || !strings.Contains(duplicate.Body.String(), "другим сайтом") {
		t.Fatal("duplicate URL was accepted")
	}
	form.Set("url", "https://new.example/")
	saved := request("POST", "/sites/1/edit", form)
	if saved.Code != 303 || saved.Header().Get("Location") != "/sites/1" {
		t.Fatalf("save failed: %d %s", saved.Code, saved.Body.String())
	}
	if response := request("POST", "/sites/1/delete", nil); response.Code != 422 {
		t.Fatal("deletion did not require confirmation")
	}
	if response := request("POST", "/sites/1/delete", url.Values{"confirm": {"yes"}}); response.Code != 303 {
		t.Fatalf("delete failed: %d %s", response.Code, response.Body.String())
	}
	if response := request("GET", "/sites/1", nil); response.Code != http.StatusNotFound {
		t.Fatal("deleted site is still available")
	}
	assertSiteOrder(t, store, []int64{2})
}
