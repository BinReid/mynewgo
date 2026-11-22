package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"
)

type Monitor struct {
	store    *Store
	client   *http.Client
	timeout  time.Duration
	parallel int
}

func (m *Monitor) run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	active := make(map[int64]bool)
	finished := make(chan int64, m.parallel)
	m.dispatch(ctx, time.Now(), active, finished, &workers)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if len(active) < m.parallel {
				m.dispatch(ctx, now, active, finished, &workers)
			}
		case id := <-finished:
			delete(active, id)
		}
	}
}

func (m *Monitor) dispatch(ctx context.Context, now time.Time, active map[int64]bool, finished chan<- int64, workers *sync.WaitGroup) {
	sites, err := m.store.due(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("load monitors: %v", err)
		}
		return
	}
	for _, site := range sites {
		if active[site.ID] {
			continue
		}
		active[site.ID] = true
		workers.Add(1)
		go func(site Site) {
			defer workers.Done()
			defer func() { finished <- site.ID }()
			check := m.check(ctx, site.URL)
			if ctx.Err() != nil {
				return
			}
			if err := m.store.recordSite(ctx, site, check); err != nil && ctx.Err() == nil {
				log.Printf("save check for site %d: %v", site.ID, err)
			}
		}(site)
		if len(active) == m.parallel {
			break
		}
	}
}

func (m *Monitor) check(ctx context.Context, address string) Check {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return Check{At: time.Now().Unix(), Problem: "Некорректный адрес"}
	}
	request.Header.Set("User-Agent", "UptimeMonitor/1.0")
	start := time.Now()
	response, err := m.client.Do(request)
	check := Check{At: time.Now().Unix(), Latency: time.Since(start).Milliseconds()}
	if err != nil {
		check.Problem = connectionProblem(ctx, err)
		return check
	}
	defer response.Body.Close()
	check.Code = response.StatusCode
	check.OK = check.Code >= 200 && check.Code < 300
	if !check.OK {
		check.Problem = fmt.Sprintf("HTTP %d · %s", check.Code, http.StatusText(check.Code))
	}
	return check
}

func connectionProblem(ctx context.Context, err error) string {
	var dns *net.DNSError
	var certificate *tls.CertificateVerificationError
	var network net.Error
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.As(err, &network) && network.Timeout():
		return "Истёк тайм-аут"
	case errors.As(err, &dns):
		return "Ошибка DNS — не удалось определить IP-адрес сайта"
	case errors.As(err, &certificate):
		return "Ошибка TLS — не удалось проверить сертификат сайта"
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.Errno(10061)):
		return "Соединение отклонено сервером"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.Errno(10054)):
		return "Сервер разорвал соединение"
	default:
		return "Ошибка соединения — " + err.Error()
	}
}
