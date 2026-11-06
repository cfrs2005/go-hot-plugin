package main

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

type pluginRow struct {
	ID        int64   `json:"id"`
	Vendor    string  `json:"vendor"`
	Version   string  `json:"version"`
	SHA256    string  `json:"sha256"`
	Signature string  `json:"signature"`
	Path      string  `json:"path"`
	CreatedAt string  `json:"created_at"`
	RetiredAt *string `json:"retired_at"`
	Active    bool    `json:"active"`
}

type store struct{ db *sql.DB }

var errNotFound = errors.New("not_found")

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者，串行化即可
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS plugins(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  vendor TEXT NOT NULL, version TEXT NOT NULL,
  sha256 TEXT NOT NULL, signature TEXT NOT NULL, path TEXT NOT NULL,
  created_at TEXT NOT NULL, retired_at TEXT,
  UNIQUE(vendor, version));
CREATE TABLE IF NOT EXISTS active(vendor TEXT PRIMARY KEY, version TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS subscribers(id INTEGER PRIMARY KEY AUTOINCREMENT, callback_url TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL);`)
	if err != nil {
		return nil, err
	}
	return &store{db: db}, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *store) insertPlugin(p pluginRow) error {
	_, err := s.db.Exec(`INSERT INTO plugins(vendor,version,sha256,signature,path,created_at) VALUES(?,?,?,?,?,?)`,
		p.Vendor, p.Version, p.SHA256, p.Signature, p.Path, now())
	return err
}

const selectPlugin = `SELECT p.id,p.vendor,p.version,p.sha256,p.signature,p.path,p.created_at,p.retired_at,
  CASE WHEN a.version IS NULL THEN 0 ELSE 1 END
  FROM plugins p LEFT JOIN active a ON a.vendor=p.vendor AND a.version=p.version`

func scanPlugin(sc interface{ Scan(...any) error }) (pluginRow, error) {
	var p pluginRow
	err := sc.Scan(&p.ID, &p.Vendor, &p.Version, &p.SHA256, &p.Signature, &p.Path, &p.CreatedAt, &p.RetiredAt, &p.Active)
	return p, err
}

func (s *store) getPlugin(vendor, version string) (pluginRow, error) {
	p, err := scanPlugin(s.db.QueryRow(selectPlugin+` WHERE p.vendor=? AND p.version=?`, vendor, version))
	if errors.Is(err, sql.ErrNoRows) {
		return p, errNotFound
	}
	return p, err
}

func (s *store) listPlugins() ([]pluginRow, error) {
	rows, err := s.db.Query(selectPlugin + ` ORDER BY p.vendor, p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []pluginRow{}
	for rows.Next() {
		p, err := scanPlugin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// activeRow 返回 vendor 当前活跃版本行；没有则 errNotFound。
func (s *store) activeRow(vendor string) (pluginRow, error) {
	var v string
	err := s.db.QueryRow(`SELECT version FROM active WHERE vendor=?`, vendor).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return pluginRow{}, errNotFound
	}
	if err != nil {
		return pluginRow{}, err
	}
	return s.getPlugin(vendor, v)
}

func (s *store) setActive(vendor, version string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO active(vendor,version,updated_at) VALUES(?,?,?)
		ON CONFLICT(vendor) DO UPDATE SET version=excluded.version, updated_at=excluded.updated_at`, vendor, version, now()); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE plugins SET retired_at=NULL WHERE vendor=? AND version=?`, vendor, version); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) retire(vendor, version string) error {
	_, err := s.db.Exec(`UPDATE plugins SET retired_at=? WHERE vendor=? AND version=?`, now(), vendor, version)
	return err
}

func (s *store) subscribe(url string) error {
	_, err := s.db.Exec(`INSERT INTO subscribers(callback_url,created_at) VALUES(?,?) ON CONFLICT(callback_url) DO NOTHING`, url, now())
	return err
}

func (s *store) subscribers() ([]string, error) {
	rows, err := s.db.Query(`SELECT callback_url FROM subscribers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
