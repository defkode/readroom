package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type Feed struct {
	ID            int64   `json:"id"`
	URL           string  `json:"url"`
	SiteURL       string  `json:"site_url"`
	Title         string  `json:"title"`        // what the UI shows: custom title, else the feed's own
	FeedTitle     string  `json:"feed_title"`   // title as published by the feed
	CustomTitle   string  `json:"custom_title"` // user's rename; empty = use feed_title
	Category      string  `json:"category"`
	Preserve      bool    `json:"preserve"`       // keep a full local copy of new articles
	PreserveSince *string `json:"preserve_since"` // only articles published at/after this; null = all
	ETag          string  `json:"-"`
	LastModified  string  `json:"-"`
	LastFetchedAt *string `json:"last_fetched_at"`
	LastError     string  `json:"last_error"`
	CreatedAt     string  `json:"created_at"`
	Unread        int     `json:"unread"`
	Total         int     `json:"total"`
}

const feedSelect = `
SELECT f.id, f.url, f.site_url, f.feed_title, f.custom_title, f.category, f.preserve,
       f.preserve_since, f.etag, f.last_modified, f.last_fetched_at, f.last_error, f.created_at,
       (SELECT count(*) FROM entries e WHERE e.feed_id = f.id AND e.read = 0),
       (SELECT count(*) FROM entries e WHERE e.feed_id = f.id)
FROM feeds f`

func scanFeed(r interface{ Scan(...any) error }) (*Feed, error) {
	var f Feed
	err := r.Scan(&f.ID, &f.URL, &f.SiteURL, &f.FeedTitle, &f.CustomTitle, &f.Category, &f.Preserve,
		&f.PreserveSince, &f.ETag, &f.LastModified, &f.LastFetchedAt, &f.LastError, &f.CreatedAt,
		&f.Unread, &f.Total)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.Title = f.CustomTitle
	if f.Title == "" {
		f.Title = f.FeedTitle
	}
	if f.Title == "" {
		f.Title = f.URL
	}
	return &f, nil
}

func (s *Store) Feeds(ctx context.Context) ([]*Feed, error) {
	rows, err := s.db.QueryContext(ctx, feedSelect+
		` ORDER BY f.category COLLATE NOCASE, coalesce(nullif(f.custom_title,''), f.feed_title, f.url) COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	feeds := []*Feed{}
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, err
		}
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

func (s *Store) Feed(ctx context.Context, id int64) (*Feed, error) {
	return scanFeed(s.db.QueryRowContext(ctx, feedSelect+` WHERE f.id = ?`, id))
}

// FeedsDue returns feeds never fetched or last fetched before the cutoff.
func (s *Store) FeedsDue(ctx context.Context, olderThan time.Duration) ([]*Feed, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(TimeFormat)
	rows, err := s.db.QueryContext(ctx, feedSelect+
		` WHERE f.last_fetched_at IS NULL OR f.last_fetched_at < ? ORDER BY f.last_fetched_at`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var feeds []*Feed
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, err
		}
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

// CreateFeed adds a feed, or returns the existing one with the same URL.
func (s *Store) CreateFeed(ctx context.Context, f *Feed) (*Feed, bool, error) {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO feeds (url, custom_title, category, preserve, preserve_since, created_at)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (url) DO NOTHING`,
		f.URL, f.CustomTitle, f.Category, b2i(f.Preserve), f.PreserveSince, Now())
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		existing, err := scanFeed(s.db.QueryRowContext(ctx, feedSelect+` WHERE f.url = ?`, f.URL))
		return existing, false, err
	}
	id, _ := res.LastInsertId()
	created, err := s.Feed(ctx, id)
	return created, true, err
}

// FeedPatch holds user-editable feed settings; nil fields are left unchanged.
type FeedPatch struct {
	URL           *string
	Title         *string // empty string resets to the feed's own title
	Category      *string
	Preserve      *bool
	PreserveSince **string // pointer to nil clears the date
}

func (s *Store) UpdateFeed(ctx context.Context, id int64, p FeedPatch) error {
	var set []string
	var args []any
	if p.URL != nil {
		// A new URL means cached validators no longer apply.
		set = append(set, "url = ?", "etag = ''", "last_modified = ''", "last_fetched_at = NULL")
		args = append(args, *p.URL)
	}
	if p.Title != nil {
		set = append(set, "custom_title = ?")
		args = append(args, strings.TrimSpace(*p.Title))
	}
	if p.Category != nil {
		set = append(set, "category = ?")
		args = append(args, strings.TrimSpace(*p.Category))
	}
	if p.Preserve != nil {
		set = append(set, "preserve = ?")
		args = append(args, b2i(*p.Preserve))
	}
	if p.PreserveSince != nil {
		set = append(set, "preserve_since = ?")
		args = append(args, *p.PreserveSince)
	}
	if len(set) == 0 {
		_, err := s.Feed(ctx, id)
		return err
	}
	args = append(args, id)
	return affected(s.db.ExecContext(ctx, `UPDATE feeds SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...))
}

func (s *Store) DeleteFeed(ctx context.Context, id int64) error {
	if err := affected(s.db.ExecContext(ctx, `DELETE FROM feeds WHERE id = ?`, id)); err != nil {
		return err
	}
	return s.pruneAssets(ctx)
}

// FeedFetched records the outcome of a fetch. fetchErr may be nil.
func (s *Store) FeedFetched(ctx context.Context, id int64, feedTitle, siteURL, etag, lastModified string, fetchErr error) error {
	if fetchErr != nil {
		_, err := s.db.ExecContext(ctx, `UPDATE feeds SET last_fetched_at = ?, last_error = ? WHERE id = ?`,
			Now(), fetchErr.Error(), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE feeds SET last_fetched_at = ?, last_error = '',
       feed_title = CASE WHEN ? != '' THEN ? ELSE feed_title END,
       site_url   = CASE WHEN ? != '' THEN ? ELSE site_url END,
       etag = ?, last_modified = ?
WHERE id = ?`, Now(), feedTitle, feedTitle, siteURL, siteURL, etag, lastModified, id)
	return err
}

// Categories lists the distinct non-empty category names in use.
func (s *Store) Categories(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT category FROM feeds WHERE category != '' ORDER BY category COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cats := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cats = append(cats, c)
	}
	return cats, rows.Err()
}
