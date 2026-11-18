package main

import (
	"bytes"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Исправлено: web/*.html и web/static/* вместо web/.html
//go:embed web/*.html web/static/*
var assets embed.FS

var durationUnits = strings.NewReplacer("h", " ч ", "m", " мин ", "s", " с")

var pages = template.Must(template.New("pages").Funcs(template.FuncMap{
	"date": func(at int64) string { return time.Unix(at, 0).Local().Format("02.01.2006 15:04:05") },
	"clock": func(at int64) string { return time.Unix(at, 0).Local().Format("15:04:05") },
	"datetime": func(at int64) string { return time.Unix(at, 0).UTC().Format(time.RFC3339) },
	"duration": func(from, to int64) string {
		return strings.TrimSpace(durationUnits.Replace((time.Duration(to-from) * time.Second).String()))
	},
	"availability": availability,
	"bucketTitle": func(b Bucket) string {
		return fmt.Sprintf("%s · успешно: %d, сбои: %d", time.Unix(b.At, 0).Local().Format("02.01 15:04"), b.OK, b.Failed)
	},
	// Добавлена функция json для безопасной передачи данных в <script>
	"json": func(v any) (template.JS, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return template.JS(b), nil
	},
}).ParseFS(assets, "web/*.html"))

type Web struct{ store *Store }

type Form struct {
	Name, URL, Error string
	Interval         int
}

type Dashboard struct {
	Sites    []Site
	Up, Down int
	Form     Form
	Query    string // Добавлено для поиска
}

type SitePage struct {
	Site      Site
	Checks    []Check
	Incidents []Incident
	Now       CheckWindow
}

type CheckWindow struct {
	Start, End int64
}

func newWeb(store *Store, port int) http.Handler {
	web := &Web{store}
	static, err := fs.Sub(assets, "web/static")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", web.dashboard)
	mux.HandleFunc("POST /{$}", web.add)
	mux.HandleFunc("GET /sites/{id}", web.detail)
	mux.HandleFunc("POST /reorder", web.reorder) // Добавлен обработчик перетаскивания

	localIP := fmt.Sprintf("127.0.0.1:%d", port)
	localName := fmt.Sprintf("localhost:%d", port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != localIP && r.Host != localName {
			http.Error(w, "Invalid host", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPost {
			origin := r.Header.Get("Origin")
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" || (origin != "" && origin != "http://"+r.Host) {
				http.Error(w, "Invalid origin", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

func (web *Web) dashboard(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	web.renderDashboard(w, r, Form{Interval: 30}, query, http.StatusOK)
}

func (web *Web) renderDashboard(w http.ResponseWriter, r *http.Request, form Form, query string, status int) {
	// Передаем query в store.sites
	sites, err := web.store.sites(r.Context(), query)
	if err != nil {
		serverError(w, err)
		return
	}
	if err := web.store.buckets(r.Context(), sites, time.Now()); err != nil {
		serverError(w, err)
		return
	}
	view := Dashboard{Sites: sites, Form: form, Query: query}
	for _, site := range sites {
		if site.Last == nil {
			continue
		}
		if site.Last.OK {
			view.Up++
		} else {
			view.Down++
		}
	}
	render(w, "dashboard", view, status)
}

func (web *Web) add(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Не удалось прочитать форму", http.StatusBadRequest)
		return
	}
	interval, _ := strconv.Atoi(r.PostForm.Get("interval"))
	form := Form{URL: strings.TrimSpace(r.PostForm.Get("url")), Name: strings.TrimSpace(r.PostForm.Get("name")), Interval: interval}
	address, err := parseAddress(form.URL)
	switch {
	case err != nil:
		form.Error = "Введите корректный адрес сайта, например https://example.com."
	case utf8.RuneCountInString(form.Name) > 80:
		form.Error = "Название должно содержать до 80 символов."
	case interval != 15 && interval != 30 && interval != 60 && interval != 300:
		form.Error = "Выберите интервал проверки из списка."
	default:
		name := form.Name
		if name == "" {
			name = address.Host
		}
		created, err := web.store.add(r.Context(), name, address.String(), interval)
		if err != nil {
			serverError(w, err)
			return
		}
		if created {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		form.Error = "Этот адрес уже отслеживается."
	}
	// При ошибке сбрасываем query
	web.renderDashboard(w, r, form, "", http.StatusUnprocessableEntity)
}

func (web *Web) reorder(w http.ResponseWriter, r *http.Request) {
	var ids []int64
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if err := web.store.reorder(r.Context(), ids); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseAddress(input string) (*url.URL, error) {
	if len(input) > 2048 {
		return nil, errors.New("URL is too long")
	}
	address, err := url.Parse(input)
	if err != nil || address.Hostname() == "" || address.User != nil || (address.Scheme != "http" && address.Scheme != "https") {
		return nil, errors.New("invalid HTTP URL")
	}
	if port := address.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, errors.New("invalid port")
		}
	}
	address.Host = strings.ToLower(address.Host)
	address.Fragment = ""
	address.RawFragment = ""
	if address.Path == "" {
		address.Path = "/"
	}
	return address, nil
}

func (web *Web) detail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	site, err := web.store.site(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	now := time.Now()
	checks, err := web.store.checks(r.Context(), id, now)
	if err != nil {
		serverError(w, err)
		return
	}
	incidents, err := web.store.incidents(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	start, end := dayWindow(now)
	render(w, "detail", SitePage{site, checks, incidents, CheckWindow{start, end}}, http.StatusOK)
}

func availability(buckets []Bucket) string {
	var ok, total int
	for _, bucket := range buckets {
		ok += bucket.OK
		total += bucket.OK + bucket.Failed
	}
	if total == 0 {
		return "—"
	}
	return strings.Replace(fmt.Sprintf("%.1f%%", float64(ok)*100/float64(total)), ".", ",", 1)
}

func render(w http.ResponseWriter, name string, data any, status int) {
	var buffer bytes.Buffer
	if err := pages.ExecuteTemplate(&buffer, name, data); err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buffer.Bytes())
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("dashboard: %v", err)
	http.Error(w, "Не удалось загрузить данные монитора", http.StatusInternalServerError)
}