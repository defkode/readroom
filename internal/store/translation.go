package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// The states of an entry's translation, as the API reports them.
const (
	TranslationQueued = "queued"
	TranslationReady  = "ready"
	TranslationFailed = "failed"
)

// translationState is the state of entry e's translation, NULL when it has none.
const translationState = `SELECT CASE WHEN tr.created_at IS NOT NULL THEN 'ready' WHEN tr.error != '' THEN 'failed' ELSE 'queued' END
FROM entry_translations tr WHERE tr.entry_id = e.id`

// staleTranslation removes a finished translation when the text it was made from
// is replaced: it would no longer be a translation of what the article says.
const staleTranslation = `DELETE FROM entry_translations WHERE entry_id = ? AND created_at IS NOT NULL`

// Translation is one article in another language.
type Translation struct {
	Language  string `json:"language"`
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	Content   string `json:"content"`
}

// RequestTranslation queues an entry to be translated into language. A
// translation into that language that exists is left alone; one that failed, or
// is in another language, is queued again.
func (s *Store) RequestTranslation(ctx context.Context, id int64, language string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO entry_translations (entry_id, language, requested_at) SELECT id, ?, ? FROM entries WHERE id = ?
ON CONFLICT (entry_id) DO UPDATE SET language = excluded.language, requested_at = excluded.requested_at,
	created_at = NULL, model = '', error = '', title = '', summary = '', content = ''
WHERE entry_translations.created_at IS NULL OR entry_translations.language != excluded.language`, language, Now(), id)
	return err
}

// PendingTranslation is an entry waiting to be translated and the language asked for.
type PendingTranslation struct {
	*Entry
	Language string
}

// PendingTranslations lists the entries waiting to be translated, in the order
// they were asked for, and how many are waiting in total. Like a summary, a
// translation waits for the article's full local copy when one is on its way.
//
// exclude lists entry ids to leave out, so a client can step past one it could
// not translate.
func (s *Store) PendingTranslations(ctx context.Context, exclude []int64, limit int) ([]PendingTranslation, int, error) {
	where := `t.created_at IS NULL AND t.error = '' AND e.hidden_at IS NULL AND NOT ` + needsCopyFirst
	var args []any
	if len(exclude) > 0 {
		where += ` AND e.id NOT IN (?` + strings.Repeat(`, ?`, len(exclude)-1) + `)`
		for _, id := range exclude {
			args = append(args, id)
		}
	}
	from := ` FROM entry_translations t JOIN entries e ON e.id = t.entry_id JOIN feeds f ON f.id = e.feed_id`
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*)`+from+` WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryCols+`, b.content, b.preserved_content, t.language`+from+`
JOIN entry_bodies b ON b.entry_id = e.id WHERE `+where+` ORDER BY t.requested_at, e.id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []PendingTranslation{}
	for rows.Next() {
		var p PendingTranslation
		if p.Entry, err = scanEntry(rows, &p.Language); err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}

// SaveTranslation stores an entry's translation, replacing any earlier one. It
// does not need a request to answer.
func (s *Store) SaveTranslation(ctx context.Context, id int64, t Translation) error {
	now := Now()
	return affected(s.db.ExecContext(ctx, `
INSERT INTO entry_translations (entry_id, language, requested_at, created_at, model, title, summary, content)
SELECT id, ?, ?, ?, ?, ?, ?, ? FROM entries WHERE id = ?
ON CONFLICT (entry_id) DO UPDATE SET language = excluded.language, created_at = excluded.created_at, model = excluded.model,
	title = excluded.title, summary = excluded.summary, content = excluded.content, error = ''`,
		t.Language, now, now, t.Model, t.Title, t.Summary, t.Content, id))
}

// TranslationFailed records why a request could not be answered, which takes it
// out of the queue until it is asked for again.
func (s *Store) TranslationFailed(ctx context.Context, id int64, cause string) error {
	return affected(s.db.ExecContext(ctx,
		`UPDATE entry_translations SET error = ? WHERE entry_id = ? AND created_at IS NULL`, cause, id))
}

// Translation loads an entry's translation.
func (s *Store) Translation(ctx context.Context, id int64) (*Translation, error) {
	var t Translation
	err := s.db.QueryRowContext(ctx, `SELECT language, model, created_at, title, summary, content
FROM entry_translations WHERE entry_id = ? AND created_at IS NOT NULL`, id).
		Scan(&t.Language, &t.Model, &t.CreatedAt, &t.Title, &t.Summary, &t.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

// TranslationLanguage is the language an entry's translation was asked for in,
// whether or not it has arrived; empty when none was.
func (s *Store) TranslationLanguage(ctx context.Context, id int64) (string, error) {
	var lang string
	err := s.db.QueryRowContext(ctx, `SELECT language FROM entry_translations WHERE entry_id = ?`, id).Scan(&lang)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return lang, err
}

// DeleteTranslation removes an entry's translation, or withdraws the request for one.
func (s *Store) DeleteTranslation(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM entry_translations WHERE entry_id = ?`, id))
}
