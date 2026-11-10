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
	AISummary     bool    `json:"ai_summary"`     // queue this feed's longer articles for an AI summary
	KeepDays      int     `json:"keep_days"`      // delete articles older than this many days; 0 = keep forever
	ETag          string  `json:"-"`
	LastModified  string  `json:"-"`
	LastFetchedAt *string `json:"last_fetched_at"`
	LastError     string  `json:"last_error"`
	FailCount     int     `json:"fail_count"`      // fetches failed in a row; 0 while healthy
	FailingSince  *string `json:"failing_since"`   // first failure of the current run
	LastOKAt      *string `json:"last_ok_at"`      // last fetch that worked
	LatestEntryAt *string `json:"latest_entry_at"` // newest article it has
	Broken        bool    `json:"broken"`          // has failed often enough to need attention
	Stale         bool    `json:"stale"`           // answers, but has published nothing for a long time
	CreatedAt     string  `json:"created_at"`
	Unread        int     `json:"unread"`
	Total         int     `json:"total"`
}

const feedSelect = `
SELECT f.id, f.url, f.site_url, f.feed_title, f.custom_title, f.category, f.preserve,
       f.preserve_since, f.ai_summary, f.keep_days, f.etag, f.last_modified, f.last_fetched_at, f.last_error, f.created_at,
       f.fail_count, f.failing_since, f.last_ok_at, (SELECT max(e.published_at) FROM entries e WHERE e.feed_id = f.id),
       (SELECT count(*) FROM entries e WHERE e.feed_id = f.id AND e.read = 0),
       (SELECT count(*) FROM entries e WHERE e.feed_id = f.id)
FROM feeds f`

func scanFeed(r interface{ Scan(...any) error }) (*Feed, error) {
	var f Feed
	err := r.Scan(&f.ID, &f.URL, &f.SiteURL, &f.FeedTitle, &f.CustomTitle, &f.Category, &f.Preserve,
		&f.PreserveSince, &f.AISummary, &f.KeepDays, &f.ETag, &f.LastModified, &f.LastFetchedAt, &f.LastError, &f.CreatedAt,
		&f.FailCount, &f.FailingSince, &f.LastOKAt, &f.LatestEntryAt,
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
	f.Broken = f.FailCount >= BrokenAfter
	if !f.Broken && f.LatestEntryAt != nil {
		f.Stale = *f.LatestEntryAt < time.Now().UTC().Add(-StaleAfter).Format(TimeFormat)
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

// FeedsDue returns the feeds whose next fetch is due: never fetched, or last
// fetched longer ago than the interval. A failing feed is retried less and less
// often (the interval doubles with each failure, up to a day), so a dead address
// is not asked every half hour for ever.
func (s *Store) FeedsDue(ctx context.Context, interval time.Duration) ([]*Feed, error) {
	now := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx, feedSelect+
		` WHERE f.last_fetched_at IS NULL OR f.last_fetched_at < ? ORDER BY f.last_fetched_at`,
		now.Add(-interval).Format(TimeFormat))
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
		if f.LastFetchedAt != nil && f.FailCount > 0 {
			wait := min(interval<<min(f.FailCount, 10), maxFetchWait)
			if *f.LastFetchedAt >= now.Add(-wait).Format(TimeFormat) {
				continue
			}
		}
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

// BrokenAfter is how many fetches in a row must fail before a feed is reported
// as broken: one timeout is not news, an afternoon of them is.
const BrokenAfter = 3

// StaleAfter is how long a working feed may publish nothing before that is
// pointed out; the blog may have moved or stopped.
const StaleAfter = 180 * 24 * time.Hour

// maxFetchWait caps how far apart retries of a failing feed drift.
const maxFetchWait = 24 * time.Hour

// DefaultKeepDays is how long a new feed's articles are kept unless it says otherwise.
const DefaultKeepDays = 365

// CreateFeed adds a feed, or returns the existing one with the same URL.
func (s *Store) CreateFeed(ctx context.Context, f *Feed) (*Feed, bool, error) {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO feeds (url, custom_title, category, preserve, preserve_since, ai_summary, keep_days, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (url) DO NOTHING`,
		f.URL, f.CustomTitle, f.Category, b2i(f.Preserve), f.PreserveSince, b2i(f.AISummary), f.KeepDays, Now())
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
	AISummary     *bool
	KeepDays      *int // 0 = keep forever
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
	if p.AISummary != nil {
		set = append(set, "ai_summary = ?")
		args = append(args, b2i(*p.AISummary))
	}
	if p.KeepDays != nil {
		set = append(set, "keep_days = ?")
		args = append(args, *p.KeepDays)
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

// FetchOutcome is what one fetch of a feed found.
type FetchOutcome struct {
	Err  error
	Gone bool // the server says the feed is gone for good (HTTP 410): broken at once

	// On success:
	Title, SiteURL, ETag, LastModified string
	MovedTo                            string // the feed's new address after a permanent redirect
}

// FeedFetched records the outcome of a fetch.
func (s *Store) FeedFetched(ctx context.Context, id int64, o FetchOutcome) error {
	now := Now()
	if o.Err != nil {
		floor := 0
		if o.Gone {
			floor = BrokenAfter
		}
		_, err := s.db.ExecContext(ctx, `
UPDATE feeds SET last_fetched_at = ?, last_error = ?, fail_count = max(fail_count + 1, ?),
       failing_since = coalesce(failing_since, ?)
WHERE id = ?`, now, o.Err.Error(), floor, now, id)
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE feeds SET last_fetched_at = ?, last_ok_at = ?, last_error = '', fail_count = 0, failing_since = NULL,
       feed_title = CASE WHEN ? != '' THEN ? ELSE feed_title END,
       site_url   = CASE WHEN ? != '' THEN ? ELSE site_url END,
       etag = ?, last_modified = ?
WHERE id = ?`, now, now, o.Title, o.Title, o.SiteURL, o.SiteURL, o.ETag, o.LastModified, id); err != nil {
		return err
	}
	if o.MovedTo != "" {
		// Follow the feed to its new home, unless that address is already subscribed to.
		_, err := s.db.ExecContext(ctx, `UPDATE OR IGNORE feeds SET url = ? WHERE id = ?`, o.MovedTo, id)
		return err
	}
	return nil
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
