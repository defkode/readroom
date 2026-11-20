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
	`
-- Articles older than this many days are deleted (see PruneExpired); 0 keeps them forever.
ALTER TABLE feeds ADD COLUMN keep_days INTEGER NOT NULL DEFAULT 365;
`,
	`
-- When the entry was marked read; NULL while unread, and for entries read before this was recorded.
ALTER TABLE entries ADD COLUMN read_at TEXT;
`,
	`
-- Feed health: how many fetches in a row have failed, since when, and when one last worked.
ALTER TABLE feeds ADD COLUMN fail_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE feeds ADD COLUMN failing_since TEXT;
ALTER TABLE feeds ADD COLUMN last_ok_at TEXT;
UPDATE feeds SET fail_count = 1, failing_since = last_fetched_at WHERE last_error != '';
UPDATE feeds SET last_ok_at = last_fetched_at WHERE last_error = '';
`,
	`
-- When the owner hid the entry; hidden entries stay out of every list and count.
ALTER TABLE entries ADD COLUMN hidden_at TEXT;
`,
	`
-- Unread lists are ordered by when an entry arrived here, so a new feed's articles are not buried.
CREATE INDEX entries_arrival ON entries (created_at DESC, published_at DESC, id DESC);
-- How much of a new feed's backlog starts out unread; applied once, by its first successful fetch.
ALTER TABLE feeds ADD COLUMN initial_unread TEXT NOT NULL DEFAULT '';
`,
	`
-- Whether the feed sends only a teaser of each article: NULL until an article's page has been compared.
ALTER TABLE feeds ADD COLUMN teaser INTEGER;
`,
	`
-- Article HTML moves out of the entries table. Lists, counts and searches read only
-- entries; with the text in the same row, every one of them had to step over it.
CREATE TABLE entry_bodies (
	entry_id          INTEGER PRIMARY KEY REFERENCES entries(id) ON DELETE CASCADE,
	content           TEXT NOT NULL DEFAULT '', -- HTML as the feed sent it
	preserved_content TEXT                      -- the full local copy, if one was kept
);
INSERT INTO entry_bodies (entry_id, content, preserved_content) SELECT id, content, preserved_content FROM entries;
ALTER TABLE entries DROP COLUMN content;
ALTER TABLE entries DROP COLUMN preserved_content;

-- Indexes for the lists that are actually shown. Unread is what is opened most and,
-- over time, the smallest part of the table: its indexes hold unread rows only.
DROP INDEX entries_arrival;
CREATE INDEX entries_unread ON entries (created_at DESC, published_at DESC, id DESC)
	WHERE read = 0 AND hidden_at IS NULL;
CREATE INDEX entries_feed_unread ON entries (feed_id, created_at DESC, published_at DESC, id DESC)
	WHERE read = 0 AND hidden_at IS NULL;
CREATE INDEX entries_starred ON entries (published_at DESC, id DESC) WHERE starred = 1;
-- "Is this entry in a readlist?" for every row of a list, and "is this image still used?".
CREATE INDEX readlist_entries_entry ON readlist_entries (entry_id);
CREATE INDEX entry_assets_sha ON entry_assets (sha256);
`,
	`
-- Spoken versions of articles, asked for one at a time and made by a text-to-speech
-- client outside the server. A row without data is a request still waiting.
CREATE TABLE entry_audio (
	entry_id     INTEGER PRIMARY KEY REFERENCES entries(id) ON DELETE CASCADE,
	requested_at TEXT NOT NULL,
	created_at   TEXT,                     -- when the recording arrived; NULL while waiting
	mime         TEXT NOT NULL DEFAULT '',
	voice        TEXT NOT NULL DEFAULT '', -- what read it, as the client names itself
	error        TEXT NOT NULL DEFAULT '', -- why the last attempt failed, if it did
	data         BLOB                      -- last, so reading the columns above never touches it
);
`,
	`
-- Articles the owner saved by address, which no feed brought, live under one built-in
-- feed of kind 'saved' that is never fetched, so everything an article can do works
-- for them. It is created when first needed (see SavedFeed).
ALTER TABLE feeds ADD COLUMN kind TEXT NOT NULL DEFAULT '';
`,
	`
-- How far into an article the owner has read, as a share of its text (0 not begun, 1 finished),
-- and when that was last reported: what lets another device offer to go on from there.
ALTER TABLE entries ADD COLUMN position REAL NOT NULL DEFAULT 0;
ALTER TABLE entries ADD COLUMN position_at TEXT;
CREATE INDEX entries_reading ON entries (position_at DESC) WHERE position > 0;
`,
	`
-- Translations of articles into the owner's language, asked for one at a time and made
-- by a client outside the server, like the spoken versions. A row without created_at is
-- a request still waiting. One per article: asking in another language replaces it.
CREATE TABLE entry_translations (
	entry_id     INTEGER PRIMARY KEY REFERENCES entries(id) ON DELETE CASCADE,
	language     TEXT NOT NULL,            -- what it is, or is to be, translated into: "pl"
	requested_at TEXT NOT NULL,
	created_at   TEXT,                     -- when the translation arrived; NULL while waiting
	model        TEXT NOT NULL DEFAULT '', -- what translated it, as the client names itself
	error        TEXT NOT NULL DEFAULT '', -- why the last attempt failed, if it did
	title        TEXT NOT NULL DEFAULT '',
	summary      TEXT NOT NULL DEFAULT '', -- the AI summary, if there was one to translate
	content      TEXT NOT NULL DEFAULT ''  -- the article's HTML with its text translated
);
`,
	`
-- AI clean-up: for the feeds that have it on, every article is read by a model outside
-- the server before it is shown. The model says what language it is in and which of its
-- blocks belong to the article; what is left is kept as Markdown beside the original
-- (clean), with the HTML that Markdown renders to (clean_html) so that showing it costs
-- nothing. An article that arrives while its feed has clean-up on is held back — in no
-- list and no count — until that has happened or been given up on.
ALTER TABLE feeds ADD COLUMN ai_cleanup INTEGER NOT NULL DEFAULT 0;
ALTER TABLE entries ADD COLUMN language TEXT NOT NULL DEFAULT '';      -- "en", as the model judged it
ALTER TABLE entries ADD COLUMN held INTEGER NOT NULL DEFAULT 0;        -- 1 while waiting for its clean copy
ALTER TABLE entries ADD COLUMN cleaned_at TEXT;
ALTER TABLE entries ADD COLUMN cleanup_model TEXT NOT NULL DEFAULT '';
ALTER TABLE entries ADD COLUMN cleanup_error TEXT NOT NULL DEFAULT ''; -- why it could not be done, if it could not
ALTER TABLE entry_bodies ADD COLUMN clean TEXT;
ALTER TABLE entry_bodies ADD COLUMN clean_html TEXT;
-- The queue is read often and is nearly always a handful of rows.
CREATE INDEX entries_uncleaned ON entries (feed_id) WHERE cleaned_at IS NULL AND cleanup_error = '';
`,
}

func Open(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "temp_store(MEMORY)")
	q.Add("_pragma", "cache_size(-16000)") // 16 MB of pages per connection
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

// Close lets SQLite refresh its query-planner statistics, then closes the database.
func (s *Store) Close() error {
	s.db.Exec(`PRAGMA optimize`)
	return s.db.Close()
}

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
	if v < len(migrations) {
		// Rare, so worth doing properly: give back the space a migration freed and
		// gather the statistics the query planner chooses indexes by.
		if _, err := s.db.Exec(`VACUUM`); err != nil {
			return fmt.Errorf("vacuum: %w", err)
		}
		if _, err := s.db.Exec(`ANALYZE`); err != nil {
			return fmt.Errorf("analyze: %w", err)
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
