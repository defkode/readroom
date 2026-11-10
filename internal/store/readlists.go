package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type Readlist struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Count     int    `json:"count"`
}

const readlistSelect = `
SELECT r.id, r.name, r.created_at,
       (SELECT count(*) FROM readlist_entries re JOIN entries e ON e.id = re.entry_id
        WHERE re.readlist_id = r.id AND e.hidden_at IS NULL)
FROM readlists r`

func scanReadlist(r interface{ Scan(...any) error }) (*Readlist, error) {
	var l Readlist
	err := r.Scan(&l.ID, &l.Name, &l.CreatedAt, &l.Count)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &l, err
}

func (s *Store) Readlists(ctx context.Context) ([]*Readlist, error) {
	rows, err := s.db.QueryContext(ctx, readlistSelect+` ORDER BY r.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lists := []*Readlist{}
	for rows.Next() {
		l, err := scanReadlist(rows)
		if err != nil {
			return nil, err
		}
		lists = append(lists, l)
	}
	return lists, rows.Err()
}

func (s *Store) Readlist(ctx context.Context, id int64) (*Readlist, error) {
	return scanReadlist(s.db.QueryRowContext(ctx, readlistSelect+` WHERE r.id = ?`, id))
}

// CreateReadlist returns the list with this name, creating it if needed.
func (s *Store) CreateReadlist(ctx context.Context, name string) (*Readlist, error) {
	name = strings.TrimSpace(name)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO readlists (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`, name, Now()); err != nil {
		return nil, err
	}
	return scanReadlist(s.db.QueryRowContext(ctx, readlistSelect+` WHERE r.name = ?`, name))
}

func (s *Store) RenameReadlist(ctx context.Context, id int64, name string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE readlists SET name = ? WHERE id = ?`, strings.TrimSpace(name), id))
}

func (s *Store) DeleteReadlist(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM readlists WHERE id = ?`, id))
}

func (s *Store) AddToReadlist(ctx context.Context, listID, entryID int64) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO readlist_entries (readlist_id, entry_id, added_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		listID, entryID, Now())
	return err
}

func (s *Store) RemoveFromReadlist(ctx context.Context, listID, entryID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM readlist_entries WHERE readlist_id = ? AND entry_id = ?`, listID, entryID)
	return err
}

// ReadlistEntries loads the full entries of a list, in the order they were added.
func (s *Store) ReadlistEntries(ctx context.Context, listID int64) ([]*Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryCols+`, e.content, e.preserved_content
FROM readlist_entries re JOIN entries e ON e.id = re.entry_id JOIN feeds f ON f.id = e.feed_id
WHERE re.readlist_id = ? AND e.hidden_at IS NULL ORDER BY re.added_at, e.id`, listID)
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
