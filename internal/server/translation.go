package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"readroom/internal/htmlx"
	"readroom/internal/store"
)

// Translations of articles into the owner's language. As with summaries and
// spoken versions, the server translates nothing itself: it keeps a queue of the
// articles the owner asked for, hands each out in pieces, and puts the pieces
// that come back into the article's own markup. What translates them is the
// client's business.
//
// The whole feature is optional: without a language (READROOM_LANGUAGE) nothing
// in the app offers it and nothing can be queued.

// Languages are the ones an article can be translated into: code, English name
// (what a client tells its model), and the name in the language itself.
var Languages = map[string][2]string{
	"bg": {"Bulgarian", "български"}, "cs": {"Czech", "čeština"}, "da": {"Danish", "dansk"},
	"de": {"German", "Deutsch"}, "el": {"Greek", "ελληνικά"}, "en": {"English", "English"},
	"es": {"Spanish", "español"}, "et": {"Estonian", "eesti"}, "fi": {"Finnish", "suomi"},
	"fr": {"French", "français"}, "hr": {"Croatian", "hrvatski"}, "hu": {"Hungarian", "magyar"},
	"it": {"Italian", "italiano"}, "ja": {"Japanese", "日本語"}, "ko": {"Korean", "한국어"},
	"lt": {"Lithuanian", "lietuvių"}, "lv": {"Latvian", "latviešu"}, "nb": {"Norwegian", "norsk"},
	"nl": {"Dutch", "Nederlands"}, "pl": {"Polish", "polski"}, "pt": {"Portuguese", "português"},
	"ro": {"Romanian", "română"}, "sk": {"Slovak", "slovenčina"}, "sl": {"Slovenian", "slovenščina"},
	"sr": {"Serbian", "srpski"}, "sv": {"Swedish", "svenska"}, "tr": {"Turkish", "Türkçe"},
	"uk": {"Ukrainian", "українська"}, "zh": {"Chinese", "中文"},
}

// LanguageCodes lists the codes of Languages in order, for an error message.
func LanguageCodes() string {
	codes := make([]string, 0, len(Languages))
	for code := range Languages {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return strings.Join(codes, ", ")
}

// languageName is a language's English name, or its code when it is not one of ours.
func languageName(code string) string {
	if names, ok := Languages[code]; ok {
		return names[0]
	}
	return code
}

// pendingTranslation is one unit of work for a translating client.
type pendingTranslation struct {
	ID           int64  `json:"id"`
	FeedTitle    string `json:"feed_title"`
	URL          string `json:"url"`
	Language     string `json:"language"`      // what to translate into: "pl"
	LanguageName string `json:"language_name"` // the same in English: "Polish"
	// Of names the text the pieces were cut from; it is sent back with them, so
	// that pieces of an article that has changed meanwhile are not put into it.
	Of      string   `json:"of"`
	Title   string   `json:"title"`
	Summary string   `json:"summary"` // the AI summary, plain text; empty when there is none
	Pieces  []string `json:"pieces"`  // the article in order: runs of text as HTML with their inline markup
}

// bodyVersion names one state of an article's text.
func bodyVersion(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:6])
}

func (s *Server) apiPendingTranslations(w http.ResponseWriter, r *http.Request) {
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
	entries, total, err := s.Store.PendingTranslations(r.Context(), exclude, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]pendingTranslation, 0, len(entries))
	for _, e := range entries {
		body := e.Body()
		out = append(out, pendingTranslation{ID: e.ID, FeedTitle: e.FeedTitle, URL: e.URL,
			Language: e.Language, LanguageName: languageName(e.Language), Of: bodyVersion(body),
			Title: e.Title, Summary: e.Summary, Pieces: htmlx.Pieces(body)})
	}
	// The server's own language is reported too, so a client can tell "nothing is
	// waiting" from "nothing ever will".
	res := map[string]any{"entries": out, "remaining": total, "language": s.Language}
	if s.Language != "" {
		res["language_name"] = languageName(s.Language)
	}
	writeJSON(w, http.StatusOK, res)
}

// apiRequestTranslation asks for an article to be translated into the owner's
// language. It answers with the article, whose "translation" says where that stands.
func (s *Server) apiRequestTranslation(w http.ResponseWriter, r *http.Request) {
	if s.Language == "" {
		writeError(w, http.StatusUnprocessableEntity, "no language to translate into: start the server with READROOM_LANGUAGE, e.g. READROOM_LANGUAGE=pl")
		return
	}
	id := pathID(r, "id")
	if err := s.Store.RequestTranslation(r.Context(), id, s.Language); err != nil {
		s.fail(w, err)
		return
	}
	e, err := s.Store.Entry(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusAccepted
	if e.Translation == store.TranslationReady {
		status = http.StatusOK
	}
	writeJSON(w, status, e)
}

// apiSetTranslation stores a translation: the pieces the queue handed out, each
// translated, in the same order.
func (s *Server) apiSetTranslation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Of       string   `json:"of"`
		Language string   `json:"language"`
		Model    string   `json:"model"`
		Title    string   `json:"title"`
		Summary  string   `json:"summary"`
		Pieces   []string `json:"pieces"`
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
	body := e.Body()
	if in.Of != "" && in.Of != bodyVersion(body) {
		writeError(w, http.StatusConflict, "the article has changed since these pieces were handed out: take it from the queue again")
		return
	}
	if in.Language = strings.TrimSpace(in.Language); in.Language == "" {
		// What was asked for, else what the server is set to.
		if in.Language, err = s.Store.TranslationLanguage(ctx, id); err != nil {
			s.fail(w, err)
			return
		}
		if in.Language == "" {
			in.Language = s.Language
		}
	}
	if in.Language == "" {
		writeError(w, http.StatusBadRequest, "language is required: nothing says what this article was translated into")
		return
	}
	blank := true
	for _, p := range in.Pieces {
		blank = blank && strings.TrimSpace(p) == ""
	}
	if blank && len(in.Pieces) > 0 {
		writeError(w, http.StatusBadRequest, "every piece is empty; use DELETE to remove a translation")
		return
	}
	content, ok := htmlx.ReplacePieces(body, in.Pieces)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "pieces must hold one translation for each of the "+
			strconv.Itoa(len(htmlx.Pieces(body)))+" pieces the queue handed out, in the same order")
		return
	}
	t := store.Translation{
		Language: in.Language,
		Model:    strings.TrimSpace(in.Model),
		Title:    htmlx.Flat(in.Title), // a title is text: whatever markup came back is dropped
		Summary:  strings.TrimSpace(in.Summary),
		Content:  htmlx.Sanitize(content), // a translator is no more trusted than a feed
	}
	if err := s.Store.SaveTranslation(ctx, id, t); err != nil {
		s.fail(w, err)
		return
	}
	s.apiEntry(w, r)
}

// apiTranslationFailed lets a client say why it could not translate an article,
// so the owner sees that instead of waiting; asking again puts it back in the queue.
func (s *Server) apiTranslationFailed(w http.ResponseWriter, r *http.Request) {
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
	if err := s.Store.TranslationFailed(r.Context(), pathID(r, "id"), in.Error); err != nil {
		s.fail(w, err)
		return
	}
	s.apiEntry(w, r)
}

func (s *Server) apiTranslation(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.Translation(r.Context(), pathID(r, "id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) apiDeleteTranslation(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteTranslation(r.Context(), pathID(r, "id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
