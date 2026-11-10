package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Each backlog policy leaves the right part of a new feed's articles unread.
func TestTrimBacklog(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Two articles from this month and twelve older ones, a day apart.
	var items []NewEntry
	for i := 0; i < 14; i++ {
		age := i
		if i >= 2 {
			age = 60 + i
		}
		items = append(items, NewEntry{GUID: fmt.Sprint(i), Title: fmt.Sprint("Post ", i),
			PublishedAt: time.Now().UTC().AddDate(0, 0, -age).Format(TimeFormat)})
	}
	for policy, want := range map[string]int{
		UnreadAll: 14, "": 14, UnreadNone: 0,
		UnreadLatest: LatestCount,   // the ten newest
		UnreadRecent: RecentAtLeast, // two from this month, topped up to three
	} {
		feed, _, err := st.CreateFeed(ctx, &Feed{URL: "https://example.com/" + policy})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddEntries(ctx, feed.ID, items); err != nil {
			t.Fatal(err)
		}
		if err := st.TrimBacklog(ctx, feed.ID, policy); err != nil {
			t.Fatal(err)
		}
		got, err := st.CountUnread(ctx, ReadScope{FeedID: feed.ID})
		if err != nil {
			t.Fatal(err)
		}
		if int(got) != want {
			t.Errorf("policy %q leaves %d unread, want %d", policy, got, want)
		}
	}
}
