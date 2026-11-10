// Package store is the SQLite persistence layer. Everything readroom knows,
// including preserved article images, lives in one database file.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

// TimeFormat is how every timestamp is stored: UTC, sortable as text.
const TimeFormat = "2006-01-02T15:04:05Z"

func Now() string { return time.Now().UTC().Format(TimeFormat) }

type Store struct {
	db *sql.DB
}

var migrations = []string{
	`
CREATE TABLE feeds (
	id              INTEGER PRIMARY KEY,
	url             TEXT NOT NULL UNIQUE,
	site_url        TEXT NOT NULL DEFAULT '',
	feed_title      TEXT NOT NULL DEFAULT '',
	custom_title    TEXT NOT NULL DEFAULT '',
	category        TEXT NOT NULL DEFAULT '',
	preserve        INTEGER NOT NULL DEFAULT 0,
	preserve_since  TEXT,
	etag            TEXT NOT NULL DEFAULT '',
	last_modified   TEXT NOT NULL DEFAULT '',
	last_fetched_at TEXT,
	last_error      TEXT NOT NULL DEFAULT '',
	created_at      TEXT NOT NULL
);

CREATE TABLE entries (
	id                 INTEGER PRIMARY KEY,
	feed_id            INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
	guid               TEXT NOT NULL,
	url                TEXT NOT NULL DEFAULT '',
	title              TEXT NOT NULL DEFAULT '',
	author             TEXT NOT NULL DEFAULT '',
	published_at       TEXT NOT NULL,
	content            TEXT NOT NULL DEFAULT '',
	preserved_content  TEXT,
	preserved_at       TEXT,
	preserve_requested INTEGER NOT NULL DEFAULT 0,
	preserve_attempts  INTEGER NOT NULL DEFAULT 0,
	preserve_error     TEXT NOT NULL DEFAULT '',
	summary            TEXT NOT NULL DEFAULT '',
	summary_model      TEXT NOT NULL DEFAULT '',
	read               INTEGER NOT NULL DEFAULT 0,
	starred            INTEGER NOT NULL DEFAULT 0,
	created_at         TEXT NOT NULL,
	UNIQUE (feed_id, guid)
);
CREATE INDEX entries_published ON entries (published_at DESC, id DESC);
CREATE INDEX entries_feed ON entries (feed_id, published_at DESC);

CREATE TABLE assets (
	sha256 TEXT PRIMARY KEY,
	mime   TEXT NOT NULL,
	data   BLOB NOT NULL
);
CREATE TABLE entry_assets (
	entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
	sha256   TEXT NOT NULL REFERENCES assets(sha256),
	PRIMARY KEY (entry_id, sha256)
);

CREATE TABLE readlists (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL
);
CREATE TABLE readlist_entries (
	readlist_id INTEGER NOT NULL REFERENCES readlists(id) ON DELETE CASCADE,
	entry_id    INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
	added_at    TEXT NOT NULL,
	PRIMARY KEY (readlist_id, entry_id)
);
`,
	`
ALTER TABLE feeds ADD COLUMN ai_summary INTEGER NOT NULL DEFAULT 0;
ALTER TABLE entries ADD COLUMN excerpt TEXT NOT NULL DEFAULT '';
-- Plain-text length of the best available body; -1 until computed.
ALTER TABLE entries ADD COLUMN text_length INTEGER NOT NULL DEFAULT -1;
`,
	`
-- Lead image of entries that are mostly a picture, shown as a thumbnail in lists.
ALTER TABLE entries ADD COLUMN image TEXT NOT NULL DEFAULT '';
-- Excerpts are now built from the first real paragraph: recompute them all.
UPDATE entries SET excerpt = '', text_length = -1;
`,
}

func Open(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := s.backfillText(); err != nil {
		db.Close()
		return nil, fmt.Errorf("backfill: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Snapshot writes a consistent, compacted copy of the whole database to path.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(path, `'`, `''`)+`'`)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
