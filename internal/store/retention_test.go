package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Expired articles are deleted unless the owner singled them out, and a feed that
// still lists them does not bring them back.
func TestRetention(t *testing.T) {
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
	old := time.Now().UTC().AddDate(0, 0, -60).Format(TimeFormat)
	items := []NewEntry{
		{GUID: "plain", Title: "Plain", PublishedAt: old},
		{GUID: "starred", Title: "Starred", PublishedAt: old},
		{GUID: "listed", Title: "Listed", PublishedAt: old},
		{GUID: "fresh", Title: "Fresh", PublishedAt: Now()},
	}
	if n, err := st.AddEntries(ctx, feed.ID, items); err != nil || n != 4 {
		t.Fatalf("add entries to a feed that keeps everything: %d, %v", n, err)
	}
	ids := map[string]int64{}
	all, _ := st.Entries(ctx, EntryFilter{})
	for _, e := range all {
		ids[e.Title] = e.ID
	}
	starred := true
	if err := st.UpdateEntry(ctx, ids["Starred"], EntryPatch{Starred: &starred}); err != nil {
		t.Fatal(err)
	}
	list, err := st.CreateReadlist(ctx, "Later")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddToReadlist(ctx, list.ID, ids["Listed"]); err != nil {
		t.Fatal(err)
	}

	if n, err := st.PruneExpired(ctx); err != nil || n != 0 {
		t.Fatalf("prune with keep forever: %d, %v", n, err)
	}
	days := 30
	if err := st.UpdateFeed(ctx, feed.ID, FeedPatch{KeepDays: &days}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.PruneExpired(ctx); err != nil || n != 1 {
		t.Fatalf("prune after 30 days: deleted %d, %v; want only the plain old article", n, err)
	}
	left, _ := st.Entries(ctx, EntryFilter{})
	if len(left) != 3 {
		t.Fatalf("%d articles left, want the starred, the listed and the fresh one", len(left))
	}
	for _, e := range left {
		if e.Title == "Plain" {
			t.Fatal("the plain expired article survived")
		}
	}
	// The feed still lists the deleted article: it must not come back.
	if n, err := st.AddEntries(ctx, feed.ID, items); err != nil || n != 0 {
		t.Fatalf("re-adding the feed's items: %d new, %v; want none", n, err)
	}
}
