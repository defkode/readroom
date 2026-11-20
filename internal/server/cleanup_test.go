package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"readroom/internal/store"
)

// With AI clean-up on for a feed, a new article is held back until a client has
// judged its blocks; the server builds the clean copy from those judgments and
// only then shows the article.
func TestCleanup(t *testing.T) {
	a, site := newApp(t, func(s *Server) { s.Language = "en" }), origin(t)
	var feed store.Feed
	a.call("POST", "/api/v1/feeds", map[string]any{"url": site.URL + "/feed.xml", "keep_days": 0, "unread": "all", "ai_cleanup": true}, &feed)

	type queue struct {
		Entries []struct {
			ID     int64  `json:"id"`
			Held   bool   `json:"held"`
			Of     string `json:"of"`
			Blocks []struct {
				Kind string `json:"kind"`
				Text string `json:"text"`
			} `json:"blocks"`
		} `json:"entries"`
		Remaining int `json:"remaining"`
	}
	pending := func() queue {
		var q queue
		a.call("GET", "/api/v1/cleanup/pending", nil, &q)
		return q
	}
	listed := func() []store.Entry {
		var list struct {
			Entries []store.Entry `json:"entries"`
		}
		a.call("GET", "/api/v1/entries", nil, &list)
		return list.Entries
	}
	page := func(path string) string {
		req, _ := http.NewRequest("GET", a.url+path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	// Both articles arrived with clean-up on: they are waiting, and nowhere to be seen.
	a.call("GET", "/api/v1/feeds/1", nil, &feed)
	if got := listed(); len(got) != 0 || feed.Unread != 0 || feed.Total != 0 || feed.Held != 2 || feed.Uncleaned != 2 {
		t.Fatalf("before clean-up: %d listed, feed %+v", len(got), feed)
	}
	q := pending()
	if len(q.Entries) != 2 || q.Remaining != 2 || !q.Entries[0].Held || q.Entries[0].ID != 1 ||
		len(q.Entries[0].Blocks) != 1 || q.Entries[0].Blocks[0].Kind != "text" || q.Entries[0].Blocks[0].Text != "Short excerpt." {
		t.Fatalf("queue: %+v", q)
	}

	// One judgment for each block, of the text that was handed out, leaving something.
	put := func(body map[string]any) int {
		return a.call("PUT", "/api/v1/entries/1/cleanup", body, nil).StatusCode
	}
	of := q.Entries[0].Of
	if code := put(map[string]any{"of": of, "roles": []string{"keep", "keep"}}); code != http.StatusUnprocessableEntity {
		t.Errorf("wrong number of roles: status %d", code)
	}
	if code := put(map[string]any{"of": of, "roles": []string{"rewrite"}}); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown role: status %d", code)
	}
	if code := put(map[string]any{"of": of, "roles": []string{"drop"}}); code != http.StatusUnprocessableEntity {
		t.Errorf("everything dropped: status %d", code)
	}
	if code := put(map[string]any{"of": "stale", "roles": []string{"keep"}}); code != http.StatusConflict {
		t.Errorf("judgments of another text: status %d", code)
	}
	if code := put(map[string]any{"of": of, "language": "EN", "model": "test-model", "roles": []string{"h2"}}); code != http.StatusOK {
		t.Fatalf("clean-up: status %d", code)
	}
	var e store.Entry
	a.call("GET", "/api/v1/entries/1", nil, &e)
	if !e.Cleaned || e.Held || e.Language != "en" || e.CleanupModel != "test-model" || e.Clean == nil || *e.Clean != "## Short excerpt.\n" ||
		e.CleanHTML == nil || !strings.Contains(*e.CleanHTML, "<h2>Short excerpt.</h2>") || e.Content != "<p>Short excerpt.</p>" {
		t.Fatalf("after clean-up: %+v", e)
	}
	if got := listed(); len(got) != 1 || got[0].ID != 1 || got[0].Language != "en" {
		t.Fatalf("listed after one was cleaned: %+v", got)
	}

	// The article is read from the clean copy, with the way back to how it came;
	// it is in the owner's language already, so no translation is offered.
	html := page("/entries/1")
	if !strings.Contains(html, `<div class="content" lang="en"><h2>Short excerpt.</h2>`) || !strings.Contains(html, "Show unedited") ||
		strings.Contains(html, "request-translation") {
		t.Error("article is not shown from its clean copy")
	}
	if html := page("/entries/1?unedited=1"); !strings.Contains(html, `<p>Short excerpt.</p></div>`) || !strings.Contains(html, "Show the clean copy") {
		t.Error("the unedited article is not shown when asked for")
	}

	// Turning clean-up off lets the waiting article through as it is.
	a.call("PATCH", "/api/v1/feeds/1", map[string]any{"ai_cleanup": false}, nil)
	if got := listed(); len(got) != 2 || len(pending().Entries) != 0 {
		t.Fatalf("after clean-up was turned off: %d listed, %d queued", len(got), len(pending().Entries))
	}
	// Turning it on again queues what has no clean copy, read or not, without hiding it.
	a.call("PATCH", "/api/v1/feeds/1", map[string]any{"ai_cleanup": true}, nil)
	if q := pending(); len(q.Entries) != 1 || q.Entries[0].ID != 2 || q.Entries[0].Held || len(listed()) != 2 {
		t.Fatalf("after clean-up was turned on again: %+v", q)
	}
	// A client that cannot judge an article says why; it is shown as it came.
	e = store.Entry{} // fields left out of the reply would keep their old values
	a.call("POST", "/api/v1/entries/2/cleanup/error", map[string]string{"error": "model went away"}, &e)
	if e.Cleaned || e.CleanupError != "model went away" || len(pending().Entries) != 0 {
		t.Fatalf("after a failure: %+v", e)
	}
	if !strings.Contains(page("/entries/2"), "model went away") {
		t.Error("article does not say why it has no clean copy")
	}
	// Removing a clean copy shows the article as it came and queues it again.
	e = store.Entry{}
	a.call("DELETE", "/api/v1/entries/1/cleanup", nil, &e)
	if e.Cleaned || e.Clean != nil || e.Language != "" || len(pending().Entries) != 1 {
		t.Fatalf("after the clean copy was removed: %+v", e)
	}
}

// An article that arrives while it is held is let through with a failure, too.
func TestCleanupFailureReleasesAHeldArticle(t *testing.T) {
	a, site := newApp(t), origin(t)
	a.call("POST", "/api/v1/feeds", map[string]any{"url": site.URL + "/feed.xml", "keep_days": 0, "unread": "all", "ai_cleanup": true}, nil)
	var e store.Entry
	a.call("POST", "/api/v1/entries/1/cleanup/error", map[string]string{"error": "no"}, &e)
	if e.Held || e.CleanupError != "no" {
		t.Fatalf("after a failure: %+v", e)
	}
	var list struct {
		Entries []store.Entry `json:"entries"`
	}
	a.call("GET", "/api/v1/entries", nil, &list)
	if len(list.Entries) != 1 || list.Entries[0].ID != 1 {
		t.Fatalf("listed: %+v", list.Entries)
	}
}
