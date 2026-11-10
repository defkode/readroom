package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Moving article bodies to their own table must lose nothing from a database
// that already holds articles, readlists and images.
func TestBodiesMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	bodies := -1 // index of the migration that creates entry_bodies
	for i, m := range migrations {
		if strings.Contains(m, "CREATE TABLE entry_bodies") {
			bodies = i
		}
	}
	if bodies < 0 {
		t.Fatal("no migration creates entry_bodies")
	}

	// A database as the previous version left it.
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < bodies; i++ {
		if _, err := old.Exec(migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	for _, stmt := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, bodies),
		`INSERT INTO feeds (id, url, created_at) VALUES (1, 'https://example.com/feed', '2026-01-01T00:00:00Z')`,
		`INSERT INTO entries (id, feed_id, guid, title, published_at, content, preserved_content, preserved_at, starred, text_length, created_at)
		 VALUES (7, 1, 'a', 'Kept', '2026-01-02T00:00:00Z', '<p>from the feed</p>', '<p>the full copy</p>', '2026-01-03T00:00:00Z', 1, 12, '2026-01-02T00:00:00Z'),
		        (8, 1, 'b', 'Plain', '2026-01-04T00:00:00Z', '<p>only the feed</p>', NULL, NULL, 0, 13, '2026-01-04T00:00:00Z')`,
		`INSERT INTO readlists (id, name, created_at) VALUES (1, 'Later', '2026-01-01T00:00:00Z')`,
		`INSERT INTO readlist_entries (readlist_id, entry_id, added_at) VALUES (1, 7, '2026-01-05T00:00:00Z')`,
		`INSERT INTO assets (sha256, mime, data) VALUES ('abc', 'image/png', x'00')`,
		`INSERT INTO entry_assets (entry_id, sha256) VALUES (7, 'abc')`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open and migrate: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	kept, err := st.Entry(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Content != "<p>from the feed</p>" || kept.PreservedContent == nil || *kept.PreservedContent != "<p>the full copy</p>" ||
		!kept.Starred || !kept.Preserved || !kept.InReadlist {
		t.Errorf("entry with a local copy after migration: %+v", kept)
	}
	plain, err := st.Entry(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Content != "<p>only the feed</p>" || plain.PreservedContent != nil {
		t.Errorf("plain entry after migration: content %q, copy %v", plain.Content, plain.PreservedContent)
	}
	if listed, err := st.ReadlistEntries(ctx, 1); err != nil || len(listed) != 1 || listed[0].ID != 7 {
		t.Errorf("readlist after migration: %v, %v", listed, err)
	}
	if _, err := st.Asset(ctx, "abc"); err != nil {
		t.Errorf("image after migration: %v", err)
	}
	var columns int
	st.db.QueryRow(`SELECT count(*) FROM pragma_table_info('entries') WHERE name IN ('content', 'preserved_content')`).Scan(&columns)
	if columns != 0 {
		t.Errorf("entries still has %d body columns", columns)
	}
	// Lists use the partial index made for them.
	var plan string
	rows, err := st.db.Query(`EXPLAIN QUERY PLAN SELECT e.id FROM entries e WHERE e.hidden_at IS NULL AND e.read = 0
		ORDER BY e.created_at DESC, e.published_at DESC, e.id DESC LIMIT 40`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		rows.Scan(&id, &parent, &unused, &detail)
		plan += detail + "; "
	}
	rows.Close()
	if !strings.Contains(plan, "entries_unread") || strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("unread list does not use its index: %s", plan)
	}
}
