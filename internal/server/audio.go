package server

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"readroom/internal/htmlx"
	"readroom/internal/store"
)

// Spoken versions of articles. As with summaries, the server makes nothing
// itself: it keeps a queue of the articles the owner asked to hear, hands each
// out as plain text, and stores whatever recording comes back. What turns the
// text into speech, and in which format, is the client's business.

// maxAudioBytes bounds one recording: hours of speech at any sensible bitrate.
const maxAudioBytes = 64 << 20

// pendingAudio is one unit of work for a text-to-speech client.
type pendingAudio struct {
	ID          int64  `json:"id"`
	FeedID      int64  `json:"feed_id"`
	FeedTitle   string `json:"feed_title"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	URL         string `json:"url"`
	PublishedAt string `json:"published_at"`
	Text        string `json:"text"` // what to read: plain text, one paragraph per line, at most max_chars characters
	Truncated   bool   `json:"truncated"`
}

func (s *Server) apiPendingAudio(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	maxChars, _ := strconv.Atoi(q.Get("max_chars"))
	if maxChars <= 0 {
		maxChars = 200000 // some three hours of speech
	}
	var exclude []int64
	for _, part := range strings.Split(q.Get("exclude"), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && len(exclude) < 500 {
			exclude = append(exclude, id)
		}
	}
	entries, total, err := s.Store.PendingAudio(r.Context(), exclude, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]pendingAudio, 0, len(entries))
	for _, e := range entries {
		text := []rune(htmlx.Speech(e.Body()))
		item := pendingAudio{ID: e.ID, FeedID: e.FeedID, FeedTitle: e.FeedTitle, Title: e.Title, Author: e.Author,
			URL: e.URL, PublishedAt: e.PublishedAt}
		if len(text) > maxChars {
			text, item.Truncated = text[:maxChars], true
		}
		item.Text = string(text)
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "remaining": total})
}

// apiRequestAudio asks for an article to be read aloud. It answers with the
// article, whose "audio" says where that stands.
func (s *Server) apiRequestAudio(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if err := s.Store.RequestAudio(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	e, err := s.Store.Entry(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusAccepted
	if e.Audio == store.AudioReady {
		status = http.StatusOK
	}
	writeJSON(w, status, e)
}

// apiSetAudio stores a recording: the request body is the audio file itself,
// its Content-Type says what kind, and "voice" names what read it.
func (s *Server) apiSetAudio(w http.ResponseWriter, r *http.Request) {
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(kind, "audio/") {
		writeError(w, http.StatusUnsupportedMediaType, "send the audio file as the body, with an audio/* Content-Type such as audio/mpeg")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAudioBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "a recording may be at most 64 MB")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "could not read the recording: "+err.Error())
		return
	case len(data) == 0:
		writeError(w, http.StatusBadRequest, "the recording is empty; use DELETE to remove one")
		return
	}
	voice := strings.TrimSpace(r.URL.Query().Get("voice"))
	if err := s.Store.SaveAudio(r.Context(), pathID(r, "id"), store.Audio{MIME: kind, Voice: voice, Data: data}); err != nil {
		s.fail(w, err)
		return
	}
	s.apiEntry(w, r)
}

// apiAudioFailed lets a client say why it could not read an article, so the
// owner sees that instead of waiting; asking again puts it back in the queue.
func (s *Server) apiAudioFailed(w http.ResponseWriter, r *http.Request) {
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
	if err := s.Store.AudioFailed(r.Context(), pathID(r, "id"), in.Error); err != nil {
		s.fail(w, err)
		return
	}
	s.apiEntry(w, r)
}

// apiAudio serves the recording, with byte ranges so a player can seek in it.
func (s *Server) apiAudio(w http.ResponseWriter, r *http.Request) {
	a, err := s.Store.Audio(r.Context(), pathID(r, "id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.MIME)
	h.Set("Cache-Control", "private, max-age=86400")
	h.Set("X-Content-Type-Options", "nosniff")
	made, _ := time.Parse(store.TimeFormat, a.CreatedAt)
	http.ServeContent(w, r, "", made, bytes.NewReader(a.Data))
}

func (s *Server) apiDeleteAudio(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteAudio(r.Context(), pathID(r, "id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
