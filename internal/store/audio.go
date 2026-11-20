package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// AudioKeepDays is how long a recording, or a request nobody answered, is kept.
// Speech is large and made to be listened to once; it can always be asked for again.
const AudioKeepDays = 30

// The states of an entry's spoken version, as the API reports them.
const (
	AudioQueued = "queued"
	AudioReady  = "ready"
	AudioFailed = "failed"
)

// audioState is the state of entry e's spoken version, NULL when it has none.
const audioState = `SELECT CASE WHEN au.created_at IS NOT NULL THEN 'ready' WHEN au.error != '' THEN 'failed' ELSE 'queued' END
FROM entry_audio au WHERE au.entry_id = e.id`

// Audio is one recording.
type Audio struct {
	MIME      string
	Voice     string
	CreatedAt string
	Data      []byte
}

// RequestAudio queues an entry to be read aloud. A recording that exists is
// left alone; a request that failed is queued again.
func (s *Store) RequestAudio(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO entry_audio (entry_id, requested_at) SELECT id, ? FROM entries WHERE id = ?
ON CONFLICT (entry_id) DO UPDATE SET requested_at = excluded.requested_at, error = ''
WHERE entry_audio.created_at IS NULL`, Now(), id)
	return err
}

// PendingAudio lists the entries waiting to be read aloud, in the order they
// were asked for, and how many are waiting in total. Like a summary, a
// recording waits for the article's full local copy when one is on its way.
//
// exclude lists entry ids to leave out, so a client can step past one it could
// not read.
func (s *Store) PendingAudio(ctx context.Context, exclude []int64, limit int) ([]*Entry, int, error) {
	where := `a.created_at IS NULL AND a.error = '' AND e.hidden_at IS NULL AND NOT ` + needsCopyFirst
	var args []any
	if len(exclude) > 0 {
		where += ` AND e.id NOT IN (?` + strings.Repeat(`, ?`, len(exclude)-1) + `)`
		for _, id := range exclude {
			args = append(args, id)
		}
	}
	from := ` FROM entry_audio a JOIN entries e ON e.id = a.entry_id JOIN feeds f ON f.id = e.feed_id`
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*)`+from+` WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryCols+`, b.content, b.preserved_content, b.clean, b.clean_html`+from+`
JOIN entry_bodies b ON b.entry_id = e.id WHERE `+where+` ORDER BY a.requested_at, e.id LIMIT ?`, append(args, limit)...)
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

// SaveAudio stores an entry's recording, replacing any earlier one. It does not
// need a request to answer: a client may read an article nobody asked for.
func (s *Store) SaveAudio(ctx context.Context, id int64, a Audio) error {
	now := Now()
	return affected(s.db.ExecContext(ctx, `
INSERT INTO entry_audio (entry_id, requested_at, created_at, mime, voice, data) SELECT id, ?, ?, ?, ?, ? FROM entries WHERE id = ?
ON CONFLICT (entry_id) DO UPDATE SET created_at = excluded.created_at, mime = excluded.mime, voice = excluded.voice,
	data = excluded.data, error = ''`, now, now, a.MIME, a.Voice, a.Data, id))
}

// AudioFailed records why a request could not be answered, which takes it out
// of the queue until it is asked for again.
func (s *Store) AudioFailed(ctx context.Context, id int64, cause string) error {
	return affected(s.db.ExecContext(ctx,
		`UPDATE entry_audio SET error = ? WHERE entry_id = ? AND created_at IS NULL`, cause, id))
}

// Audio loads an entry's recording.
func (s *Store) Audio(ctx context.Context, id int64) (*Audio, error) {
	var a Audio
	err := s.db.QueryRowContext(ctx,
		`SELECT mime, voice, created_at, data FROM entry_audio WHERE entry_id = ? AND created_at IS NOT NULL`, id).
		Scan(&a.MIME, &a.Voice, &a.CreatedAt, &a.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &a, err
}

// DeleteAudio removes an entry's recording, or withdraws the request for one.
func (s *Store) DeleteAudio(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM entry_audio WHERE entry_id = ?`, id))
}

// PruneAudio deletes recordings made more than AudioKeepDays ago, and requests
// that have waited as long.
func (s *Store) PruneAudio(ctx context.Context) (int64, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -AudioKeepDays).Format(TimeFormat)
	res, err := s.db.ExecContext(ctx, `DELETE FROM entry_audio WHERE coalesce(created_at, requested_at) < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
