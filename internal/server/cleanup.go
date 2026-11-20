package server

import (
	"net/http"
	"strconv"
	"strings"

	"readroom/internal/htmlx"
	"readroom/internal/store"
)

// AI clean-up. For the feeds that have it on, every article is read by a model
// before it is shown: the model says what language it is in and which of its
// blocks are the article. As everywhere else the server calls no model: it hands
// the article out cut into blocks, and a client sends back one judgment per
// block. The server then builds the clean copy itself, from its own blocks — so
// no word of an article ever comes from a model.

// pendingCleanup is one unit of work for a client doing AI clean-up.
type pendingCleanup struct {
	ID        int64  `json:"id"`
	FeedTitle string `json:"feed_title"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	Held      bool   `json:"held"` // nobody can read it until this is done
	// Of names the text the blocks were cut from; it is sent back with the
	// judgments, so that those of an article that has changed are not applied to it.
	Of     string         `json:"of"`
	Blocks []cleanupBlock `json:"blocks"`
}

// cleanupBlock is one block as a judge sees it.
type cleanupBlock struct {
	Kind string `json:"kind"` // h2…h6, text, quote, item, code, image, table, rule
	Text string `json:"text"` // its words, without markup
}

func (s *Server) apiPendingCleanup(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	var exclude []int64
	for _, part := range strings.Split(q.Get("exclude"), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && len(exclude) < 500 {
			exclude = append(exclude, id)
		}
	}
	entries, total, err := s.Store.PendingCleanup(r.Context(), exclude, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]pendingCleanup, 0, len(entries))
	for _, e := range entries {
		source := e.Source()
		item := pendingCleanup{ID: e.ID, FeedTitle: e.FeedTitle, URL: e.URL, Title: e.Title, Author: e.Author,
			Held: e.Held, Of: bodyVersion(source), Blocks: []cleanupBlock{}}
		for _, b := range htmlx.Blocks(source) {
			item.Blocks = append(item.Blocks, cleanupBlock{Kind: b.Label(), Text: b.Text()})
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "remaining": total})
}

// apiSetCleanup takes the judgments on an article's blocks — one role for each,
// in order — and the language it is in, and makes the clean copy from them.
func (s *Server) apiSetCleanup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Of       string   `json:"of"`
		Language string   `json:"language"`
		Model    string   `json:"model"`
		Roles    []string `json:"roles"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	ctx, id := r.Context(), pathID(r, "id")
	e, err := s.Store.Entry(ctx, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	source := e.Source()
	if in.Of != "" && in.Of != bodyVersion(source) {
		writeError(w, http.StatusConflict, "the article has changed since these blocks were handed out: take it from the queue again")
		return
	}
	in.Language = strings.ToLower(strings.TrimSpace(in.Language))
	if len(in.Language) > 8 || strings.ContainsAny(in.Language, " <>\"'") {
		writeError(w, http.StatusBadRequest, `language must be a code such as "en", or empty when it cannot be told`)
		return
	}
	blocks := htmlx.Blocks(source)
	clean, ok := htmlx.Assemble(blocks, in.Roles)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "roles must hold one of keep, drop, h2, h3, text, quote, ref for each of the "+
			strconv.Itoa(len(blocks))+" blocks the queue handed out, in the same order")
		return
	}
	if len(blocks) > 0 && htmlx.Flat(htmlx.WithoutLinks(clean)) == "" && !strings.Contains(clean, "<img") {
		writeError(w, http.StatusUnprocessableEntity, "every block was dropped, which would leave an empty article")
		return
	}
	md, err := htmlx.Markdown(clean)
	if err != nil {
		s.fail(w, err)
		return
	}
	// What is shown is what the Markdown says, so that the Markdown is the copy
	// and not a by-product of it.
	shown, err := htmlx.FromMarkdown(md)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Store.SaveCleanup(ctx, id, store.Cleanup{Language: in.Language, Model: strings.TrimSpace(in.Model), Markdown: md, HTML: shown}); err != nil {
		s.fail(w, err)
		return
	}
	s.apiEntry(w, r)
}

// apiCleanupFailed lets a client say why it could not judge an article. The
// article is then shown as it came.
func (s *Server) apiCleanupFailed(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Error string `json:"error"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Error = strings.TrimSpace(in.Error); in.Error == "" {
		writeError(w, http.StatusBadRequest, "error is required")
		return
	}
	if cut := []rune(in.Error); len(cut) > 500 {
		in.Error = string(cut[:500]) + "…"
	}
	if err := s.Store.CleanupFailed(r.Context(), pathID(r, "id"), in.Error); err != nil {
		s.fail(w, err)
		return
	}
	s.apiEntry(w, r)
}

// apiDeleteCleanup removes the clean copy; the article is shown as it came and,
// if its feed has clean-up on, goes back in the queue.
func (s *Server) apiDeleteCleanup(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteCleanup(r.Context(), pathID(r, "id")); err != nil {
		s.fail(w, err)
		return
	}
	s.apiEntry(w, r)
}
