package server

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"readroom/internal/export"
	"readroom/internal/fetch"
	"readroom/internal/htmlx"
	"readroom/internal/store"
)

// --- feeds ------------------------------------------------------------------

func (s *Server) apiFeeds(w http.ResponseWriter, r *http.Request) {
	feeds, err := s.Store.Feeds(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, feeds)
}

func (s *Server) apiFeed(w http.ResponseWriter, r *http.Request) {
	feed, err := s.Store.Feed(r.Context(), pathID(r, "id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, feed)
}

func validFeedURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw // "example.com/blog" is how people give an address
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return raw, true
}

// addFeedError says in plain words why an address cannot be subscribed to.
func addFeedError(err error) string {
	var status *fetch.StatusError
	switch {
	case errors.Is(err, fetch.ErrNoFeed):
		return "No feed found at that address or linked from it."
	case errors.As(err, &status):
		return fmt.Sprintf("That address answered with an error (HTTP %d).", status.Code)
	}
	return "Could not reach that address: " + err.Error()
}

func (s *Server) apiCreateFeed(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL           string `json:"url"`
		Title         string `json:"title"`
		Category      string `json:"category"`
		Preserve      bool   `json:"preserve"`
		PreserveSince string `json:"preserve_since"`
		AISummary     bool   `json:"ai_summary"`
		KeepDays      *int   `json:"keep_days"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	keep := store.DefaultKeepDays
	if in.KeepDays != nil {
		if keep = *in.KeepDays; !validKeepDays(keep) {
			writeError(w, http.StatusBadRequest, keepDaysRule)
			return
		}
	}
	feedURL, ok := validFeedURL(in.URL)
	if !ok {
		writeError(w, http.StatusBadRequest, "url must be an http(s) feed URL")
		return
	}
	since, err := parseSince(in.PreserveSince)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Nothing is saved until the address proves to be a feed, or to lead to one.
	feedURL, err = s.Fetcher.Probe(r.Context(), feedURL)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, addFeedError(err))
		return
	}
	feed, created, err := s.Store.CreateFeed(r.Context(), &store.Feed{
		URL: feedURL, CustomTitle: strings.TrimSpace(in.Title), Category: strings.TrimSpace(in.Category),
		Preserve: in.Preserve, PreserveSince: since, AISummary: in.AISummary, KeepDays: keep,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, feed)
		return
	}
	// Fetch right away so the caller sees a title and entries (or the error).
	s.Fetcher.Refresh(r.Context(), feed)
	if fresh, err := s.Store.Feed(r.Context(), feed.ID); err == nil {
		feed = fresh
	}
	writeJSON(w, http.StatusCreated, feed)
}

func (s *Server) apiUpdateFeed(w http.ResponseWriter, r *http.Request) {
	var in map[string]json.RawMessage
	if !readJSON(w, r, &in) {
		return
	}
	var p store.FeedPatch
	bad := func(field string) { writeError(w, http.StatusBadRequest, "invalid value for "+field) }
	for field, raw := range in {
		switch field {
		case "url":
			var v string
			if json.Unmarshal(raw, &v) != nil {
				bad(field)
				return
			}
			u, ok := validFeedURL(v)
			if !ok {
				bad(field)
				return
			}
			// A corrected address is checked like a new one, and may be a page that leads to the feed.
			if current, err := s.Store.Feed(r.Context(), pathID(r, "id")); err == nil && current.URL != u {
				if u, err = s.Fetcher.Probe(r.Context(), u); err != nil {
					writeError(w, http.StatusUnprocessableEntity, addFeedError(err))
					return
				}
			}
			p.URL = &u
		case "title":
			var v string
			if json.Unmarshal(raw, &v) != nil {
				bad(field)
				return
			}
			p.Title = &v
		case "category":
			var v string
			if json.Unmarshal(raw, &v) != nil {
				bad(field)
				return
			}
			p.Category = &v
		case "preserve":
			var v bool
			if json.Unmarshal(raw, &v) != nil {
				bad(field)
				return
			}
			p.Preserve = &v
		case "ai_summary":
			var v bool
			if json.Unmarshal(raw, &v) != nil {
				bad(field)
				return
			}
			p.AISummary = &v
		case "keep_days":
			var v int
			if json.Unmarshal(raw, &v) != nil || !validKeepDays(v) {
				writeError(w, http.StatusBadRequest, keepDaysRule)
				return
			}
			p.KeepDays = &v
		case "preserve_since":
			var v *string
			if json.Unmarshal(raw, &v) != nil {
				bad(field)
				return
			}
			var since *string
			if v != nil {
				var err error
				if since, err = parseSince(*v); err != nil {
					writeError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
			p.PreserveSince = &since
		default:
			writeError(w, http.StatusBadRequest, "unknown field "+field)
			return
		}
	}
	id := pathID(r, "id")
	if err := s.Store.UpdateFeed(r.Context(), id, p); err != nil {
		s.fail(w, err)
		return
	}
	if p.Preserve != nil || p.PreserveSince != nil || p.URL != nil {
		s.Fetcher.Kick() // new settings may make existing articles due for preservation
	}
	if p.KeepDays != nil {
		// A shorter period applies at once rather than at the next hourly sweep.
		if _, err := s.Store.PruneExpired(r.Context()); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.apiFeed(w, r)
}

const keepDaysRule = "keep_days must be a number of days from 1 to 36500, or 0 to keep articles forever"

func validKeepDays(n int) bool { return n >= 0 && n <= 36500 }

func (s *Server) apiDeleteFeed(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteFeed(r.Context(), pathID(r, "id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiRefreshFeed(w http.ResponseWriter, r *http.Request) {
	feed, err := s.Store.Feed(r.Context(), pathID(r, "id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	// Force a full fetch rather than a conditional one.
	feed.ETag, feed.LastModified = "", ""
	if err := s.Fetcher.Refresh(r.Context(), feed); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.apiFeed(w, r)
}

func (s *Server) apiCategories(w http.ResponseWriter, r *http.Request) {
	cats, err := s.Store.Categories(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cats)
}

// --- entries ----------------------------------------------------------------

func boolParam(q url.Values, name string) *bool {
	switch strings.ToLower(q.Get(name)) {
	case "true", "1", "yes":
		v := true
		return &v
	case "false", "0", "no":
		v := false
		return &v
	}
	return nil
}

// entryFilter builds a filter from query parameters shared by the API and the web UI.
func entryFilter(q url.Values) store.EntryFilter {
	f := store.EntryFilter{
		Read: boolParam(q, "read"), Starred: boolParam(q, "starred"), Preserved: boolParam(q, "preserved"),
		Query: q.Get("q"), Before: q.Get("before"), Oldest: q.Get("order") == "asc",
	}
	f.FeedID, _ = strconv.ParseInt(q.Get("feed_id"), 10, 64)
	f.ReadlistID, _ = strconv.ParseInt(q.Get("readlist_id"), 10, 64)
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if q.Has("category") {
		c := q.Get("category")
		f.Category = &c
	}
	switch q.Get("summary") {
	case "missing":
		v := false
		f.HasSummary = &v
	case "present":
		v := true
		f.HasSummary = &v
	}
	return f
}

func cursor(entries []*store.Entry, limit int) string {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if len(entries) < limit {
		return ""
	}
	last := entries[len(entries)-1]
	return last.PublishedAt + "," + strconv.FormatInt(last.ID, 10)
}

func (s *Server) apiEntries(w http.ResponseWriter, r *http.Request) {
	f := entryFilter(r.URL.Query())
	entries, err := s.Store.Entries(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "next": cursor(entries, f.Limit)})
}

func (s *Server) apiEntry(w http.ResponseWriter, r *http.Request) {
	e, err := s.Store.Entry(r.Context(), pathID(r, "id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) apiUpdateEntry(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Read         *bool   `json:"read"`
		Starred      *bool   `json:"starred"`
		Summary      *string `json:"summary"`
		SummaryModel *string `json:"summary_model"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	id := pathID(r, "id")
	if _, err := s.Store.Entry(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Store.UpdateEntry(r.Context(), id, store.EntryPatch{
		Read: in.Read, Starred: in.Starred, Summary: in.Summary, SummaryModel: in.SummaryModel,
	}); err != nil {
		s.fail(w, err)
		return
	}
	s.apiEntry(w, r)
}

// apiMarkRead marks unread entries as read: all of them, or those of a feed or
// category, optionally only one entry and everything older ("through"). It
// returns the ids it changed so the change can be undone with mark-unread.
func (s *Server) apiMarkRead(w http.ResponseWriter, r *http.Request) {
	var in struct {
		FeedID   int64   `json:"feed_id"`
		Category *string `json:"category"`
		Through  int64   `json:"through"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &in) {
		return
	}
	if in.Through != 0 {
		if _, err := s.Store.Entry(r.Context(), in.Through); err != nil {
			s.fail(w, err)
			return
		}
	}
	ids, err := s.Store.MarkRead(r.Context(), store.ReadScope{FeedID: in.FeedID, Category: in.Category, Through: in.Through})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"marked": len(ids), "ids": ids})
}

func (s *Server) apiMarkUnread(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	n, err := s.Store.MarkUnread(r.Context(), in.IDs)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"marked": n})
}

func (s *Server) apiPreserveEntry(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.RequestPreserve(r.Context(), pathID(r, "id")); err != nil {
		s.fail(w, err)
		return
	}
	s.Fetcher.Kick()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// --- AI summaries -----------------------------------------------------------

// pendingSummary is one unit of work for a summarising script or agent: all it
// needs to prompt a model, with no HTML to parse and no second request.
type pendingSummary struct {
	ID          int64  `json:"id"`
	FeedID      int64  `json:"feed_id"`
	FeedTitle   string `json:"feed_title"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	PublishedAt string `json:"published_at"`
	TextLength  int    `json:"text_length"` // of the whole article, before truncation
	Text        string `json:"text"`        // plain text, at most max_chars characters
	Truncated   bool   `json:"truncated"`
}

func (s *Server) apiPendingSummaries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	maxChars, _ := strconv.Atoi(q.Get("max_chars"))
	if maxChars <= 0 {
		maxChars = 24000
	}
	feedID, _ := strconv.ParseInt(q.Get("feed_id"), 10, 64)
	var exclude []int64
	for _, part := range strings.Split(q.Get("exclude"), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && len(exclude) < 500 {
			exclude = append(exclude, id)
		}
	}
	entries, total, err := s.Store.PendingSummaries(r.Context(), s.SummaryMinChars, feedID, exclude, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]pendingSummary, 0, len(entries))
	for _, e := range entries {
		text := []rune(htmlx.Text(e.Body()))
		item := pendingSummary{ID: e.ID, FeedID: e.FeedID, FeedTitle: e.FeedTitle, Title: e.Title, URL: e.URL,
			PublishedAt: e.PublishedAt, TextLength: e.TextLength}
		if len(text) > maxChars {
			text, item.Truncated = text[:maxChars], true
		}
		item.Text = string(text)
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "remaining": total, "min_chars": s.SummaryMinChars})
}

func (s *Server) apiSetSummary(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Summary string `json:"summary"`
		Model   string `json:"model"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Summary) == "" {
		writeError(w, http.StatusBadRequest, "summary is required; use DELETE to remove one")
		return
	}
	if err := s.Store.UpdateEntry(r.Context(), pathID(r, "id"),
		store.EntryPatch{Summary: &in.Summary, SummaryModel: &in.Model}); err != nil {
		s.fail(w, err)
		return
	}
	s.apiEntry(w, r)
}

// apiClearSummary removes a summary, which puts the entry back in the queue.
func (s *Server) apiClearSummary(w http.ResponseWriter, r *http.Request) {
	empty := ""
	if err := s.Store.UpdateEntry(r.Context(), pathID(r, "id"),
		store.EntryPatch{Summary: &empty, SummaryModel: &empty}); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var unsafeName = regexp.MustCompile(`[^\p{L}\p{N} ._-]+`)

func fileName(title, ext string) string {
	name := strings.TrimSpace(unsafeName.ReplaceAllString(title, " "))
	name = strings.Join(strings.Fields(name), " ")
	if len(name) > 80 {
		name = strings.TrimSpace(name[:80])
	}
	if name == "" {
		name = "readroom"
	}
	return name + ext
}

func attachment(w http.ResponseWriter, ctype, name string) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="export%s"; filename*=UTF-8''%s`, filepath.Ext(name), url.PathEscape(name)))
}

func (s *Server) writeExport(w http.ResponseWriter, r *http.Request, title string, entries []*store.Entry) {
	switch format := r.URL.Query().Get("format"); format {
	case "epub", "":
		var buf bytes.Buffer
		if err := export.EPUB(r.Context(), &buf, title, entries, s.Store.Asset); err != nil {
			s.fail(w, err)
			return
		}
		attachment(w, "application/epub+zip", fileName(title, ".epub"))
		w.Write(buf.Bytes())
	case "md", "markdown":
		md, err := export.MarkdownAll(title, entries, baseURL(r))
		if err != nil {
			s.fail(w, err)
			return
		}
		attachment(w, "text/markdown; charset=utf-8", fileName(title, ".md"))
		io.WriteString(w, md)
	default:
		writeError(w, http.StatusBadRequest, "format must be epub or md")
	}
}

func (s *Server) apiExportEntry(w http.ResponseWriter, r *http.Request) {
	e, err := s.Store.Entry(r.Context(), pathID(r, "id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeExport(w, r, e.Title, []*store.Entry{e})
}

// --- readlists --------------------------------------------------------------

func (s *Server) apiReadlists(w http.ResponseWriter, r *http.Request) {
	lists, err := s.Store.Readlists(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lists)
}

func (s *Server) apiReadlist(w http.ResponseWriter, r *http.Request) {
	l, err := s.Store.Readlist(r.Context(), pathID(r, "id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) apiCreateReadlist(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	l, err := s.Store.CreateReadlist(r.Context(), in.Name)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

func (s *Server) apiRenameReadlist(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.Store.RenameReadlist(r.Context(), pathID(r, "id"), in.Name); err != nil {
		s.fail(w, err)
		return
	}
	s.apiReadlist(w, r)
}

func (s *Server) apiDeleteReadlist(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteReadlist(r.Context(), pathID(r, "id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiReadlistAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := s.Store.Readlist(ctx, pathID(r, "id")); err != nil {
		s.fail(w, err)
		return
	}
	if _, err := s.Store.Entry(ctx, pathID(r, "entry")); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Store.AddToReadlist(ctx, pathID(r, "id"), pathID(r, "entry")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiReadlistRemove(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.RemoveFromReadlist(r.Context(), pathID(r, "id"), pathID(r, "entry")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiExportReadlist(w http.ResponseWriter, r *http.Request) {
	l, err := s.Store.Readlist(r.Context(), pathID(r, "id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	entries, err := s.Store.ReadlistEntries(r.Context(), l.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeExport(w, r, l.Name, entries)
}

// --- whole-collection export / import ---------------------------------------

func (s *Server) apiExportDB(w http.ResponseWriter, r *http.Request) {
	dir, err := os.MkdirTemp("", "readroom-export")
	if err != nil {
		s.fail(w, err)
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "snapshot.db")
	if err := s.Store.Snapshot(r.Context(), path); err != nil {
		s.fail(w, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	name := "readroom-" + time.Now().Format("2006-01-02") + ".db"
	attachment(w, "application/vnd.sqlite3", name)
	http.ServeContent(w, r, "", time.Now(), f)
}

type opml struct {
	XMLName xml.Name      `xml:"opml"`
	Version string        `xml:"version,attr"`
	Title   string        `xml:"head>title"`
	Body    []opmlOutline `xml:"body>outline"`
}

type opmlOutline struct {
	Text     string        `xml:"text,attr"`
	Title    string        `xml:"title,attr,omitempty"`
	Type     string        `xml:"type,attr,omitempty"`
	XMLURL   string        `xml:"xmlUrl,attr,omitempty"`
	HTMLURL  string        `xml:"htmlUrl,attr,omitempty"`
	Outlines []opmlOutline `xml:"outline"`
}

func (s *Server) apiExportOPML(w http.ResponseWriter, r *http.Request) {
	feeds, err := s.Store.Feeds(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	doc := opml{Version: "2.0", Title: "readroom"}
	groups := map[string]int{}
	for _, f := range feeds {
		o := opmlOutline{Text: f.Title, Title: f.Title, Type: "rss", XMLURL: f.URL, HTMLURL: f.SiteURL}
		if f.Category == "" {
			doc.Body = append(doc.Body, o)
			continue
		}
		i, ok := groups[f.Category]
		if !ok {
			doc.Body = append(doc.Body, opmlOutline{Text: f.Category, Title: f.Category})
			i = len(doc.Body) - 1
			groups[f.Category] = i
		}
		doc.Body[i].Outlines = append(doc.Body[i].Outlines, o)
	}
	attachment(w, "text/x-opml; charset=utf-8", "readroom.opml")
	io.WriteString(w, xml.Header)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	enc.Encode(doc)
}

// apiImportOPML accepts an OPML document as the raw body or a multipart "file" field.
func (s *Server) apiImportOPML(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	var src io.Reader = r.Body
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		file, _, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, "missing file field")
			return
		}
		defer file.Close()
		src = file
	}
	var doc opml
	dec := xml.NewDecoder(src)
	dec.Strict = false
	if err := dec.Decode(&doc); err != nil {
		writeError(w, http.StatusBadRequest, "invalid OPML: "+err.Error())
		return
	}
	added, skipped := 0, 0
	var walk func(outlines []opmlOutline, category string)
	walk = func(outlines []opmlOutline, category string) {
		for _, o := range outlines {
			if feedURL, ok := validFeedURL(o.XMLURL); ok {
				_, created, err := s.Store.CreateFeed(r.Context(), &store.Feed{URL: feedURL, Category: category, KeepDays: store.DefaultKeepDays})
				if err == nil && created {
					added++
				} else {
					skipped++
				}
				continue
			}
			name := o.Text
			if name == "" {
				name = o.Title
			}
			walk(o.Outlines, name)
		}
	}
	walk(doc.Body, "")
	if added > 0 {
		s.Fetcher.Kick()
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": added, "skipped": skipped})
}
