package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// compress gzips text responses (pages, JSON, the stylesheet and scripts) for
// clients that accept it. A list page shrinks to about a sixth, which matters
// on the NAS, where every response also passes through the Tailscale proxy.
func compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

var gzipPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return w
}}

// gzipWriter decides when the headers are written whether the body is worth
// compressing, and from then on writes it through gzip or straight through.
type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
}

func compressible(contentType string) bool {
	t, _, _ := strings.Cut(contentType, ";")
	switch t = strings.TrimSpace(t); {
	case strings.HasPrefix(t, "text/"):
		return true
	case t == "application/json", t == "application/javascript", t == "application/manifest+json",
		t == "application/xml", t == "image/svg+xml":
		return true
	}
	return false
}

func (w *gzipWriter) WriteHeader(status int) {
	if !w.decided {
		w.decided = true
		h := w.Header()
		if status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified &&
			status != http.StatusPartialContent && h.Get("Content-Encoding") == "" && compressible(h.Get("Content-Type")) {
			h.Del("Content-Length")
			h.Del("Accept-Ranges")
			h.Set("Content-Encoding", "gzip")
			h.Add("Vary", "Accept-Encoding")
			w.gz = gzipPool.Get().(*gzip.Writer)
			w.gz.Reset(w.ResponseWriter)
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipWriter) Write(b []byte) (int, error) {
	if !w.decided {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(b))
		}
		w.WriteHeader(http.StatusOK)
	}
	if w.gz != nil {
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *gzipWriter) Flush() {
	if w.gz != nil {
		w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *gzipWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *gzipWriter) close() {
	if w.gz != nil {
		w.gz.Close()
		gzipPool.Put(w.gz)
	}
}
