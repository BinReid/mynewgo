package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOrderMigrationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE sites (id INTEGER PRIMARY KEY, name TEXT NOT NULL, url TEXT NOT NULL UNIQUE, interval_seconds INTEGER NOT NULL);
INSERT INTO sites VALUES (1, 'First', 'https://one.example/', 15), (2, 'Second', 'https://two.example/', 15), (3, 'Third', 'https://three.example/', 15);
CREATE TABLE site_order (site_id INTEGER PRIMARY KEY, position INTEGER NOT NULL);
INSERT INTO site_order VALUES (3, 1), (1, 2), (2, 3);`)
	legacy.Close()
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.db.Close() })
	assertSiteOrder(t, store, []int64{3, 1, 2})
	if err := store.move(context.Background(), 2, 3, false); err != nil {
		t.Fatal(err)
	}
	if err := store.move(context.Background(), 2, 999, false); err != errInvalidOrder {
		t.Fatalf("invalid target: %v", err)
	}
	if err := store.reorder(context.Background(), []int64{1, 1, 3}); err != errInvalidOrder {
		t.Fatalf("duplicate IDs: %v", err)
	}
	if err := store.reorder(context.Background(), []int64{3}); err != errInvalidOrder {
		t.Fatalf("partial order: %v", err)
	}
	store.db.Close()
	store, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.add(context.Background(), "Fourth", "https://four.example/", 15); err != nil {
		t.Fatal(err)
	}
	assertSiteOrder(t, store, []int64{2, 3, 1, 4})
}

func assertSiteOrder(t *testing.T, store *Store, expected []int64) {
	t.Helper()
	sites, err := store.sites(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, site := range sites {
		ids = append(ids, site.ID)
	}
	if !reflect.DeepEqual(ids, expected) {
		t.Fatalf("site order %v, expected %v", ids, expected)
	}
}

func TestDashboardSearchAndChart(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	ctx := context.Background()
	for _, site := range []struct{ name, url string }{{"Магазин", "https://shop.example/"}, {"Docs 100%", "https://docs.example/"}} {
		if _, err := store.add(ctx, site.name, site.url, 60); err != nil {
			t.Fatal(err)
		}
	}
	problem := "HTTP 503 · </script><script>alert(1)</script>"
	if err := store.record(ctx, 1, Check{At: time.Now().Unix(), Code: 503, Problem: problem}); err != nil {
		t.Fatal(err)
	}
	if err := store.record(ctx, 2, Check{At: time.Now().Unix(), OK: true, Code: 200, Latency: 42}); err != nil {
		t.Fatal(err)
	}
	handler := newWeb(store, 8080)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	page := request("GET", "/?q=%D0%9C%D0%90%D0%93%D0%90%D0%97%D0%98%D0%9D", "")
	if page.Code != 200 {
		t.Fatal(page.Body.String())
	}
	for _, text := range []string{`src="/static/app.js"`, `data-refresh`, `HTTP 503`, `data-id="1"`, `<strong>2</strong>`} {
		if !strings.Contains(page.Body.String(), text) {
			t.Fatalf("missing %q", text)
		}
	}
	if strings.Contains(page.Body.String(), `data-id="2"`) {
		t.Fatal("search did not filter the list")
	}
	for query, expected := range map[string]int{"МАГАЗИН": 1, "docs.example": 1, "%": 1, "_": 0} {
		sites, err := store.sites(ctx, query)
		if err != nil || len(sites) != expected {
			t.Fatalf("search %q: %v, %v", query, sites, err)
		}
	}
	detail := request("GET", "/sites/1", "")
	if detail.Code != 200 {
		t.Fatal(detail.Body.String())
	}
	markup := detail.Body.String()
	startMarker := `<script class="chart-data" type="application/json">`
	_, chart, found := strings.Cut(markup, startMarker)
	if !found {
		t.Fatal("chart data missing")
	}
	chart, _, _ = strings.Cut(chart, "</script>")
	var checks []Check
	if err := json.Unmarshal([]byte(chart), &checks); err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].Problem != problem {
		t.Fatalf("invalid chart data: %v", checks)
	}
	if strings.Contains(markup, "<script>alert(1)</script>") {
		t.Fatal("unescaped chart data")
	}
	if !strings.Contains(markup, `data-interval="60"`) {
		t.Fatal("chart ignores monitor interval")
	}
	invalid := request("POST", "/", "url=invalid&interval=30")
	if invalid.Code != 422 || !strings.Contains(invalid.Body.String(), `class="panel add-panel" open`) {
		t.Fatal("form validation error is hidden")
	}
}

func TestMoveHTTPValidation(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	for _, address := range []string{"https://one.example/", "https://two.example/", "https://three.example/"} {
		if _, err := store.add(context.Background(), address, address, 15); err != nil {
			t.Fatal(err)
		}
	}
	handler := newWeb(store, 8080)
	for _, test := range []struct {
		path, body string
		status     int
	}{
		{"/sites/order", `{"id":3,"target":1,"after":false}`, http.StatusNoContent},
		{"/sites/order", `{"id":3,"target":999}`, http.StatusConflict},
		{"/sites/order", `{"id":1,"target":2} {}`, http.StatusBadRequest},
		{"/reorder", `[1,1,3]`, http.StatusConflict},
		{"/reorder", `[1,2]`, http.StatusConflict},
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8080"+test.path, strings.NewReader(test.body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s %s: got %d", test.path, test.body, w.Code)
		}
	}
	assertSiteOrder(t, store, []int64{3, 1, 2})
}
