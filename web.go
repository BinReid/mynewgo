package main

import (
	"bytes"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

//go:embed web/*.html web/static/*
var assets embed.FS

var durationUnits = strings.NewReplacer("h", " ч ", "m", " мин ", "s", " с")

var pages = template.Must(template.New("pages").Funcs(template.FuncMap{
	"date":     func(at int64) string { return time.Unix(at, 0).Local().Format("02.01.2006 15:04:05") },
	"clock":    func(at int64) string { return time.Unix(at, 0).Local().Format("15:04:05") },
	"datetime": func(at int64) string { return time.Unix(at, 0).UTC().Format(time.RFC3339) },
	"duration": func(from, to int64) string {
		return strings.TrimSpace(durationUnits.Replace((time.Duration(to-from) * time.Second).String()))
	},
	"availability": availability,
	"bucketTitle": func(b Bucket) string {
		return fmt.Sprintf("%s · успешно: %d, сбои: %d", time.Unix(b.At, 0).Local().Format("02.01 15:04"), b.OK, b.Failed)
	},
	"json": func(v any) (template.JS, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return template.JS(b), nil
	},
}).ParseFS(assets, "web/*.html"))

type Web struct {
	store   *Store
	baseURL string
}

type Form struct {
	Name, URL, Error string
	Interval         int
}

type Dashboard struct {
	Sites       []Site
	Up, Down    int
	Form        Form
	Query       string
	Total       int
	Canonical   string
	Description string
}

type SitePage struct {
	Site        Site
	Checks      []Check
	Incidents   []Incident
	Now         CheckWindow
	Canonical   string
	Description string
}

type CheckWindow struct {
	Start, End int64
}

type EditPage struct {
	Site        Site
	Form        Form
	Canonical   string
	Description string
	Robots      string
}

func newWeb(store *Store, port int) http.Handler {
	web := &Web{store: store}
	static, err := fs.Sub(assets, "web/static")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", web.dashboard)
	mux.HandleFunc("POST /{$}", web.add)
	mux.HandleFunc("GET /sites/{id}", web.detail)
	mux.HandleFunc("GET /sites/{id}/edit", web.edit)
	mux.HandleFunc("POST /sites/{id}/edit", web.save)
	mux.HandleFunc("POST /sites/{id}/delete", web.delete)
	mux.HandleFunc("POST /reorder", web.reorder)
	mux.HandleFunc("POST /sites/order", web.move)
	mux.HandleFunc("POST /probe", web.probe)

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
		web.baseURL = "http://" + r.Host
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		mux.ServeHTTP(w, r)
	})
}

func (web *Web) canonical(path string) string {
	if web.baseURL == "" {
		return path
	}
	return web.baseURL + path
}

func (web *Web) dashboard(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	web.renderDashboard(w, r, Form{Interval: 30}, query, http.StatusOK)
}

func (web *Web) renderDashboard(w http.ResponseWriter, r *http.Request, form Form, query string, status int) {
	sites, err := web.store.sites(r.Context(), "")
	if err != nil {
		serverError(w, err)
		return
	}
	if err := web.store.buckets(r.Context(), sites, time.Now()); err != nil {
		serverError(w, err)
		return
	}
	view := Dashboard{
		Form:        form,
		Query:       query,
		Total:       len(sites),
		Canonical:   web.canonical("/"),
		Description: "Uptime Monitor — локальный монитор доступности HTTP и HTTPS. Статусы сайтов, задержки и история проверок за 24 часа.",
	}
	if query != "" {
		view.Canonical = web.canonical("/?q=" + url.QueryEscape(query))
		view.Description = fmt.Sprintf("Результаты поиска «%s» в Uptime Monitor: статусы, задержки и доступность сайтов за 24 часа.", query)
	}
	for _, site := range sites {
		if strings.Contains(strings.ToLower(site.Name+" "+site.URL), strings.ToLower(query)) {
			view.Sites = append(view.Sites, site)
		}
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
	form, ok := readSiteForm(w, r)
	if !ok {
		return
	}
	if form.Error == "" {
		created, err := web.store.add(r.Context(), form.Name, form.URL, form.Interval)
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
	web.renderDashboard(w, r, form, strings.TrimSpace(r.URL.Query().Get("q")), http.StatusUnprocessableEntity)
}

func readSiteForm(w http.ResponseWriter, r *http.Request) (Form, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Не удалось прочитать форму", http.StatusBadRequest)
		return Form{}, false
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
		if form.Name == "" {
			form.Name = address.Host
		}
		form.URL = address.String()
	}
	return form, true
}

func (web *Web) edit(w http.ResponseWriter, r *http.Request) {
	site, ok := web.findSite(w, r)
	if !ok {
		return
	}
	render(w, "edit", EditPage{
		Site:        site,
		Form:        Form{Name: site.Name, URL: site.URL, Interval: site.Interval},
		Canonical:   web.canonical(fmt.Sprintf("/sites/%d/edit", site.ID)),
		Description: fmt.Sprintf("Настройки монитора для %s (%s).", site.Name, site.URL),
	}, http.StatusOK)
}

func (web *Web) save(w http.ResponseWriter, r *http.Request) {
	site, ok := web.findSite(w, r)
	if !ok {
		return
	}
	form, ok := readSiteForm(w, r)
	if !ok {
		return
	}
	if form.Error == "" {
		err := web.store.update(r.Context(), site.ID, form.Name, form.URL, form.Interval)
		switch {
		case errors.Is(err, errDuplicateSite):
			form.Error = "Этот адрес уже отслеживается другим сайтом."
		case errors.Is(err, sql.ErrNoRows):
			http.NotFound(w, r)
			return
		case err != nil:
			serverError(w, err)
			return
		default:
			http.Redirect(w, r, fmt.Sprintf("/sites/%d", site.ID), http.StatusSeeOther)
			return
		}
	}
	render(w, "edit", EditPage{
		Site:        site,
		Form:        form,
		Canonical:   web.canonical(fmt.Sprintf("/sites/%d/edit", site.ID)),
		Description: fmt.Sprintf("Настройки монитора для %s (%s).", site.Name, site.URL),
	}, http.StatusUnprocessableEntity)
}

func (web *Web) delete(w http.ResponseWriter, r *http.Request) {
	site, ok := web.findSite(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Не удалось прочитать форму", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("confirm") != "yes" {
		form := Form{Name: site.Name, URL: site.URL, Interval: site.Interval, Error: "Для удаления подтвердите удаление сайта и его истории."}
		render(w, "edit", EditPage{
			Site:        site,
			Form:        form,
			Canonical:   web.canonical(fmt.Sprintf("/sites/%d/edit", site.ID)),
			Description: fmt.Sprintf("Настройки монитора для %s (%s).", site.Name, site.URL),
		}, http.StatusUnprocessableEntity)
		return
	}
	if err := web.store.delete(r.Context(), site.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		serverError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (web *Web) reorder(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	var ids []int64
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&ids); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if err := web.store.reorder(r.Context(), ids); err != nil {
		if errors.Is(err, errInvalidOrder) {
			http.Error(w, "Список сайтов изменился. Обновите страницу.", http.StatusConflict)
			return
		}
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (web *Web) move(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var move struct {
		ID     int64 `json:"id"`
		Target int64 `json:"target"`
		After  bool  `json:"after"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&move); err != nil || move.ID < 1 || move.Target < 1 {
		http.Error(w, "Некорректный порядок сайтов", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if err := web.store.move(r.Context(), move.ID, move.Target, move.After); err != nil {
		if errors.Is(err, errInvalidOrder) {
			http.Error(w, "Список сайтов изменился. Обновите страницу.", http.StatusConflict)
			return
		}
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (web *Web) probe(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		SiteID    int64 `json:"siteId"`
		Reachable bool  `json:"reachable"`
		Latency   int64 `json:"latencyMs"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.SiteID < 1 || body.Latency < 0 || body.Latency > 600000 {
		http.Error(w, "Invalid probe", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	check := Check{At: time.Now().Unix(), Latency: body.Latency, OK: body.Reachable}
	if !body.Reachable {
		check.Problem = "Нет соединения с этого устройства"
	}
	if err := web.store.recordClient(r.Context(), body.SiteID, check); err != nil {
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

func (web *Web) findSite(w http.ResponseWriter, r *http.Request) (Site, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return Site{}, false
	}
	site, err := web.store.site(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return Site{}, false
	}
	if err != nil {
		serverError(w, err)
		return Site{}, false
	}
	return site, true
}

func (web *Web) detail(w http.ResponseWriter, r *http.Request) {
	site, ok := web.findSite(w, r)
	if !ok {
		return
	}
	id := site.ID
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
	description := fmt.Sprintf("Мониторинг %s (%s): текущий статус, задержка, история проверок и инцидентов за 24 часа.", site.Name, site.URL)
	if site.Last != nil && !site.Last.OK {
		description = fmt.Sprintf("%s (%s) — сбой: %s. История проверок и инцидентов за 24 часа.", site.Name, site.URL, site.Last.Problem)
	}
	render(w, "detail", SitePage{
		Site:        site,
		Checks:      checks,
		Incidents:   incidents,
		Now:         CheckWindow{start, end},
		Canonical:   web.canonical(fmt.Sprintf("/sites/%d", site.ID)),
		Description: description,
	}, http.StatusOK)
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