// Package server exposes the REST API and the web app.
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"readroom/internal/fetch"
	"readroom/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const cookieName = "readroom_token"

type Server struct {
	Store   *store.Store
	Fetcher *fetch.Fetcher
	Token   string // empty disables authentication
	Log     *slog.Logger

	// SummaryMinChars is the shortest article text worth an AI summary.
	SummaryMinChars int

	pages map[string]*template.Template
}

func New(st *store.Store, f *fetch.Fetcher, token string, log *slog.Logger) *Server {
	s := &Server{Store: st, Fetcher: f, Token: token, Log: log, SummaryMinChars: 1500}
	s.loadTemplates()
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public: app shell assets and login.
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	mux.HandleFunc("GET /manifest.webmanifest", s.manifest)
	mux.HandleFunc("GET /icon/{file}", s.icon)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })

	// REST API.
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/feeds", s.apiFeeds)
	api.HandleFunc("POST /api/v1/feeds", s.apiCreateFeed)
	api.HandleFunc("GET /api/v1/feeds/{id}", s.apiFeed)
	api.HandleFunc("PATCH /api/v1/feeds/{id}", s.apiUpdateFeed)
	api.HandleFunc("DELETE /api/v1/feeds/{id}", s.apiDeleteFeed)
	api.HandleFunc("POST /api/v1/feeds/{id}/refresh", s.apiRefreshFeed)
	api.HandleFunc("GET /api/v1/categories", s.apiCategories)

	api.HandleFunc("GET /api/v1/entries", s.apiEntries)
	api.HandleFunc("POST /api/v1/entries/mark-read", s.apiMarkRead)
	api.HandleFunc("POST /api/v1/entries/mark-unread", s.apiMarkUnread)
	api.HandleFunc("GET /api/v1/entries/{id}", s.apiEntry)
	api.HandleFunc("PATCH /api/v1/entries/{id}", s.apiUpdateEntry)
	api.HandleFunc("POST /api/v1/entries/{id}/preserve", s.apiPreserveEntry)
	api.HandleFunc("GET /api/v1/entries/{id}/export", s.apiExportEntry)
	api.HandleFunc("PUT /api/v1/entries/{id}/summary", s.apiSetSummary)
	api.HandleFunc("DELETE /api/v1/entries/{id}/summary", s.apiClearSummary)
	api.HandleFunc("GET /api/v1/summaries/pending", s.apiPendingSummaries)

	api.HandleFunc("GET /api/v1/readlists", s.apiReadlists)
	api.HandleFunc("POST /api/v1/readlists", s.apiCreateReadlist)
	api.HandleFunc("GET /api/v1/readlists/{id}", s.apiReadlist)
	api.HandleFunc("PATCH /api/v1/readlists/{id}", s.apiRenameReadlist)
	api.HandleFunc("DELETE /api/v1/readlists/{id}", s.apiDeleteReadlist)
	api.HandleFunc("PUT /api/v1/readlists/{id}/entries/{entry}", s.apiReadlistAdd)
	api.HandleFunc("DELETE /api/v1/readlists/{id}/entries/{entry}", s.apiReadlistRemove)
	api.HandleFunc("GET /api/v1/readlists/{id}/export", s.apiExportReadlist)

	api.HandleFunc("GET /api/v1/export/db", s.apiExportDB)
	api.HandleFunc("GET /api/v1/export/opml", s.apiExportOPML)
	api.HandleFunc("POST /api/v1/import/opml", s.apiImportOPML)
	mux.Handle("/api/", s.auth(api, false))

	// Web app.
	web := http.NewServeMux()
	web.HandleFunc("GET /{$}", s.pageEntries)
	web.HandleFunc("GET /browse", s.pageBrowse)
	web.HandleFunc("GET /entries/{id}", s.pageEntry)
	web.HandleFunc("GET /feeds", s.pageFeeds)
	web.HandleFunc("GET /readlists", s.pageReadlists)
	web.HandleFunc("GET /assets/{sha}", s.asset)
	web.HandleFunc("POST /logout", s.logout)
	mux.Handle("/", s.auth(web, true))

	return s.logRequests(compress(mux))
}

// assetVersion changes whenever any embedded static file does. Pages link the
// stylesheet and scripts with it in the address (see the "static" template
// function), so a browser keeps them for good and still picks up a new release
// at once. Without it every page view fetched them again, which is felt on the NAS.
var assetVersion = func() string {
	h := sha256.New()
	fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := staticFS.ReadFile(path)
			h.Write([]byte(path))
			h.Write(data)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}()

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "fonts/") || r.URL.Query().Get("v") == assetVersion {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// Asked for without the current version: never let it go stale.
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	given := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if given == "" {
		if c, err := r.Cookie(cookieName); err == nil {
			given = c.Value
		}
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(s.Token)) == 1
}

func (s *Server) auth(next http.Handler, web bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authorized(r) {
			next.ServeHTTP(w, r)
			return
		}
		if web && r.Method == http.MethodGet {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="readroom"`)
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if !strings.HasPrefix(r.URL.Path, "/static/") && !strings.HasPrefix(r.URL.Path, "/assets/") {
			s.Log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
		}
	})
}

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// fail maps a store error to an HTTP response.
func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case strings.Contains(err.Error(), "UNIQUE constraint"):
		writeError(w, http.StatusConflict, "already exists")
	default:
		s.Log.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func pathID(r *http.Request, name string) int64 {
	id, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// parseSince accepts "2026-01-31" or a full RFC 3339 time and normalises it.
func parseSince(v string) (*string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, v); err != nil {
			return nil, errors.New("preserve_since must be YYYY-MM-DD or RFC 3339")
		}
	}
	out := t.UTC().Format(store.TimeFormat)
	return &out, nil
}
