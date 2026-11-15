package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"readroom/internal/store"
)

// An article is read aloud on request: asking queues it, a client takes the text
// from the queue and uploads a recording in whatever format it makes, and the
// article page plays it.
func TestAudio(t *testing.T) {
	a, site := newApp(t), origin(t)
	var feed store.Feed
	a.call("POST", "/api/v1/feeds", map[string]any{"url": site.URL + "/feed.xml", "keep_days": 0, "unread": "all"}, &feed)

	type queue struct {
		Entries []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"entries"`
		Remaining int `json:"remaining"`
	}
	pending := func() queue {
		var q queue
		a.call("GET", "/api/v1/audio/pending", nil, &q)
		return q
	}
	page := func() string {
		req, _ := http.NewRequest("GET", a.url+"/entries/1", nil)
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	upload := func(ctype string, data []byte) *http.Response {
		req, _ := http.NewRequest("PUT", a.url+"/api/v1/entries/1/audio?voice=test+voice", bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Content-Type", ctype)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	// Nothing is read unless asked for.
	if q := pending(); len(q.Entries) != 0 || q.Remaining != 0 {
		t.Fatalf("queue before any request: %+v", q)
	}
	if strings.Contains(page(), `data-state="queued"`) {
		t.Error("article shows a recording in progress before one was asked for")
	}

	var e store.Entry
	if resp := a.call("POST", "/api/v1/entries/1/audio", nil, &e); resp.StatusCode != http.StatusAccepted || e.Audio != store.AudioQueued {
		t.Fatalf("request: status %d, audio %q", resp.StatusCode, e.Audio)
	}
	if resp := a.call("POST", "/api/v1/entries/999/audio", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("request for a missing article: status %d", resp.StatusCode)
	}
	q := pending()
	if len(q.Entries) != 1 || q.Entries[0].ID != 1 || q.Remaining != 1 || !strings.HasSuffix(q.Entries[0].Text, ".") ||
		strings.Contains(q.Entries[0].Text, "<") {
		t.Fatalf("queue after a request: %+v", q)
	}
	if !strings.Contains(page(), `data-state="queued"`) {
		t.Error("article does not show the recording as in progress")
	}
	if resp := a.call("GET", "/api/v1/entries/1/audio", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("audio before it was made: status %d", resp.StatusCode)
	}

	// A client that cannot read it says why; asking again puts it back.
	a.call("POST", "/api/v1/entries/1/audio/error", map[string]string{"error": "no voice installed"}, &e)
	if e.Audio != store.AudioFailed || e.AudioError != "no voice installed" || len(pending().Entries) != 0 {
		t.Fatalf("after a failure: audio %q, error %q", e.Audio, e.AudioError)
	}
	if !strings.Contains(page(), "no voice installed") {
		t.Error("article does not say why the recording failed")
	}
	e = store.Entry{} // fields left out of the reply would keep their old values
	a.call("POST", "/api/v1/entries/1/audio", nil, &e)
	if e.Audio != store.AudioQueued || e.AudioError != "" || len(pending().Entries) != 1 {
		t.Fatalf("asked again after a failure: audio %q, error %q", e.Audio, e.AudioError)
	}

	// The recording is the request body; only audio is accepted.
	if resp := upload("text/html", []byte("<script>")); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("upload of something that is not audio: status %d", resp.StatusCode)
	}
	if resp := upload("audio/ogg", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty upload: status %d", resp.StatusCode)
	}
	sound := bytes.Repeat([]byte("speech"), 500)
	if resp := upload("audio/ogg; codecs=opus", sound); resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: status %d", resp.StatusCode)
	}
	a.call("GET", "/api/v1/entries/1", nil, &e)
	if e.Audio != store.AudioReady || e.AudioVoice != "test voice" || e.AudioAt == "" || len(pending().Entries) != 0 {
		t.Fatalf("after the upload: %q by %q at %q", e.Audio, e.AudioVoice, e.AudioAt)
	}
	if html := page(); !strings.Contains(html, `<audio controls`) || !strings.Contains(html, "Read by test voice") {
		t.Error("article has no player for its recording")
	}

	// It is served as it came, and in parts, so a player can seek.
	resp := a.call("GET", "/api/v1/entries/1/audio", nil, nil)
	got, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Type") != "audio/ogg" || !bytes.Equal(got, sound) {
		t.Errorf("download: type %q, %d bytes", resp.Header.Get("Content-Type"), len(got))
	}
	req, _ := http.NewRequest("GET", a.url+"/api/v1/entries/1/audio", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Range", "bytes=6-11")
	part, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(part.Body)
	part.Body.Close()
	if part.StatusCode != http.StatusPartialContent || string(got) != "speech" {
		t.Errorf("range request: status %d, body %q", part.StatusCode, got)
	}

	// Asking again leaves a finished recording alone; removing it clears everything.
	if resp := a.call("POST", "/api/v1/entries/1/audio", nil, &e); resp.StatusCode != http.StatusOK || e.Audio != store.AudioReady {
		t.Errorf("request when a recording exists: status %d, audio %q", resp.StatusCode, e.Audio)
	}
	if resp := a.call("DELETE", "/api/v1/entries/1/audio", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete: status %d", resp.StatusCode)
	}
	e = store.Entry{}
	a.call("GET", "/api/v1/entries/1", nil, &e)
	if e.Audio != "" || e.AudioVoice != "" {
		t.Errorf("after deleting: audio %q", e.Audio)
	}

	// A hidden article is not read.
	a.call("POST", "/api/v1/entries/1/audio", nil, nil)
	a.call("PATCH", "/api/v1/entries/1", map[string]any{"hidden": true}, nil)
	if len(pending().Entries) != 0 {
		t.Error("a hidden article is still queued to be read aloud")
	}
}
