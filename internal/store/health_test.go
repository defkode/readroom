package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// A feed is broken after a run of failures, at once when it is gone for good,
// and healthy again after one fetch that works.
func TestFeedHealth(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	feed, _, err := st.CreateFeed(ctx, &Feed{URL: "https://example.com/feed"})
	if err != nil {
		t.Fatal(err)
	}
	get := func() *Feed {
		f, err := st.Feed(ctx, feed.ID)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	boom := errors.New("HTTP 500")

	for i := 1; i <= BrokenAfter; i++ {
		if err := st.FeedFetched(ctx, feed.ID, FetchOutcome{Err: boom}); err != nil {
			t.Fatal(err)
		}
		f := get()
		if f.FailCount != i || f.FailingSince == nil || f.Broken != (i >= BrokenAfter) {
			t.Fatalf("after %d failures: count %d, since %v, broken %v", i, f.FailCount, f.FailingSince, f.Broken)
		}
	}
	if err := st.FeedFetched(ctx, feed.ID, FetchOutcome{Title: "Example", MovedTo: "https://example.com/new"}); err != nil {
		t.Fatal(err)
	}
	if f := get(); f.Broken || f.FailCount != 0 || f.FailingSince != nil || f.LastError != "" || f.LastOKAt == nil || f.URL != "https://example.com/new" {
		t.Fatalf("after a fetch that worked and moved: %+v", f)
	}
	if err := st.FeedFetched(ctx, feed.ID, FetchOutcome{Err: errors.New("HTTP 410"), Gone: true}); err != nil {
		t.Fatal(err)
	}
	if f := get(); !f.Broken {
		t.Fatalf("a feed that is gone should be broken at once: count %d", f.FailCount)
	}
}
