package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"readroom/internal/store"
)

const rss = `<?xml version="1.0"?><rss version="2.0"><channel><title>Site</title>
<item><title>One</title><link>/one</link><guid>1</guid><description>Short.</description></item>
</channel></rss>`

// site serves a small blog: a feed, pages that point to it in different ways,
// and addresses that redirect.
func site(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	feed := func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, rss) }
	mux.HandleFunc("/feed.xml", feed)
	mux.HandleFunc("/linked", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head><link rel="alternate" type="application/atom+xml" href="/hidden/atom"></head></html>`)
	})
	mux.HandleFunc("/hidden/atom", feed)
	mux.HandleFunc("/article", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html><body>An article</body></html>") })
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/feed.xml", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/elsewhere-for-now", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/feed.xml", http.StatusFound)
	})
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "gone", http.StatusGone) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fetcher(t *testing.T) *Fetcher {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestProbeFindsTheFeed(t *testing.T) {
	f, srv := fetcher(t), site(t)
	for given, want := range map[string]string{
		"/feed.xml": "/feed.xml",    // a feed is itself
		"/linked":   "/hidden/atom", // a page that advertises its feed
		"/article":  "/feed.xml",    // any page of a site with a feed at a customary path
		"/moved":    "/feed.xml",    // an address that has moved for good
	} {
		got, err := f.Probe(context.Background(), srv.URL+given)
		if err != nil || got != srv.URL+want {
			t.Errorf("Probe(%s) = %q, %v; want %s", given, strings.TrimPrefix(got, srv.URL), err, want)
		}
	}
}

func TestProbeRejectsWhatIsNotAFeed(t *testing.T) {
	f, srv := fetcher(t), site(t)
	var status *StatusError
	if _, err := f.Probe(context.Background(), srv.URL+"/gone"); !errors.As(err, &status) || status.Code != http.StatusGone {
		t.Errorf("a gone address: %v, want HTTP 410", err)
	}
	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, "<html><body>No feed anywhere</body></html>")
	}))
	t.Cleanup(bare.Close)
	if _, err := f.Probe(context.Background(), bare.URL+"/"); !errors.Is(err, ErrNoFeed) {
		t.Errorf("a site without a feed: %v, want ErrNoFeed", err)
	}
}

// Only a permanent redirect changes where the feed lives.
func TestRedirects(t *testing.T) {
	f, srv := fetcher(t), site(t)
	for path, moved := range map[string]bool{"/feed.xml": false, "/moved": true, "/elsewhere-for-now": false} {
		got, err := f.fetchFeed(context.Background(), &store.Feed{URL: srv.URL + path})
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if (got.movedTo != "") != moved {
			t.Errorf("%s: movedTo = %q, want moved = %v", path, got.movedTo, moved)
		}
		// Links in the feed are resolved against where it was served from.
		if len(got.items) != 1 || got.items[0].URL != srv.URL+"/one" {
			t.Errorf("%s: items %+v", path, got.items)
		}
	}
}

func TestFeedLinks(t *testing.T) {
	page := `<html><head>
<link rel="stylesheet" href="/style.css">
<link rel="alternate" type="application/rss+xml" href="rss.xml">
<link rel="alternate" type="text/html" hreflang="pl" href="/pl/">
<link rel="ALTERNATE" type="application/feed+json" href="https://other.example/feed.json">
</head></html>`
	got := feedLinks([]byte(page), "https://example.com/blog/")
	want := []string{"https://example.com/blog/rss.xml", "https://other.example/feed.json"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("feedLinks = %v, want %v", got, want)
	}
}
