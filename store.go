package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Site struct {
	ID        int64
	Name, URL string
	Interval  int
	Last      *Check
	Samples   []Bucket
}

type Check struct {
	At      int64  `json:"at"`
	Latency int64  `json:"latency"`
	OK      bool   `json:"ok"`
	Code    int    `json:"code"`
	Problem string `json:"problem"`
}

type Incident struct {
	Started  int64
	Resolved sql.NullInt64
	Problem  string
}

type Bucket struct {
	At         int64
	OK, Failed int
}

type Store struct{ db *sql.DB }

const schema = `
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS sites (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  url TEXT NOT NULL UNIQUE,
  interval_seconds INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS checks (
  id INTEGER PRIMARY KEY,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  checked_at INTEGER NOT NULL,
  latency_ms INTEGER NOT NULL,
  ok INTEGER NOT NULL,
  status_code INTEGER NOT NULL,
  problem TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS checks_site_time ON checks(site_id, checked_at, id);
CREATE INDEX IF NOT EXISTS checks_time ON checks(checked_at);
CREATE TABLE IF NOT EXISTS incidents (
  id INTEGER PRIMARY KEY,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  started_at INTEGER NOT NULL,
  resolved_at INTEGER,
  problem TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS incidents_one_active ON incidents(site_id) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS incidents_site_time ON incidents(site_id, started_at DESC, id DESC);
`

func openStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

func (s *Store) add(ctx context.Context, name, url string, interval int) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO sites(name, url, interval_seconds) VALUES (?, ?, ?) ON CONFLICT(url) DO NOTHING`, name, url, interval)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

const siteQuery = `SELECT s.id, s.name, s.url, s.interval_seconds,
  c.checked_at, c.latency_ms, c.ok, c.status_code, c.problem
FROM sites s LEFT JOIN checks c ON c.id = (
  SELECT id FROM checks WHERE site_id = s.id ORDER BY checked_at DESC, id DESC LIMIT 1
)`

func scanSite(row interface{ Scan(...any) error }) (Site, error) {
	var site Site
	var at, latency, code sql.NullInt64
	var ok sql.NullBool
	var problem sql.NullString
	err := row.Scan(&site.ID, &site.Name, &site.URL, &site.Interval, &at, &latency, &ok, &code, &problem)
	if at.Valid {
		site.Last = &Check{at.Int64, latency.Int64, ok.Bool, int(code.Int64), problem.String}
	}
	return site, err
}

func (s *Store) sites(ctx context.Context) ([]Site, error) {
	rows, err := s.db.QueryContext(ctx, siteQuery+` ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sites []Site
	for rows.Next() {
		site, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}

func (s *Store) site(ctx context.Context, id int64) (Site, error) {
	return scanSite(s.db.QueryRowContext(ctx, siteQuery+` WHERE s.id = ?`, id))
}

func (s *Store) due(ctx context.Context, now time.Time) ([]Site, error) {
	rows, err := s.db.QueryContext(ctx, siteQuery+`
WHERE c.id IS NULL OR c.checked_at + s.interval_seconds <= ?
ORDER BY COALESCE(c.checked_at + s.interval_seconds, 0), s.id`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sites []Site
	for rows.Next() {
		site, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}

const bucketSeconds = 30 * 60
const bucketCount = 48

func dayWindow(now time.Time) (int64, int64) {
	end := now.Unix()
	return end - 24*60*60 + 1, end
}

func (s *Store) buckets(ctx context.Context, sites []Site, now time.Time) error {
	start, end := dayWindow(now)
	byID := make(map[int64][]Bucket, len(sites))
	for i := range sites {
		sites[i].Samples = make([]Bucket, bucketCount)
		for j := range sites[i].Samples {
			sites[i].Samples[j].At = start + int64(j)*bucketSeconds
		}
		byID[sites[i].ID] = sites[i].Samples
	}
	rows, err := s.db.QueryContext(ctx, `SELECT site_id, (checked_at - ?) / ?, SUM(ok), COUNT(*) - SUM(ok)
FROM checks WHERE checked_at BETWEEN ? AND ? GROUP BY 1, 2`, start, bucketSeconds, start, end)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var index, ok, failed int
		if err := rows.Scan(&id, &index, &ok, &failed); err != nil {
			return err
		}
		if buckets := byID[id]; buckets != nil {
			buckets[index].OK = ok
			buckets[index].Failed = failed
		}
	}
	return rows.Err()
}

func (s *Store) checks(ctx context.Context, id int64, now time.Time) ([]Check, error) {
	start, end := dayWindow(now)
	rows, err := s.db.QueryContext(ctx, `SELECT checked_at, latency_ms, ok, status_code, problem FROM checks
WHERE site_id = ? AND checked_at BETWEEN ? AND ? ORDER BY checked_at, id`, id, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	checks := make([]Check, 0)
	for rows.Next() {
		var check Check
		if err := rows.Scan(&check.At, &check.Latency, &check.OK, &check.Code, &check.Problem); err != nil {
			return nil, err
		}
		checks = append(checks, check)
	}
	return checks, rows.Err()
}

func (s *Store) incidents(ctx context.Context, id int64) ([]Incident, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT started_at, resolved_at, problem FROM incidents
WHERE site_id = ? ORDER BY started_at DESC, id DESC LIMIT 30`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var incidents []Incident
	for rows.Next() {
		var incident Incident
		if err := rows.Scan(&incident.Started, &incident.Resolved, &incident.Problem); err != nil {
			return nil, err
		}
		incidents = append(incidents, incident)
	}
	return incidents, rows.Err()
}

func (s *Store) record(ctx context.Context, id int64, check Check) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO checks(site_id, checked_at, latency_ms, ok, status_code, problem)
VALUES (?, ?, ?, ?, ?, ?)`, id, check.At, check.Latency, check.OK, check.Code, check.Problem); err != nil {
		return err
	}
	if check.OK {
		_, err = tx.ExecContext(ctx, `UPDATE incidents SET resolved_at = ? WHERE site_id = ? AND resolved_at IS NULL`, check.At, id)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO incidents(site_id, started_at, problem) VALUES (?, ?, ?)
ON CONFLICT(site_id) WHERE resolved_at IS NULL DO NOTHING`, id, check.At, check.Problem)
	}
	if err != nil {
		return fmt.Errorf("update incident: %w", err)
	}
	return tx.Commit()
}
