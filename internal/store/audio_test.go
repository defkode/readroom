package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Recordings, and requests nobody answered, are deleted after AudioKeepDays.
func TestPruneAudio(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	feed, _, err := st.CreateFeed(ctx, &Feed{URL: "https://example.com/feed", KeepDays: 0})
	if err != nil {
		t.Fatal(err)
	}
	items := []NewEntry{
		{GUID: "old", Title: "Old recording", PublishedAt: Now()},
		{GUID: "new", Title: "New recording", PublishedAt: Now()},
		{GUID: "stale", Title: "Stale request", PublishedAt: Now()},
		{GUID: "waiting", Title: "Waiting request", PublishedAt: Now()},
	}
	if _, err := st.AddEntries(ctx, feed.ID, items); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	all, _ := st.Entries(ctx, EntryFilter{})
	for _, e := range all {
		ids[e.Title] = e.ID
	}
	for _, title := range []string{"Old recording", "New recording"} {
		if err := st.SaveAudio(ctx, ids[title], Audio{MIME: "audio/mpeg", Voice: "v", Data: []byte("sound")}); err != nil {
			t.Fatal(err)
		}
	}
	for _, title := range []string{"Stale request", "Waiting request"} {
		if err := st.RequestAudio(ctx, ids[title]); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().UTC().AddDate(0, 0, -AudioKeepDays-1).Format(TimeFormat)
	if _, err := st.db.Exec(`UPDATE entry_audio SET created_at = ? WHERE entry_id = ?`, past, ids["Old recording"]); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE entry_audio SET requested_at = ? WHERE entry_id = ?`, past, ids["Stale request"]); err != nil {
		t.Fatal(err)
	}

	if n, err := st.PruneAudio(ctx); err != nil || n != 2 {
		t.Fatalf("prune: %d deleted, %v", n, err)
	}
	want := map[string]string{"Old recording": "", "New recording": AudioReady, "Stale request": "", "Waiting request": AudioQueued}
	all, _ = st.Entries(ctx, EntryFilter{})
	for _, e := range all {
		if e.Audio != want[e.Title] {
			t.Errorf("%s: audio %q, want %q", e.Title, e.Audio, want[e.Title])
		}
	}
	// The articles themselves stay.
	if len(all) != 4 {
		t.Errorf("%d articles left, want 4", len(all))
	}
}
