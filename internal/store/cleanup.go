package store

import (
	"context"
	"database/sql"
	"strings"
	"unicode/utf8"

	"readroom/internal/htmlx"
)

// AI clean-up, as the store sees it: which articles wait for a clean copy, where
// that copy is kept, and the gate that holds a new article back until it has one.

// needsCleanupFirst matches entries that will get a clean copy and have none yet,
// so that what is made from an article's text — a summary — is made from the
// clean text.
const needsCleanupFirst = `(f.ai_cleanup = 1 AND e.cleaned_at IS NULL AND e.cleanup_error = '')`

// PendingCleanup lists the entries waiting for AI clean-up and how many are
// waiting in total: those held back first, since nobody can read them yet, then
// the unread, then the rest, newest first. Like a summary, clean-up waits for the
// article's full local copy when one is on its way.
//
// exclude lists entry ids to leave out, so a client can step past one it could
// not do.
func (s *Store) PendingCleanup(ctx context.Context, exclude []int64, limit int) ([]*Entry, int, error) {
	where := needsCleanupFirst + ` AND e.hidden_at IS NULL AND NOT ` + needsCopyFirst
	var args []any
	if len(exclude) > 0 {
		where += ` AND e.id NOT IN (?` + strings.Repeat(`, ?`, len(exclude)-1) + `)`
		for _, id := range exclude {
			args = append(args, id)
		}
	}
	from := ` FROM entries e JOIN feeds f ON f.id = e.feed_id`
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*)`+from+` WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryCols+`, b.content, b.preserved_content, b.clean, b.clean_html`+from+`
JOIN entry_bodies b ON b.entry_id = e.id WHERE `+where+`
ORDER BY e.held DESC, e.read, e.published_at DESC, e.id DESC LIMIT ?`, append(args, limit)...)
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

// Cleanup is the outcome of AI clean-up for one article.
type Cleanup struct {
	Language string // "en"; may be empty when there was too little text to tell
	Model    string
	Markdown string // the clean copy
	HTML     string // what that Markdown renders to
}

// arrive lets a held article into the lists, as arriving now: it is new to the
// owner at this moment, and Unread is in order of arrival.
const arrive = `held = 0, created_at = CASE WHEN held = 1 THEN ? ELSE created_at END`

// SaveCleanup stores an article's clean copy and lets the article through if it
// was held back for it. Whatever was made from the old text and shows it — a
// translation — goes.
func (s *Store) SaveCleanup(ctx context.Context, id int64, c Cleanup) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	if err := affected(tx.ExecContext(ctx, `
UPDATE entries SET language = ?, cleaned_at = ?, cleanup_model = ?, cleanup_error = '', text_length = ?, `+arrive+` WHERE id = ?`,
		c.Language, now, c.Model, utf8.RuneCountInString(htmlx.Flat(c.HTML)), now, id)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE entry_bodies SET clean = ?, clean_html = ? WHERE entry_id = ?`, c.Markdown, c.HTML, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, staleTranslation, id); err != nil {
		return err
	}
	return tx.Commit()
}

// CleanupFailed records why an article got no clean copy, which takes it out of
// the queue and lets it through as it is: an article nobody can clean should not
// stay out of sight for good.
func (s *Store) CleanupFailed(ctx context.Context, id int64, cause string) error {
	return affected(s.db.ExecContext(ctx,
		`UPDATE entries SET cleanup_error = ?, `+arrive+` WHERE id = ? AND cleaned_at IS NULL`, cause, Now(), id))
}

// DeleteCleanup removes an article's clean copy, or the record of a failure, so
// that it is shown as it came and, if its feed has clean-up on, is done again.
func (s *Store) DeleteCleanup(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM entries WHERE id = ?`, id).Scan(&one); err != nil {
		return ErrNotFound
	}
	if err := staleCleanup(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, staleTranslation, id); err != nil {
		return err
	}
	return tx.Commit()
}

// staleCleanup forgets an article's clean copy because the text it was made from
// has been replaced or it is to be made again; the article goes back in the
// queue. Its length goes back to that of its source.
func staleCleanup(ctx context.Context, tx *sql.Tx, id int64) error {
	var content string
	var preserved *string
	if err := tx.QueryRowContext(ctx, `SELECT content, preserved_content FROM entry_bodies WHERE entry_id = ?`, id).
		Scan(&content, &preserved); err != nil {
		return err
	}
	if preserved != nil && *preserved != "" {
		content = *preserved
	}
	if _, err := tx.ExecContext(ctx, `UPDATE entry_bodies SET clean = NULL, clean_html = NULL WHERE entry_id = ?`, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
UPDATE entries SET cleaned_at = NULL, cleanup_model = '', cleanup_error = '', language = '', text_length = ? WHERE id = ?`,
		utf8.RuneCountInString(htmlx.Flat(content)), id)
	return err
}

// releaseHeld lets through the held articles that match where.
func (s *Store) releaseHeld(ctx context.Context, where string, args ...any) error {
	_, err := s.db.ExecContext(ctx, `UPDATE entries SET `+arrive+` WHERE held = 1 AND `+where, append([]any{Now()}, args...)...)
	return err
}
