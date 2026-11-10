package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"readroom/internal/htmlx"
)

type Entry struct {
	ID             int64   `json:"id"`
	FeedID         int64   `json:"feed_id"`
	FeedTitle      string  `json:"feed_title"`
	Category       string  `json:"category"`
	GUID           string  `json:"guid"`
	URL            string  `json:"url"`
	Title          string  `json:"title"`
	Author         string  `json:"author"`
	PublishedAt    string  `json:"published_at"`
	Excerpt        string  `json:"excerpt"` // short teaser from the feed, or the opening of the text
	Summary        string  `json:"summary"` // written by a model through the API; empty until then
	SummaryModel   string  `json:"summary_model"`
	Image          string  `json:"image"`           // lead image of a picture-only entry, else empty
	TextLength     int     `json:"text_length"`     // plain-text characters in the best available body
	ReadingMinutes int     `json:"reading_minutes"` // estimated from text_length; 0 when there is too little text to say
	Read           bool    `json:"read"`
	ReadAt         *string `json:"read_at"` // when it was marked read; null if unread or not recorded
	Starred        bool    `json:"starred"`
	InReadlist     bool    `json:"in_readlist"` // member of at least one readlist
	Preserved      bool    `json:"preserved"`
	PreservedAt    *string `json:"preserved_at"`
	PreserveError  string  `json:"preserve_error,omitempty"`
	CreatedAt      string  `json:"created_at"`

	// Only filled when a single entry is loaded.
	Content          string  `json:"content,omitempty"`           // HTML from the feed
	PreservedContent *string `json:"preserved_content,omitempty"` // full article HTML, images at /assets/…
	Readlists        []int64 `json:"readlists,omitempty"`
}

// Body is the best HTML available: the preserved copy, else the feed's content.
func (e *Entry) Body() string {
	if e.PreservedContent != nil && *e.PreservedContent != "" {
		return *e.PreservedContent
	}
	return e.Content
}

const entryCols = `
e.id, e.feed_id, coalesce(nullif(f.custom_title,''), nullif(f.feed_title,''), f.url), f.category,
e.guid, e.url, e.title, e.author, e.published_at, e.excerpt, e.image, e.summary, e.summary_model, e.text_length,
e.read, e.read_at, e.starred, EXISTS (SELECT 1 FROM readlist_entries rl WHERE rl.entry_id = e.id),
e.preserved_at IS NOT NULL, e.preserved_at, e.preserve_error, e.created_at`

func scanEntry(r interface{ Scan(...any) error }, extra ...any) (*Entry, error) {
	var e Entry
	dest := []any{&e.ID, &e.FeedID, &e.FeedTitle, &e.Category, &e.GUID, &e.URL, &e.Title, &e.Author,
		&e.PublishedAt, &e.Excerpt, &e.Image, &e.Summary, &e.SummaryModel, &e.TextLength, &e.Read, &e.ReadAt, &e.Starred, &e.InReadlist, &e.Preserved, &e.PreservedAt,
		&e.PreserveError, &e.CreatedAt}
	if len(extra) > 0 {
		dest = append(dest, &e.Content, &e.PreservedContent)
	}
	if err := r.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	e.ReadingMinutes = readingMinutes(e.TextLength)
	return &e, nil
}

// readingChars is how many characters of plain text are read in a minute: about
// 230 words. Counting characters rather than words keeps the estimate fair for
// languages with longer words.
const readingChars = 1400

// readingMinutes estimates how long a text of this many characters takes to
// read. Notes and picture posts too short to plan around get no estimate.
func readingMinutes(chars int) int {
	if chars < readingChars/2 {
		return 0
	}
	return (chars + readingChars/2) / readingChars
}

type EntryFilter struct {
	FeedID     int64
	Category   *string
	ReadlistID int64
	Read       *bool
	Starred    *bool
	Preserved  *bool
	HasSummary *bool
	Query      string // substring match on title and summary
	Before     string // pagination cursor: "published_at,id" of the last item seen
	Limit      int
	Oldest     bool // oldest first instead of newest first
}

// conditions turns the filter's selection (not its paging or order) into SQL.
func (f EntryFilter) conditions() (where []string, args []any) {
	add := func(cond string, a ...any) {
		where = append(where, cond)
		args = append(args, a...)
	}
	if f.FeedID != 0 {
		add("e.feed_id = ?", f.FeedID)
	}
	if f.Category != nil {
		add("f.category = ?", *f.Category)
	}
	if f.ReadlistID != 0 {
		add("e.id IN (SELECT entry_id FROM readlist_entries WHERE readlist_id = ?)", f.ReadlistID)
	}
	if f.Read != nil {
		add("e.read = ?", b2i(*f.Read))
	}
	if f.Starred != nil {
		add("e.starred = ?", b2i(*f.Starred))
	}
	if f.Preserved != nil {
		if *f.Preserved {
			add("e.preserved_at IS NOT NULL")
		} else {
			add("e.preserved_at IS NULL")
		}
	}
	if f.HasSummary != nil {
		if *f.HasSummary {
			add("e.summary != ''")
		} else {
			add("e.summary = ''")
		}
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		add(`(e.title LIKE ? ESCAPE '\' OR e.summary LIKE ? ESCAPE '\' OR e.excerpt LIKE ? ESCAPE '\')`, like, like, like)
	}
	return where, args
}

// CountEntries reports how many entries the filter selects, ignoring paging.
func (s *Store) CountEntries(ctx context.Context, f EntryFilter) (n int64, err error) {
	where, args := f.conditions()
	q := `SELECT count(*) FROM entries e JOIN feeds f ON f.id = e.feed_id`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	err = s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

func (s *Store) Entries(ctx context.Context, f EntryFilter) ([]*Entry, error) {
	where, args := f.conditions()
	add := func(cond string, a ...any) {
		where = append(where, cond)
		args = append(args, a...)
	}
	cmp, order := "<", "DESC"
	if f.Oldest {
		cmp, order = ">", "ASC"
	}
	if pub, idStr, ok := strings.Cut(f.Before, ","); ok {
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
			add("(e.published_at, e.id) "+cmp+" (?, ?)", pub, id)
		}
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	q := `SELECT ` + entryCols + ` FROM entries e JOIN feeds f ON f.id = e.feed_id`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY e.published_at ` + order + `, e.id ` + order + ` LIMIT ?`
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []*Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Entry loads one entry with its content and readlist memberships.
func (s *Store) Entry(ctx context.Context, id int64) (*Entry, error) {
	e, err := scanEntry(s.db.QueryRowContext(ctx, `SELECT `+entryCols+`, e.content, e.preserved_content
FROM entries e JOIN feeds f ON f.id = e.feed_id WHERE e.id = ?`, id), true)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT readlist_id FROM readlist_entries WHERE entry_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return nil, err
		}
		e.Readlists = append(e.Readlists, rid)
	}
	return e, rows.Err()
}

// NewEntry is an item parsed from a feed.
type NewEntry struct {
	GUID, URL, Title, Author, PublishedAt, Content string
	Excerpt                                        string // short teaser published by the feed, if it has one
}

// AddEntries inserts items not seen before and returns how many were new.
func (s *Store) AddEntries(ctx context.Context, feedID int64, items []NewEntry) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Leave out what PruneExpired would delete again, or a feed that still lists
	// old articles would bring them back as unread on every fetch.
	var expired, preserve bool
	var cutoff string
	var since *string
	if err := tx.QueryRowContext(ctx, `SELECT keep_days > 0, `+expiryCutoff+`, preserve, preserve_since FROM feeds f WHERE id = ?`,
		feedID).Scan(&expired, &cutoff, &preserve, &since); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO entries (feed_id, guid, url, title, author, published_at, content, excerpt, image, text_length, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (feed_id, guid) DO NOTHING`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	now, added := Now(), 0
	for _, it := range items {
		keptLocally := preserve && (since == nil || it.PublishedAt >= *since)
		if expired && it.PublishedAt < cutoff && !keptLocally {
			continue
		}
		excerpt, image, length := derive(it.Title, it.Content)
		if it.Excerpt != "" {
			excerpt = it.Excerpt
		}
		res, err := stmt.ExecContext(ctx, feedID, it.GUID, it.URL, it.Title, it.Author, it.PublishedAt, it.Content,
			excerpt, image, length, now)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		added += int(n)
		if n == 0 && it.Excerpt != "" {
			// Already stored: adopt the feed's teaser if we only had our own lead
			// (entries saved before teasers were kept), or if the feed changed it.
			if _, err := tx.ExecContext(ctx,
				`UPDATE entries SET excerpt = ? WHERE feed_id = ? AND guid = ? AND excerpt != ?`,
				it.Excerpt, feedID, it.GUID, it.Excerpt); err != nil {
				return 0, err
			}
		}
	}
	return added, tx.Commit()
}

// EntryPatch holds the fields a client may change; nil fields are left alone.
type EntryPatch struct {
	Read         *bool
	Starred      *bool
	Summary      *string
	SummaryModel *string
}

func (s *Store) UpdateEntry(ctx context.Context, id int64, p EntryPatch) error {
	var set []string
	var args []any
	if p.Read != nil {
		// read_at is stamped when an unread entry becomes read, and cleared with it.
		if *p.Read {
			set = append(set, "read_at = CASE WHEN read = 0 THEN ? ELSE read_at END", "read = 1")
			args = append(args, Now())
		} else {
			set = append(set, "read = 0", "read_at = NULL")
		}
	}
	if p.Starred != nil {
		set = append(set, "starred = ?")
		args = append(args, b2i(*p.Starred))
	}
	if p.Summary != nil {
		set = append(set, "summary = ?")
		args = append(args, strings.TrimSpace(*p.Summary))
	}
	if p.SummaryModel != nil {
		set = append(set, "summary_model = ?")
		args = append(args, *p.SummaryModel)
	}
	if len(set) == 0 {
		return nil
	}
	args = append(args, id)
	return affected(s.db.ExecContext(ctx, `UPDATE entries SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...))
}

// ReadScope selects the unread entries a bulk "mark as read" covers.
type ReadScope struct {
	FeedID   int64
	Category *string
	Through  int64 // when set, only this entry and those older than it
}

func (sc ReadScope) where() (string, []any) {
	q := ` WHERE read = 0`
	var args []any
	if sc.FeedID != 0 {
		q += ` AND feed_id = ?`
		args = append(args, sc.FeedID)
	}
	if sc.Category != nil {
		q += ` AND feed_id IN (SELECT id FROM feeds WHERE category = ?)`
		args = append(args, *sc.Category)
	}
	if sc.Through != 0 {
		// The same order lists use: newest first by publication time, then id.
		q += ` AND (published_at, id) <= (SELECT published_at, id FROM entries WHERE id = ?)`
		args = append(args, sc.Through)
	}
	return q, args
}

// CountUnread reports how many entries MarkRead would mark for this scope.
func (s *Store) CountUnread(ctx context.Context, sc ReadScope) (n int64, err error) {
	where, args := sc.where()
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM entries`+where, args...).Scan(&n)
	return n, err
}

// MarkRead marks the unread entries in scope as read and returns their ids, so
// the caller can offer to undo exactly that change with MarkUnread.
func (s *Store) MarkRead(ctx context.Context, sc ReadScope) ([]int64, error) {
	where, args := sc.where()
	rows, err := s.db.QueryContext(ctx, `UPDATE entries SET read = 1, read_at = ?`+where+` RETURNING id`,
		append([]any{Now()}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MarkUnread marks the given entries as unread again.
func (s *Store) MarkUnread(ctx context.Context, ids []int64) (int64, error) {
	list, err := json.Marshal(ids)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE entries SET read = 0, read_at = NULL WHERE read = 1 AND id IN (SELECT value FROM json_each(?))`, string(list))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- retention --------------------------------------------------------------

// expiryCutoff is the publication time before which feed f's articles have expired.
const expiryCutoff = `strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-' || f.keep_days || ' days')`

// PruneExpired deletes articles older than their feed's keep_days. Anything the
// owner has singled out stays: starred articles, those in a readlist, and those
// with a local copy made, requested or due under the feed's preserve setting.
func (s *Store) PruneExpired(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
DELETE FROM entries WHERE id IN (
	SELECT e.id FROM entries e JOIN feeds f ON f.id = e.feed_id
	WHERE f.keep_days > 0 AND e.published_at < `+expiryCutoff+`
	  AND e.starred = 0 AND e.preserved_at IS NULL AND e.preserve_requested = 0
	  AND NOT (f.preserve = 1 AND (f.preserve_since IS NULL OR e.published_at >= f.preserve_since))
	  AND e.id NOT IN (SELECT entry_id FROM readlist_entries))`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- summaries --------------------------------------------------------------

// excerptLead is how much of the text stands in when a feed has no teaser.
const excerptLead = 280

// pictureOnly is the text length under which an entry with an image counts as a
// picture post (a comic, a photo) and gets a thumbnail in lists.
const pictureOnly = 200

// derive computes what lists show for an entry from its title and body.
func derive(title, content string) (excerpt, image string, length int) {
	length = utf8.RuneCountInString(htmlx.Flat(content))
	excerpt = htmlx.Excerpt(content, title, excerptLead)
	if length < pictureOnly {
		image, _ = htmlx.FirstImage(content)
	}
	return excerpt, image, length
}

// needsCopyFirst matches entries whose full local copy is still on its way, so a
// summary written now would be based on the feed's excerpt alone.
const needsCopyFirst = `(e.preserved_at IS NULL AND e.preserve_attempts < ` + "3" + `
  AND (e.preserve_requested = 1
       OR (f.preserve = 1 AND (f.preserve_since IS NULL OR e.published_at >= f.preserve_since))))`

const pendingSummaryWhere = `f.ai_summary = 1 AND e.summary = '' AND e.text_length >= ? AND NOT ` + needsCopyFirst

// PendingSummaries lists entries waiting for an AI summary, newest first: their
// feed has AI summaries on, they have none yet, and the text is at least
// minChars long. It also returns how many are waiting in total.
//
// exclude lists entry ids to leave out, so a client working through the queue
// one article at a time can step past one it could not summarise.
func (s *Store) PendingSummaries(ctx context.Context, minChars int, feedID int64, exclude []int64, limit int) ([]*Entry, int, error) {
	where, args := pendingSummaryWhere, []any{minChars}
	if feedID != 0 {
		where += ` AND e.feed_id = ?`
		args = append(args, feedID)
	}
	if len(exclude) > 0 {
		where += ` AND e.id NOT IN (?` + strings.Repeat(`, ?`, len(exclude)-1) + `)`
		for _, id := range exclude {
			args = append(args, id)
		}
	}
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM entries e JOIN feeds f ON f.id = e.feed_id WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryCols+`, e.content, e.preserved_content
FROM entries e JOIN feeds f ON f.id = e.feed_id WHERE `+where+`
ORDER BY e.published_at DESC, e.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	entries := []*Entry{}
	for rows.Next() {
		e, err := scanEntry(rows, true)
		if err != nil {
			return nil, 0, err
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// backfillText computes excerpt, thumbnail and text length for entries stored
// before those columns existed, or after the way they are derived changed.
func (s *Store) backfillText() error {
	for {
		rows, err := s.db.Query(`SELECT id, title, content, preserved_content, excerpt FROM entries WHERE text_length < 0 LIMIT 200`)
		if err != nil {
			return err
		}
		type row struct {
			id             int64
			excerpt, image string
			length         int
		}
		var batch []row
		for rows.Next() {
			var id int64
			var title, content, kept string
			var preserved *string
			if err := rows.Scan(&id, &title, &content, &preserved, &kept); err != nil {
				rows.Close()
				return err
			}
			excerpt, image, length := derive(title, content)
			if kept != "" {
				excerpt = kept
			}
			if preserved != nil && *preserved != "" {
				length = utf8.RuneCountInString(htmlx.Flat(*preserved))
				if local, _ := htmlx.FirstImage(*preserved); image != "" && local != "" {
					image = local
				}
			}
			batch = append(batch, row{id, excerpt, image, length})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		for _, r := range batch {
			if _, err := tx.Exec(`UPDATE entries SET excerpt = ?, image = ?, text_length = ? WHERE id = ?`,
				r.excerpt, r.image, r.length, r.id); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
}

// --- preservation -----------------------------------------------------------

const maxPreserveAttempts = 3

// RequestPreserve queues one entry for preservation regardless of feed settings.
func (s *Store) RequestPreserve(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx,
		`UPDATE entries SET preserve_requested = 1, preserve_attempts = 0, preserve_error = '' WHERE id = ?`, id))
}

// PendingPreserve returns entries that should have a local copy but don't yet:
// explicitly requested ones, plus those covered by their feed's preserve setting.
func (s *Store) PendingPreserve(ctx context.Context, limit int) ([]*Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryCols+`, e.content, e.preserved_content
FROM entries e JOIN feeds f ON f.id = e.feed_id
WHERE e.preserved_at IS NULL AND e.preserve_attempts < ?
  AND (e.preserve_requested = 1
       OR (f.preserve = 1 AND (f.preserve_since IS NULL OR e.published_at >= f.preserve_since)))
ORDER BY e.preserve_requested DESC, e.published_at DESC LIMIT ?`, maxPreserveAttempts, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []*Entry
	for rows.Next() {
		e, err := scanEntry(rows, true)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

type Asset struct {
	SHA256 string
	MIME   string
	Data   []byte
}

// SavePreserved stores the full article HTML and the images it references.
func (s *Store) SavePreserved(ctx context.Context, id int64, html string, assets []Asset) error {
	// A picture-only entry's thumbnail should come from the local copy too.
	localImage, _ := htmlx.FirstImage(html)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, a := range assets {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO assets (sha256, mime, data) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
			a.SHA256, a.MIME, a.Data); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO entry_assets (entry_id, sha256) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			id, a.SHA256); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE entries SET preserved_content = ?, preserved_at = ?, preserve_requested = 0, preserve_error = '',
       text_length = ?, image = CASE WHEN image != '' AND ? != '' THEN ? ELSE image END
WHERE id = ?`, html, Now(), utf8.RuneCountInString(htmlx.Flat(html)), localImage, localImage, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PreserveFailed(ctx context.Context, id int64, cause error) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE entries SET preserve_attempts = preserve_attempts + 1, preserve_error = ? WHERE id = ?`,
		cause.Error(), id)
	return err
}

func (s *Store) Asset(ctx context.Context, sha string) (*Asset, error) {
	a := Asset{SHA256: sha}
	err := s.db.QueryRowContext(ctx, `SELECT mime, data FROM assets WHERE sha256 = ?`, sha).Scan(&a.MIME, &a.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &a, err
}

func (s *Store) pruneAssets(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM assets WHERE sha256 NOT IN (SELECT sha256 FROM entry_assets)`)
	return err
}
