package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
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
			if err := m.store.record(ctx, site.ID, check); err != nil && ctx.Err() == nil {
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
		check.Problem = "Ошибка соединения"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			check.Problem = "Истёк тайм-аут"
		}
		return check
	}
	defer response.Body.Close()
	check.Code = response.StatusCode
	check.OK = check.Code >= 200 && check.Code < 300
	if !check.OK {
		check.Problem = fmt.Sprintf("HTTP %d", check.Code)
	}
	return check
}
