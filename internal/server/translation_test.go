package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"readroom/internal/store"
)

// An article is translated on request, into the language the server was given:
// asking queues it, a client takes it from the queue in pieces and sends the
// pieces back translated, and the article page then opens in the translation.
func TestTranslation(t *testing.T) {
	a, site := newApp(t, func(s *Server) { s.Language = "pl" }), origin(t)
	var feed store.Feed
	a.call("POST", "/api/v1/feeds", map[string]any{"url": site.URL + "/feed.xml", "keep_days": 0, "unread": "all"}, &feed)

	type queue struct {
		Entries []struct {
			ID           int64    `json:"id"`
			Language     string   `json:"language"`
			LanguageName string   `json:"language_name"`
			Of           string   `json:"of"`
			Title        string   `json:"title"`
			Pieces       []string `json:"pieces"`
		} `json:"entries"`
		Remaining int    `json:"remaining"`
		Language  string `json:"language"`
	}
	pending := func() queue {
		var q queue
		a.call("GET", "/api/v1/translations/pending", nil, &q)
		return q
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

	// Nothing is translated unless asked for; the article offers it.
	if q := pending(); len(q.Entries) != 0 || q.Language != "pl" {
		t.Fatalf("queue before any request: %+v", q)
	}
	if html := page("/entries/1"); !strings.Contains(html, `data-action="request-translation"`) || !strings.Contains(html, "into Polish") {
		t.Error("article does not offer a translation")
	}

	var e store.Entry
	if resp := a.call("POST", "/api/v1/entries/1/translation", nil, &e); resp.StatusCode != http.StatusAccepted || e.Translation != store.TranslationQueued {
		t.Fatalf("request: status %d, translation %q", resp.StatusCode, e.Translation)
	}
	q := pending()
	if len(q.Entries) != 1 || q.Entries[0].Language != "pl" || q.Entries[0].LanguageName != "Polish" ||
		q.Entries[0].Title != "Hello & welcome" || len(q.Entries[0].Pieces) != 1 || q.Entries[0].Pieces[0] != "Short excerpt." {
		t.Fatalf("queue after a request: %+v", q)
	}
	if html := page("/entries/1"); !strings.Contains(html, `id="translation" data-state="queued"`) || strings.Contains(html, `data-action="request-translation"`) {
		t.Error("article does not show the translation as in progress")
	}

	// A client that cannot translate it says why; asking again puts it back.
	a.call("POST", "/api/v1/entries/1/translation/error", map[string]string{"error": "model went away"}, &e)
	if e.Translation != store.TranslationFailed || e.TranslationError != "model went away" || len(pending().Entries) != 0 {
		t.Fatalf("after a failure: %q, %q", e.Translation, e.TranslationError)
	}
	a.call("POST", "/api/v1/entries/1/translation", nil, &e)
	if e.Translation != store.TranslationQueued {
		t.Fatalf("asked again after a failure: %q", e.Translation)
	}

	// Pieces must be the ones handed out: as many, and of the same text.
	put := func(body map[string]any) int {
		return a.call("PUT", "/api/v1/entries/1/translation", body, nil).StatusCode
	}
	of := q.Entries[0].Of
	if code := put(map[string]any{"of": of, "pieces": []string{"a", "b"}}); code != http.StatusUnprocessableEntity {
		t.Errorf("wrong number of pieces: status %d", code)
	}
	if code := put(map[string]any{"of": "stale", "pieces": []string{"a"}}); code != http.StatusConflict {
		t.Errorf("pieces of another text: status %d", code)
	}
	if code := put(map[string]any{"of": of, "pieces": []string{" "}}); code != http.StatusBadRequest {
		t.Errorf("empty pieces: status %d", code)
	}
	// What comes back is no more trusted than a feed.
	if code := put(map[string]any{"of": of, "model": "test-model", "title": "Witaj <b>serdecznie</b>",
		"pieces": []string{`Krótki fragment.<script>alert(1)</script>`}}); code != http.StatusOK {
		t.Fatalf("translation: status %d", code)
	}
	var got store.Translation
	a.call("GET", "/api/v1/entries/1/translation", nil, &got)
	if got.Language != "pl" || got.Model != "test-model" || got.Title != "Witaj serdecznie" || got.Content != "<p>Krótki fragment.</p>" {
		t.Fatalf("stored translation: %+v", got)
	}
	a.call("GET", "/api/v1/entries/1", nil, &e)
	if e.Translation != store.TranslationReady || e.TranslationLanguage != "pl" || len(pending().Entries) != 0 {
		t.Fatalf("after the translation: %+v", e)
	}

	// The article opens in the translation, and still has its original.
	html := page("/entries/1")
	if !strings.Contains(html, `<div class="content" lang="pl"><p>Krótki fragment.</p></div>`) || !strings.Contains(html, "Witaj serdecznie</h1>") ||
		!strings.Contains(html, "Translated into Polish by test-model") {
		t.Error("article does not open in its translation")
	}
	html = page("/entries/1?original=1")
	if !strings.Contains(html, `<div class="content"><p>Short excerpt.</p></div>`) || !strings.Contains(html, "Show translation") {
		t.Error("the original is not shown when asked for")
	}

	// A fuller copy of the article replaces its text, so the translation of the old text goes.
	a.call("POST", "/api/v1/entries/2/translation", nil, nil)
	second := pending().Entries[0]
	if code := a.call("PUT", "/api/v1/entries/2/translation", map[string]any{"of": second.Of, "pieces": []string{"Stary fragment."}}, nil).StatusCode; code != http.StatusOK {
		t.Fatalf("translation of the second article: status %d", code)
	}
	a.call("POST", "/api/v1/entries/2/preserve", nil, nil)
	if kept := a.waitPreserved(2); kept.Translation != "" {
		t.Errorf("after a local copy was kept: translation %q", kept.Translation)
	}

	// Asking again leaves a finished translation alone; removing it brings the offer back.
	if resp := a.call("POST", "/api/v1/entries/1/translation", nil, &e); resp.StatusCode != http.StatusOK || e.Translation != store.TranslationReady {
		t.Errorf("asked again for a finished translation: status %d, %q", resp.StatusCode, e.Translation)
	}
	if resp := a.call("DELETE", "/api/v1/entries/1/translation", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete: status %d", resp.StatusCode)
	}
	if html := page("/entries/1"); !strings.Contains(html, `data-action="request-translation"`) || !strings.Contains(html, "Short excerpt.") {
		t.Error("article does not go back to its original once the translation is removed")
	}
}

// Without a language the feature is not there at all.
func TestTranslationIsOffWithoutALanguage(t *testing.T) {
	a, site := newApp(t), origin(t)
	var feed store.Feed
	a.call("POST", "/api/v1/feeds", map[string]any{"url": site.URL + "/feed.xml", "keep_days": 0, "unread": "all"}, &feed)
	if resp := a.call("POST", "/api/v1/entries/1/translation", nil, nil); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("request without a language: status %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", a.url+"/entries/1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "request-translation") {
		t.Error("article offers a translation with no language set")
	}
}
