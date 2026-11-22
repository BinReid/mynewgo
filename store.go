package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Site struct {
	ID         int64
	Name, URL  string
	Interval   int
	Generation string
	Last       *Check
	LastClient *Check
	Samples    []Bucket
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
    interval_seconds INTEGER NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 0,
    generation TEXT NOT NULL DEFAULT '',
    check_requested INTEGER NOT NULL DEFAULT 0
); 
CREATE TABLE IF NOT EXISTS checks (
  id INTEGER PRIMARY KEY,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  checked_at INTEGER NOT NULL,
  latency_ms INTEGER NOT NULL,
  ok INTEGER NOT NULL,
  status_code INTEGER NOT NULL,
  problem TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT 'server'
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
	if err := migrateOrder(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateSource(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

func migrateOrder(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`PRAGMA table_info(sites)`)
	if err != nil {
		return err
	}
	hasOrder := false
	hasGeneration, hasRequested := false, false
	for rows.Next() {
		var index, required, primary int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&index, &name, &kind, &required, &defaultValue, &primary); err != nil {
			rows.Close()
			return err
		}
		hasOrder = hasOrder || name == "sort_order"
		hasGeneration = hasGeneration || name == "generation"
		hasRequested = hasRequested || name == "check_requested"
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !hasOrder {
		if _, err := tx.Exec(`ALTER TABLE sites ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE sites SET sort_order = id`); err != nil {
			return err
		}
		var legacyOrder int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'site_order'`).Scan(&legacyOrder); err != nil {
			return err
		}
		if legacyOrder > 0 {
			if _, err := tx.Exec(`UPDATE sites SET sort_order = COALESCE((SELECT position FROM site_order WHERE site_id = sites.id), id)`); err != nil {
				return err
			}
		}
	}
	if !hasGeneration {
		if _, err := tx.Exec(`ALTER TABLE sites ADD COLUMN generation TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasRequested {
		if _, err := tx.Exec(`ALTER TABLE sites ADD COLUMN check_requested INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE sites SET generation = lower(hex(randomblob(16))) WHERE generation = ''`); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateSource(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`PRAGMA table_info(checks)`)
	if err != nil {
		return err
	}
	hasSource := false
	for rows.Next() {
		var index, required, primary int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&index, &name, &kind, &required, &defaultValue, &primary); err != nil {
			rows.Close()
			return err
		}
		hasSource = hasSource || name == "source"
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !hasSource {
		if _, err := tx.Exec(`ALTER TABLE checks ADD COLUMN source TEXT NOT NULL DEFAULT 'server'`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) add(ctx context.Context, name, url string, interval int) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO sites(name, url, interval_seconds, sort_order, generation) VALUES (?, ?, ?, (SELECT COALESCE(MAX(sort_order), -1) + 1 FROM sites), lower(hex(randomblob(16)))) ON CONFLICT(url) DO NOTHING`, name, url, interval)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

const siteQuery = `SELECT s.id, s.name, s.url, s.interval_seconds,
  c.checked_at, c.latency_ms, c.ok, c.status_code, c.problem, s.generation,
  cc.checked_at, cc.latency_ms, cc.ok, cc.status_code, cc.problem
FROM sites s LEFT JOIN checks c ON c.id = (
  SELECT id FROM checks WHERE site_id = s.id AND source = 'server' ORDER BY checked_at DESC, id DESC LIMIT 1
) AND s.check_requested = 0
LEFT JOIN checks cc ON cc.id = (
  SELECT id FROM checks WHERE site_id = s.id AND source = 'client' ORDER BY checked_at DESC, id DESC LIMIT 1
)`

func scanSite(row interface{ Scan(...any) error }) (Site, error) {
	var site Site
	var at, latency, code sql.NullInt64
	var ok sql.NullBool
	var problem sql.NullString
	var cat, clatency, ccode sql.NullInt64
	var cok sql.NullBool
	var cproblem sql.NullString
	err := row.Scan(&site.ID, &site.Name, &site.URL, &site.Interval,
		&at, &latency, &ok, &code, &problem, &site.Generation,
		&cat, &clatency, &cok, &ccode, &cproblem)
	if at.Valid {
		site.Last = &Check{at.Int64, latency.Int64, ok.Bool, int(code.Int64), problem.String}
	}
	if cat.Valid {
		site.LastClient = &Check{cat.Int64, clatency.Int64, cok.Bool, int(ccode.Int64), cproblem.String}
	}
	return site, err
}

func (s *Store) sites(ctx context.Context, query string) ([]Site, error) {
	rows, err := s.db.QueryContext(ctx, siteQuery+` ORDER BY s.sort_order, s.id`)
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
		if strings.Contains(strings.ToLower(site.Name+" "+site.URL), strings.ToLower(strings.TrimSpace(query))) {
			sites = append(sites, site)
		}
	}
	return sites, rows.Err()
}

func (s *Store) reorder(ctx context.Context, ids []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sites`).Scan(&count); err != nil {
		return err
	}
	if len(ids) != count {
		return errInvalidOrder
	}
	seen := make(map[int64]bool, len(ids))
	for i, id := range ids {
		if id < 1 || seen[id] {
			return errInvalidOrder
		}
		seen[id] = true
		result, err := tx.ExecContext(ctx, `UPDATE sites SET sort_order = ? WHERE id = ?`, i, id)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return errInvalidOrder
		}
	}
	return tx.Commit()
}

var errInvalidOrder = errors.New("invalid site order")

func (s *Store) move(ctx context.Context, id, target int64, after bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM sites ORDER BY sort_order, id`)
	if err != nil {
		return err
	}
	var ids []int64
	found := false
	for rows.Next() {
		var current int64
		if err := rows.Scan(&current); err != nil {
			rows.Close()
			return err
		}
		if current == id {
			found = true
		} else {
			ids = append(ids, current)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !found || id == target {
		return errInvalidOrder
	}
	index := -1
	for i, current := range ids {
		if current == target {
			index = i
			break
		}
	}
	if index < 0 {
		return errInvalidOrder
	}
	if after {
		index++
	}
	ids = append(ids, 0)
	copy(ids[index+1:], ids[index:])
	ids[index] = id
	for i, current := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE sites SET sort_order = ? WHERE id = ?`, i, current); err != nil {
			return err
		}
	}
	return tx.Commit()
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
FROM checks WHERE checked_at BETWEEN ? AND ? AND source = 'server' GROUP BY 1, 2`, start, bucketSeconds, start, end)
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
WHERE site_id = ? AND checked_at BETWEEN ? AND ? AND source = 'server' ORDER BY checked_at, id`, id, start, end)
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
	return s.recordCheck(ctx, id, "", check, "server")
}

func (s *Store) recordSite(ctx context.Context, site Site, check Check) error {
	return s.recordCheck(ctx, site.ID, site.Generation, check, "server")
}

func (s *Store) recordClient(ctx context.Context, id int64, check Check) error {
	return s.recordCheck(ctx, id, "", check, "client")
}

func (s *Store) recordCheck(ctx context.Context, id int64, generation string, check Check, source string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM sites WHERE id = ?`, id).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if generation != "" && generation != current {
		return nil
	}
	if source == "server" {
		if _, err := tx.ExecContext(ctx, `UPDATE sites SET check_requested = 0 WHERE id = ?`, id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO checks(site_id, checked_at, latency_ms, ok, status_code, problem, source)
VALUES (?, ?, ?, ?, ?, ?, ?)`, id, check.At, check.Latency, check.OK, check.Code, check.Problem, source); err != nil {
		return err
	}
	if source == "server" {
		if check.OK {
			_, err = tx.ExecContext(ctx, `UPDATE incidents SET resolved_at = ? WHERE site_id = ? AND resolved_at IS NULL`, check.At, id)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO incidents(site_id, started_at, problem) VALUES (?, ?, ?)
ON CONFLICT(site_id) WHERE resolved_at IS NULL DO NOTHING`, id, check.At, check.Problem)
		}
		if err != nil {
			return fmt.Errorf("update incident: %w", err)
		}
	}
	return tx.Commit()
}

var errDuplicateSite = errors.New("site URL already exists")

func (s *Store) update(ctx context.Context, id int64, name, address string, interval int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous string
	if err := tx.QueryRowContext(ctx, `SELECT url FROM sites WHERE id = ?`, id).Scan(&previous); err != nil {
		return err
	}
	var duplicate bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sites WHERE url = ? AND id <> ?)`, address, id).Scan(&duplicate); err != nil {
		return err
	}
	if duplicate {
		return errDuplicateSite
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sites SET name = ?, url = ?, interval_seconds = ?, generation = lower(hex(randomblob(16))), check_requested = CASE WHEN ? THEN 1 ELSE check_requested END WHERE id = ?`, name, address, interval, previous != address, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) delete(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM checks WHERE site_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM incidents WHERE site_id = ?`, id); err != nil {
		return err
	}
	var legacyOrder bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'site_order')`).Scan(&legacyOrder); err != nil {
		return err
	}
	if legacyOrder {
		if _, err := tx.ExecContext(ctx, `DELETE FROM site_order WHERE site_id = ?`, id); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM sites WHERE id = ?`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}