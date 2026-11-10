package server

import (
	"bytes"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"readroom/internal/store"
)

func (s *Server) loadTemplates() {
	funcs := template.FuncMap{
		"ago":  ago,
		"date": longDate,
		"day":  func(v *string) string { return dayOf(v) },
		"deref": func(v *string) string {
			if v == nil {
				return ""
			}
			return *v
		},
		"readWhen": readWhen,
		"shortDay": shortDay,
		"brief":    briefError,
		"static":   func(name string) string { return "/static/" + name + "?v=" + assetVersion },
		"html":     func(v string) template.HTML { return template.HTML(v) }, // content is sanitised on ingest
		"icon": func(name string) template.HTML {
			return template.HTML(`<svg class="icon" aria-hidden="true"><use href="#i-` + name + `"/></svg>`)
		},
	}
	s.pages = map[string]*template.Template{}
	for _, page := range []string{"entries", "browse", "entry", "feeds", "readlists", "login"} {
		s.pages[page] = template.Must(template.New("layout.html").Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/"+page+".html"))
	}
}

// ago renders a compact relative time such as "5m", "3h" or "12 Mar".
func ago(v string) string {
	t, err := time.Parse(store.TimeFormat, v)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case t.Year() == time.Now().Year():
		return t.Local().Format("2 Jan")
	}
	return t.Local().Format("2 Jan 2006")
}

func longDate(v string) string {
	t, err := time.Parse(store.TimeFormat, v)
	if err != nil {
		return v
	}
	return t.Local().Format("Monday, 2 January 2006 · 15:04")
}

// readWhen says how long ago an article was read, e.g. "Read 3 days ago"; the
// exact time is in the label's tooltip. Articles read before read times were
// recorded have none.
func readWhen(v *string) string {
	if v == nil {
		return "Read"
	}
	t, err := time.Parse(store.TimeFormat, *v)
	if err != nil {
		return "Read"
	}
	ago := func(n int, unit string) string {
		if n == 1 {
			if unit == "hour" {
				return "Read an hour ago"
			}
			return "Read a " + unit + " ago"
		}
		return fmt.Sprintf("Read %d %ss ago", n, unit)
	}
	const day = 24 * time.Hour
	switch d := time.Since(t); {
	case d < time.Minute:
		return "Read just now"
	case d < time.Hour:
		return ago(int(d.Minutes()), "minute")
	case d < day:
		return ago(int(d.Hours()), "hour")
	case d < 2*day:
		return "Read yesterday"
	case d < 14*day:
		return ago(int(d/day), "day")
	case d < 60*day:
		return ago(int(d/(7*day)), "week")
	case d < 365*day:
		return ago(int(d/(30*day)), "month")
	default:
		return ago(int(d/(365*day)), "year")
	}
}

// shortDay renders a stored time as a day, e.g. "3 Nov" (with the year if it is not this one).
func shortDay(v *string) string {
	if v == nil {
		return ""
	}
	t, err := time.Parse(store.TimeFormat, *v)
	if err != nil {
		return ""
	}
	t = t.Local()
	if t.Year() != time.Now().Year() {
		return t.Format("2 Jan 2006")
	}
	return t.Format("2 Jan")
}

// briefError turns a fetch error into a few words fit for a list.
func briefError(msg string) string {
	switch low := strings.ToLower(msg); {
	case strings.HasPrefix(msg, "HTTP "):
		return msg
	case strings.HasPrefix(low, "not a feed"):
		return "no longer a feed"
	case strings.Contains(low, "no such host"):
		return "address not found"
	case strings.Contains(low, "timeout") || strings.Contains(low, "deadline exceeded"):
		return "timed out"
	case strings.Contains(low, "connection refused"):
		return "connection refused"
	case strings.Contains(low, "certificate"):
		return "certificate problem"
	}
	if r := []rune(msg); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return msg
}

func dayOf(v *string) string {
	if v == nil || len(*v) < 10 {
		return ""
	}
	return (*v)[:10]
}

type navGroup struct {
	Category string
	Unread   int
	Feeds    []*store.Feed
}

// shell is the data every page needs to draw the sidebar and tab bar.
type shell struct {
	Title     string
	Tab       string // which tab-bar item is active
	Active    string // which sidebar row is active, e.g. "view:unread", "feed:3"
	Back      string // when set, the top bar shows a back button to this URL
	Auth      bool
	Reader    bool // article view: no title in the top bar, no tab bar
	Unread    int
	Groups    []navGroup
	Broken    []*store.Feed // feeds that have stopped updating, for the notice and the Manage page
	Readlists []*store.Readlist
	Page      any
}

func (s *Server) shell(r *http.Request, title, tab, active string) (*shell, error) {
	feeds, err := s.Store.Feeds(r.Context())
	if err != nil {
		return nil, err
	}
	lists, err := s.Store.Readlists(r.Context())
	if err != nil {
		return nil, err
	}
	sh := &shell{Title: title, Tab: tab, Active: active, Auth: s.Token != "", Readlists: lists}
	for _, f := range feeds {
		sh.Unread += f.Unread
		if f.Broken {
			sh.Broken = append(sh.Broken, f)
		}
		if n := len(sh.Groups); n == 0 || sh.Groups[n-1].Category != f.Category {
			sh.Groups = append(sh.Groups, navGroup{Category: f.Category})
		}
		g := &sh.Groups[len(sh.Groups)-1]
		g.Feeds = append(g.Feeds, f)
		g.Unread += f.Unread
	}
	return sh, nil
}

// pageCSP makes the browser enforce that the app is self-contained: scripts,
// styles and fonts may only come from this server, so it works with no internet.
// Images and media are the one exception, because articles that were not
// preserved still point at their original sites.
const pageCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; font-src 'self'; " +
	"img-src 'self' data: https: http:; media-src 'self' https: http:; frame-src https:; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'"

func (s *Server) render(w http.ResponseWriter, page, block string, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, block, data); err != nil {
		s.Log.Error("render", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", pageCSP)
	w.Write(buf.Bytes())
}

func (s *Server) webFail(w http.ResponseWriter, err error) {
	if err == store.ErrNotFound {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	s.Log.Error("page failed", "err", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// --- pages ------------------------------------------------------------------

type entriesPage struct {
	Entries    []*store.Entry
	More       string // URL of the next page of items, empty at the end
	View       string
	Tabs       []viewTab         // unread / all / starred switcher, keeping the current scope
	Hidden     map[string]string // scope fields the search form must carry along
	Query      string
	FeedID     int64
	Category   *string
	ListID     int64       // set in a readlist, where each row offers removal
	CanMark    bool        // bulk "mark as read" applies here: a feed, a category or everything
	Unread     int64       // how many articles "mark all as read" would mark
	Feed       *store.Feed // set when the list is one feed, whose settings can be opened here
	Notice     bool        // the main Unread list: the place to say that feeds have stopped updating
	HiddenView bool        // the list of hidden articles, whose rows offer to show them again
	ByArrival  bool        // the list is in arrival order, which "this and older" must follow
	Categories []string    // for the feed settings sheet
	Empty      string
}

func (s *Server) pageEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := entryFilter(q)
	f.Limit = 40
	view := q.Get("view")
	title, tab, active := "Unread", "unread", "view:unread"
	scope := url.Values{}
	ctx := r.Context()
	var feed *store.Feed

	switch {
	case f.ReadlistID != 0:
		l, err := s.Store.Readlist(ctx, f.ReadlistID)
		if err != nil {
			s.webFail(w, err)
			return
		}
		title, tab, active = l.Name, "lists", "list:"+strconv.FormatInt(l.ID, 10)
		scope.Set("readlist_id", strconv.FormatInt(l.ID, 10))
		if view == "" {
			view = "all"
		}
	case f.FeedID != 0:
		var err error
		if feed, err = s.Store.Feed(ctx, f.FeedID); err != nil {
			s.webFail(w, err)
			return
		}
		title, tab, active = feed.Title, "browse", "feed:"+strconv.FormatInt(feed.ID, 10)
		scope.Set("feed_id", strconv.FormatInt(feed.ID, 10))
	case f.Category != nil:
		title, tab, active = *f.Category, "browse", "cat:"+*f.Category
		if title == "" {
			title = "Uncategorised"
		}
		scope.Set("category", *f.Category)
	}
	if view == "" {
		view = "unread"
	}
	scoped := len(scope) > 0
	empty := "Nothing here yet."
	switch view {
	case "unread":
		unread := false
		f.Read = &unread
		// What arrived last comes first, so a newly added feed's articles are not
		// buried among older ones; the other views are an archive, by publication.
		f.ByArrival = true
		empty = "You're all caught up."
	case "starred":
		starred := true
		f.Starred = &starred
		empty = "No starred articles."
		if !scoped {
			title, tab, active = "Starred", "starred", "view:starred"
		}
	case "hidden":
		// What the owner put away, across everything; the one place it can be brought back.
		f = store.EntryFilter{Hidden: true, Query: f.Query, Before: f.Before, Limit: f.Limit}
		scope, scoped, feed = url.Values{}, false, nil
		empty = "Nothing is hidden."
		title, tab, active = "Hidden articles", "manage", "hidden"
	default:
		view = "all"
		if !scoped {
			title, tab, active = "All articles", "unread", "view:all"
		}
	}
	if f.Query != "" {
		empty = "No matches."
	}

	entries, err := s.Store.Entries(ctx, f)
	if err != nil {
		s.webFail(w, err)
		return
	}
	page := entriesPage{Entries: entries, View: view, Query: f.Query, Hidden: map[string]string{"view": view},
		FeedID: f.FeedID, Category: f.Category, ListID: f.ReadlistID, Empty: empty, Feed: feed,
		Notice: !scoped && view == "unread" && f.Query == "", HiddenView: view == "hidden", ByArrival: f.ByArrival}
	if feed != nil && q.Get("partial") == "" {
		if page.Categories, err = s.Store.Categories(ctx); err != nil {
			s.webFail(w, err)
			return
		}
	}
	// Bulk marking covers a whole feed, category or everything, so it is not offered
	// where the list shows a narrower selection (a readlist, starred, a search).
	if page.CanMark = f.ReadlistID == 0 && view != "starred" && view != "hidden" && f.Query == ""; page.CanMark && q.Get("partial") == "" {
		if page.Unread, err = s.Store.CountUnread(ctx, store.ReadScope{FeedID: f.FeedID, Category: f.Category}); err != nil {
			s.webFail(w, err)
			return
		}
	}
	for k := range scope {
		page.Hidden[k] = scope.Get(k)
	}
	for _, t := range [][2]string{{"unread", "Unread"}, {"all", "All"}, {"starred", "Starred"}} {
		if view == "hidden" {
			break // not a view of a list: it has no siblings
		}
		link := url.Values{"view": {t[0]}}
		for k, v := range scope {
			link[k] = v
		}
		tab := viewTab{Label: t[1], URL: "/?" + link.Encode(), Active: view == t[0]}
		if q.Get("partial") == "" {
			// Each tab says how many articles it holds here.
			count := store.EntryFilter{FeedID: f.FeedID, Category: f.Category, ReadlistID: f.ReadlistID, Query: f.Query}
			switch t[0] {
			case "unread":
				unread := false
				count.Read = &unread
			case "starred":
				starred := true
				count.Starred = &starred
			}
			if tab.Count, err = s.Store.CountEntries(ctx, count); err != nil {
				s.webFail(w, err)
				return
			}
		}
		page.Tabs = append(page.Tabs, tab)
	}
	if next := cursor(entries, f); next != "" {
		more := url.Values{}
		for k, v := range scope {
			more[k] = v
		}
		more.Set("view", view)
		more.Set("before", next)
		more.Set("partial", "1")
		if f.Query != "" {
			more.Set("q", f.Query)
		}
		page.More = "/?" + more.Encode()
	}
	if q.Get("partial") != "" {
		s.render(w, "entries", "items", page)
		return
	}
	sh, err := s.shell(r, title, tab, active)
	if err != nil {
		s.webFail(w, err)
		return
	}
	if scoped {
		sh.Back = "/browse"
		if f.ReadlistID != 0 {
			sh.Back = "/readlists"
		}
	}
	sh.Page = page
	s.render(w, "entries", "layout.html", sh)
}

func (s *Server) pageBrowse(w http.ResponseWriter, r *http.Request) {
	sh, err := s.shell(r, "Feeds", "browse", "")
	if err != nil {
		s.webFail(w, err)
		return
	}
	s.render(w, "browse", "layout.html", sh)
}

type viewTab struct {
	Label, URL string
	Active     bool
	Count      int64 // articles this view holds in the current scope and search
}

type entryPage struct {
	Entry     *store.Entry
	Lists     []listMembership
	Pending   bool // a local copy was requested and isn't ready yet
	FirstRead bool // this very visit marked it read, so there is no earlier reading to report
	Teaser    bool // the feed sends only teasers and this article has no fuller copy: offer one
	Paragraph []string
}

type listMembership struct {
	*store.Readlist
	Member bool
}

func (s *Server) pageEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := pathID(r, "id")
	e, err := s.Store.Entry(ctx, id)
	if err != nil {
		s.webFail(w, err)
		return
	}
	firstRead := !e.Read
	if firstRead {
		read := true
		s.Store.UpdateEntry(ctx, id, store.EntryPatch{Read: &read})
		now := store.Now()
		e.Read, e.ReadAt = true, &now
	}
	sh, err := s.shell(r, e.Title, "", "")
	if err != nil {
		s.webFail(w, err)
		return
	}
	sh.Back, sh.Reader = "/", true
	if ref, err := url.Parse(r.Referer()); err == nil && ref.Host == r.Host && ref.Path == "/" {
		sh.Back = ref.RequestURI()
	}
	member := map[int64]bool{}
	for _, lid := range e.Readlists {
		member[lid] = true
	}
	page := entryPage{Entry: e, Paragraph: paragraphs(e.Summary), FirstRead: firstRead}
	for _, g := range sh.Groups {
		for _, feed := range g.Feeds {
			if feed.ID == e.FeedID {
				page.Teaser = feed.TeaserOnly && !feed.Preserve && !e.Preserved && e.URL != ""
			}
		}
	}
	for _, l := range sh.Readlists {
		page.Lists = append(page.Lists, listMembership{Readlist: l, Member: member[l.ID]})
	}
	sh.Page = page
	s.render(w, "entry", "layout.html", sh)
}

var blankLines = regexp.MustCompile(`\n\s*\n|\n`)

func paragraphs(text string) []string {
	var out []string
	for _, p := range blankLines.Split(text, -1) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) pageFeeds(w http.ResponseWriter, r *http.Request) {
	sh, err := s.shell(r, "Manage feeds", "manage", "manage")
	if err != nil {
		s.webFail(w, err)
		return
	}
	cats, err := s.Store.Categories(r.Context())
	if err != nil {
		s.webFail(w, err)
		return
	}
	sh.Page = cats
	s.render(w, "feeds", "layout.html", sh)
}

func (s *Server) pageReadlists(w http.ResponseWriter, r *http.Request) {
	sh, err := s.shell(r, "Readlists", "lists", "lists")
	if err != nil {
		s.webFail(w, err)
		return
	}
	s.render(w, "readlists", "layout.html", sh)
}

// asset serves a preserved image straight from the database.
func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	a, err := s.Store.Asset(r.Context(), r.PathValue("sha"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.MIME)
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox") // inert if opened directly
	w.Write(a.Data)
}

// --- login ------------------------------------------------------------------

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.authorized(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login", "layout.html", &shell{Title: "Sign in", Tab: "login", Page: r.URL.Query().Get("error") != ""})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("token")
	r.Header.Set("Authorization", "Bearer "+token)
	if s.Token == "" || !s.authorized(r) {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: baseURL(r)[:5] == "https", MaxAge: 400 * 24 * 3600})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- installable web app ----------------------------------------------------

func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Write([]byte(`{
  "name": "Readroom",
  "short_name": "Readroom",
  "description": "Self-hosted feed reader",
  "start_url": "/",
  "scope": "/",
  "display": "standalone",
  "background_color": "#f1f1f3",
  "theme_color": "#f1f1f3",
  "icons": [
    {"src": "/icon/192.png", "sizes": "192x192", "type": "image/png", "purpose": "any maskable"},
    {"src": "/icon/512.png", "sizes": "512x512", "type": "image/png", "purpose": "any maskable"}
  ]
}`))
}

var iconCache sync.Map // size -> []byte

// icon draws the app icon (a feed glyph on the accent colour) at the requested size.
func (s *Server) icon(w http.ResponseWriter, r *http.Request) {
	size, _ := strconv.Atoi(strings.TrimSuffix(r.PathValue("file"), ".png"))
	if size != 180 && size != 192 && size != 512 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(iconPNG(size))
}

func iconPNG(size int) []byte {
	if b, ok := iconCache.Load(size); ok {
		return b.([]byte)
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	bg := [3]float64{0xe8, 0x59, 0x0c}
	n := float64(size)
	// Glyph geometry in unit coordinates, origin at the glyph's bottom-left corner.
	ox, oy, span := 0.29, 0.71, 0.42
	cover := func(dist, half float64) float64 { // anti-aliased coverage of a stroke/disc edge
		return math.Max(0, math.Min(1, (half-dist)*n+0.5))
	}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			px, py := (float64(x)+0.5)/n-ox, oy-(float64(y)+0.5)/n
			d := math.Hypot(px, py)
			a := cover(math.Hypot(px-0.045, py-0.045), 0.06) // the dot
			if px >= -0.01 && py >= -0.01 {
				a = math.Max(a, cover(math.Abs(d-span*0.55), 0.04))
				a = math.Max(a, cover(math.Abs(d-span), 0.04))
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(bg[0] + (255-bg[0])*a), G: uint8(bg[1] + (255-bg[1])*a), B: uint8(bg[2] + (255-bg[2])*a), A: 255,
			})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	iconCache.Store(size, buf.Bytes())
	return buf.Bytes()
}
