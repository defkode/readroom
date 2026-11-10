package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"readroom/internal/fetch"
	"readroom/internal/store"
)

// A 1x1 PNG, served as the article's image.
var pixel = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0,
	0x1f, 0x15, 0xc4, 0x89, 0, 0, 0, 0x0d, 'I', 'D', 'A', 'T', 0x78, 0x9c, 0x62, 0, 1, 0, 0, 5, 0, 1, 0x0d, 0x0a, 0x2d, 0xb4,
	0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82}

// origin is a fake website: a feed with one excerpt-only item, the full article, and an image.
func origin(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>Origin Blog</title><link>%[1]s/</link>
<item><title>Hello &amp; welcome</title><link>%[1]s/hello</link><guid>hello-1</guid>
<pubDate>Mon, 02 Nov 2026 10:00:00 GMT</pubDate>
<description>&lt;p&gt;Short excerpt.&lt;/p&gt;&lt;script&gt;alert(1)&lt;/script&gt;</description></item>
<item><title>Old post</title><link>%[1]s/old</link><guid>old-1</guid>
<pubDate>Mon, 05 Jan 2026 10:00:00 GMT</pubDate><description>Old excerpt.</description></item>
</channel></rss>`, srv.URL)
	})
	article := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head><title>Hello</title></head><body><nav>Menu</nav><article><h1>Hello</h1>`+
			strings.Repeat(`<p>This is the full text of the article, which is much longer than the feed's excerpt.</p>`, 12)+
			`<p><img src="/pixel.png" alt="A pixel"><br>Line &amp; break</p></article></body></html>`)
	}
	mux.HandleFunc("/hello", article)
	mux.HandleFunc("/old", article)
	mux.HandleFunc("/pixel.png", func(w http.ResponseWriter, r *http.Request) { w.Write(pixel) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type app struct {
	t   *testing.T
	url string
}

func newApp(t *testing.T) *app {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(st, fetch.New(st, time.Hour, log), "secret", log)
	server.SummaryMinChars = 600 // the test article is about 1,100 characters
	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)
	return &app{t: t, url: srv.URL}
}

// call makes an authenticated API request and decodes the JSON reply into out.
func (a *app) call(method, path string, body, out any) *http.Response {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.url+path, rd)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			a.t.Fatalf("%s %s: status %d, body %q: %v", method, path, resp.StatusCode, data, err)
		}
	}
	return resp
}

func (a *app) waitPreserved(id int64) store.Entry {
	a.t.Helper()
	var e store.Entry
	for i := 0; i < 100; i++ {
		a.call("GET", fmt.Sprintf("/api/v1/entries/%d", id), nil, &e)
		if e.Preserved {
			return e
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.t.Fatalf("entry %d was never preserved (error: %q)", id, e.PreserveError)
	return e
}

func TestAuthRequired(t *testing.T) {
	a := newApp(t)
	resp, _ := http.Get(a.url + "/api/v1/feeds")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("API without token: got %d, want 401", resp.StatusCode)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ = noFollow.Get(a.url + "/")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("web without cookie: got %d to %q, want redirect to /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestFeedLifecycle(t *testing.T) {
	a, site := newApp(t), origin(t)

	// Adding a feed fetches it immediately.
	var feed store.Feed
	resp := a.call("POST", "/api/v1/feeds", map[string]any{"url": site.URL + "/feed.xml", "category": "Tech", "keep_days": 0}, &feed)
	if resp.StatusCode != http.StatusCreated || feed.Title != "Origin Blog" || feed.Total != 2 || feed.Category != "Tech" {
		t.Fatalf("create feed: status %d, feed %+v", resp.StatusCode, feed)
	}

	// Rename, recategorise, and keep copies only of articles since October.
	a.call("PATCH", "/api/v1/feeds/1", map[string]any{
		"title": "My Blog", "category": "Reading", "preserve": true, "preserve_since": "2026-10-01"}, &feed)
	if feed.Title != "My Blog" || feed.FeedTitle != "Origin Blog" || feed.Category != "Reading" ||
		!feed.Preserve || feed.PreserveSince == nil || *feed.PreserveSince != "2026-10-01T00:00:00Z" {
		t.Fatalf("update feed: %+v", feed)
	}
	// An empty title falls back to the feed's own.
	a.call("PATCH", "/api/v1/feeds/1", map[string]any{"title": ""}, &feed)
	if feed.Title != "Origin Blog" {
		t.Fatalf("reset title: got %q", feed.Title)
	}
	if resp := a.call("PATCH", "/api/v1/feeds/1", map[string]any{"preserve_since": "soon"}, nil); resp.StatusCode != 400 {
		t.Fatalf("bad preserve_since: got %d, want 400", resp.StatusCode)
	}

	var list struct{ Entries []store.Entry }
	a.call("GET", "/api/v1/entries?read=false", nil, &list)
	if len(list.Entries) != 2 || list.Entries[0].Title != "Hello & welcome" {
		t.Fatalf("entries: %+v", list.Entries)
	}
	newID, oldID := list.Entries[0].ID, list.Entries[1].ID

	// The new article is preserved from the full page with its image; the script is gone.
	e := a.waitPreserved(newID)
	body := *e.PreservedContent
	if !strings.Contains(body, "full text of the article") || !strings.Contains(body, `src="/assets/`) ||
		strings.Contains(body, "Menu") || strings.Contains(e.Content, "alert") {
		t.Fatalf("preserved content wrong:\nfeed: %s\npreserved: %s", e.Content, body)
	}
	// The January article predates preserve_since, so it is left alone…
	var old store.Entry
	a.call("GET", fmt.Sprintf("/api/v1/entries/%d", oldID), nil, &old)
	if old.Preserved {
		t.Fatal("article older than preserve_since was preserved")
	}
	// …until it is requested explicitly.
	a.call("POST", fmt.Sprintf("/api/v1/entries/%d/preserve", oldID), nil, nil)
	a.waitPreserved(oldID)

	// The image is served from the database.
	sha := body[strings.Index(body, "/assets/")+8:][:64]
	req, _ := http.NewRequest("GET", a.url+"/assets/"+sha, nil)
	req.Header.Set("Authorization", "Bearer secret")
	img, _ := http.DefaultClient.Do(req)
	if got, _ := io.ReadAll(img.Body); !bytes.Equal(got, pixel) || img.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("asset: %d bytes, type %q", len(got), img.Header.Get("Content-Type"))
	}

	// Excerpts: the feed's teaser where it has a short one.
	if e.Excerpt != "Short excerpt." || old.Excerpt != "Old excerpt." {
		t.Fatalf("excerpts: %q and %q", e.Excerpt, old.Excerpt)
	}
	if e.TextLength < 900 {
		t.Fatalf("text_length after preservation: %d", e.TextLength)
	}

	// The summary queue is empty until the feed opts in.
	var queue struct {
		Entries   []pendingSummary
		Remaining int
	}
	a.call("GET", "/api/v1/summaries/pending", nil, &queue)
	if len(queue.Entries) != 0 || queue.Remaining != 0 {
		t.Fatalf("queue before opting in: %+v", queue)
	}
	a.call("PATCH", "/api/v1/feeds/1", map[string]any{"ai_summary": true}, &feed)
	if !feed.AISummary {
		t.Fatal("ai_summary not stored")
	}
	// Both articles are preserved and long enough now; newest first, plain text, truncated on request.
	a.call("GET", "/api/v1/summaries/pending?max_chars=200", nil, &queue)
	if len(queue.Entries) != 2 || queue.Remaining != 2 || queue.Entries[0].ID != newID {
		t.Fatalf("queue: %+v", queue)
	}
	first := queue.Entries[0]
	if !first.Truncated || len([]rune(first.Text)) != 200 || strings.Contains(first.Text, "<") ||
		!strings.Contains(first.Text, "full text of the article") || first.Title != "Hello & welcome" {
		t.Fatalf("queue item: %+v", first)
	}
	// A client stepping through one at a time can skip an article it could not summarise.
	a.call("GET", fmt.Sprintf("/api/v1/summaries/pending?limit=1&exclude=%d", newID), nil, &queue)
	if len(queue.Entries) != 1 || queue.Entries[0].ID != oldID || queue.Remaining != 1 {
		t.Fatalf("queue with exclude: %+v", queue)
	}
	// Writing a summary takes the article out of the queue; deleting it puts it back.
	if resp := a.call("PUT", fmt.Sprintf("/api/v1/entries/%d/summary", oldID),
		map[string]any{"summary": "Old news.", "model": "test"}, nil); resp.StatusCode != 200 {
		t.Fatalf("put summary: %d", resp.StatusCode)
	}
	if resp := a.call("PUT", fmt.Sprintf("/api/v1/entries/%d/summary", oldID), map[string]any{"summary": " "}, nil); resp.StatusCode != 400 {
		t.Fatalf("empty summary: got %d, want 400", resp.StatusCode)
	}
	a.call("GET", "/api/v1/summaries/pending", nil, &queue)
	if len(queue.Entries) != 1 || queue.Entries[0].ID != newID {
		t.Fatalf("queue after one summary: %+v", queue)
	}
	if resp := a.call("DELETE", fmt.Sprintf("/api/v1/entries/%d/summary", oldID), nil, nil); resp.StatusCode != 204 {
		t.Fatalf("delete summary: %d", resp.StatusCode)
	}
	a.call("GET", "/api/v1/summaries/pending?feed_id=1", nil, &queue)
	if queue.Remaining != 2 {
		t.Fatalf("queue after deleting the summary: %+v", queue)
	}

	// Summary, read and starred are set through PATCH.
	a.call("PATCH", fmt.Sprintf("/api/v1/entries/%d", newID),
		map[string]any{"summary": "A greeting.", "summary_model": "test", "read": true, "starred": true}, &e)
	if e.Summary != "A greeting." || !e.Read || !e.Starred {
		t.Fatalf("patch entry: %+v", e)
	}
	// How long a feed keeps its articles is a setting; the test feed keeps them forever.
	var kept store.Feed
	a.call("PATCH", fmt.Sprintf("/api/v1/feeds/%d", feed.ID), map[string]any{"keep_days": 3650}, &kept)
	if kept.KeepDays != 3650 {
		t.Fatalf("keep_days after PATCH: %d", kept.KeepDays)
	}
	if resp := a.call("PATCH", fmt.Sprintf("/api/v1/feeds/%d", feed.ID), map[string]any{"keep_days": -1}, nil); resp.StatusCode != 400 {
		t.Fatalf("negative keep_days: got %d, want 400", resp.StatusCode)
	}
	a.call("PATCH", fmt.Sprintf("/api/v1/feeds/%d", feed.ID), map[string]any{"keep_days": 0}, &kept)

	// Marking "this and older" touches only unread articles from that one down, and can be undone.
	var marked struct {
		Marked int     `json:"marked"`
		IDs    []int64 `json:"ids"`
	}
	a.call("POST", "/api/v1/entries/mark-read", map[string]any{"through": oldID}, &marked)
	if marked.Marked != 1 || len(marked.IDs) != 1 || marked.IDs[0] != oldID {
		t.Fatalf("mark read through the old entry: %+v", marked)
	}
	if resp := a.call("POST", "/api/v1/entries/mark-read", map[string]any{"through": 999}, nil); resp.StatusCode != 404 {
		t.Fatalf("mark read through a missing entry: got %d, want 404", resp.StatusCode)
	}
	a.call("POST", "/api/v1/entries/mark-unread", map[string]any{"ids": marked.IDs}, &marked)
	if marked.Marked != 1 {
		t.Fatalf("undo mark read: %+v", marked)
	}
	a.call("GET", "/api/v1/entries?summary=missing", nil, &list)
	if len(list.Entries) != 1 || list.Entries[0].ID != oldID {
		t.Fatalf("summary=missing: %+v", list.Entries)
	}

	// Readlist and its exports.
	var rl store.Readlist
	a.call("POST", "/api/v1/readlists", map[string]any{"name": "Weekend"}, &rl)
	if resp := a.call("PUT", fmt.Sprintf("/api/v1/readlists/%d/entries/%d", rl.ID, newID), nil, nil); resp.StatusCode != 204 {
		t.Fatalf("add to readlist: %d", resp.StatusCode)
	}
	if resp := a.call("PUT", fmt.Sprintf("/api/v1/readlists/%d/entries/999", rl.ID), nil, nil); resp.StatusCode != 404 {
		t.Fatalf("add missing entry: got %d, want 404", resp.StatusCode)
	}
	epub, _ := io.ReadAll(a.call("GET", fmt.Sprintf("/api/v1/readlists/%d/export?format=epub", rl.ID), nil, nil).Body)
	checkEPUB(t, epub)
	md, _ := io.ReadAll(a.call("GET", fmt.Sprintf("/api/v1/readlists/%d/export?format=md", rl.ID), nil, nil).Body)
	if !strings.Contains(string(md), "# Hello & welcome") || !strings.Contains(string(md), "> A greeting.") ||
		!strings.Contains(string(md), "]("+a.url+"/assets/"+sha) {
		t.Fatalf("markdown export:\n%s", md)
	}

	// Whole-collection snapshot opens as a database holding everything.
	db, _ := io.ReadAll(a.call("GET", "/api/v1/export/db", nil, nil).Body)
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(snap, db, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(snap)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got, _ := st.Entries(context.Background(), store.EntryFilter{}); len(got) != 2 {
		t.Fatalf("snapshot has %d entries, want 2", len(got))
	}
	if _, err := st.Asset(context.Background(), sha); err != nil {
		t.Fatalf("snapshot is missing the image: %v", err)
	}

	// Every page renders.
	for _, path := range []string{"/", "/?view=all", "/?view=starred", "/?feed_id=1", "/?category=Reading", "/?q=hello",
		fmt.Sprintf("/?readlist_id=%d", rl.ID), "/browse", "/feeds", "/readlists", fmt.Sprintf("/entries/%d", newID),
		"/manifest.webmanifest", "/icon/192.png", "/static/app.css", "/login"} {
		req, _ := http.NewRequest("GET", a.url+path, nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret"})
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if path == "/login" { // already signed in
			if resp.StatusCode != http.StatusSeeOther {
				t.Errorf("GET %s: got %d, want redirect", path, resp.StatusCode)
			}
			continue
		}
		// The list marks model-written summaries and falls back to the feed's teaser.
		if path == "/?view=all" && (!bytes.Contains(page, []byte(`class="ai"`)) || !bytes.Contains(page, []byte("A greeting.")) ||
			!bytes.Contains(page, []byte("Old excerpt."))) {
			t.Errorf("GET %s: expected an AI-marked summary and a plain excerpt", path)
		}
		// Only a readlist offers to remove its rows.
		if inList := strings.Contains(path, "readlist_id"); bytes.Contains(page, []byte(`data-action="remove-from-list"`)) != inList {
			t.Errorf("GET %s: remove-from-readlist button present = %v, want %v", path, !inList, inList)
		}
		if resp.StatusCode != 200 || len(page) == 0 || bytes.Contains(page, []byte("template error")) {
			t.Errorf("GET %s: status %d, %d bytes", path, resp.StatusCode, len(page))
		}
		// Pages must be self-contained: nothing loaded from another host, enforced by CSP.
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "font-src 'self'") {
				t.Errorf("GET %s: missing content security policy (%q)", path, csp)
			}
			if m := externalAsset.Find(page); m != nil {
				t.Errorf("GET %s: loads an external asset: %s", path, m)
			}
		}
	}

	// Deleting the feed removes its articles and their images.
	if resp := a.call("DELETE", "/api/v1/feeds/1", nil, nil); resp.StatusCode != 204 {
		t.Fatalf("delete feed: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", a.url+"/assets/"+sha, nil)
	req.Header.Set("Authorization", "Bearer secret")
	if gone, _ := http.DefaultClient.Do(req); gone.StatusCode != 404 {
		t.Fatalf("asset after feed delete: got %d, want 404", gone.StatusCode)
	}
}

func TestOPMLRoundTrip(t *testing.T) {
	a := newApp(t)
	doc := `<opml version="2.0"><body><outline text="News"><outline text="A" xmlUrl="http://127.0.0.1:1/a.xml"/></outline>
<outline text="B" xmlUrl="http://127.0.0.1:1/b.xml"/></body></opml>`
	req, _ := http.NewRequest("POST", a.url+"/api/v1/import/opml", strings.NewReader(doc))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("import: %v %v", resp, err)
	}
	var feeds []store.Feed
	a.call("GET", "/api/v1/feeds", nil, &feeds)
	if len(feeds) != 2 || feeds[0].Category != "" || feeds[1].Category != "News" {
		t.Fatalf("imported feeds: %+v", feeds)
	}
	out, _ := io.ReadAll(a.call("GET", "/api/v1/export/opml", nil, nil).Body)
	var parsed opml
	if err := xml.Unmarshal(out, &parsed); err != nil || len(parsed.Body) != 2 {
		t.Fatalf("exported OPML: %v\n%s", err, out)
	}
}

// externalAsset matches a stylesheet, script or font pulled from another host.
var externalAsset = regexp.MustCompile(`<(?:link|script)[^>]+(?:href|src)="(?:https?:)?//[^"]*"|@import|url\((?:https?:)?//`)

// checkEPUB verifies the container layout and that every XML part is well-formed.
func checkEPUB(t *testing.T, data []byte) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("epub is not a zip: %v", err)
	}
	if zr.File[0].Name != "mimetype" || zr.File[0].Method != zip.Store {
		t.Fatal("epub: mimetype must be the first entry and stored uncompressed")
	}
	images, chapters := 0, 0
	for _, f := range zr.File {
		rc, _ := f.Open()
		body, _ := io.ReadAll(rc)
		rc.Close()
		switch {
		case strings.HasPrefix(f.Name, "OEBPS/img/"):
			images++
		case strings.HasSuffix(f.Name, ".xhtml"), strings.HasSuffix(f.Name, ".opf"), strings.HasSuffix(f.Name, ".xml"):
			dec := xml.NewDecoder(bytes.NewReader(body))
			for {
				if _, err := dec.Token(); err == io.EOF {
					break
				} else if err != nil {
					t.Fatalf("epub: %s is not well-formed XML: %v\n%s", f.Name, err, body)
				}
			}
			if strings.HasPrefix(f.Name, "OEBPS/ch") {
				chapters++
				if !bytes.Contains(body, []byte(`src="img/`)) || !bytes.Contains(body, []byte("A greeting.")) {
					t.Fatalf("epub chapter lacks embedded image or summary:\n%s", body)
				}
			}
		}
	}
	if images != 1 || chapters != 1 {
		t.Fatalf("epub: %d images and %d chapters, want 1 and 1", images, chapters)
	}
}
