// Package fetch polls feeds and preserves full local copies of articles.
package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/mmcdole/gofeed"
	"golang.org/x/net/html"

	"readroom/internal/htmlx"
	"readroom/internal/store"
)

const (
	userAgent    = "readroom/0.1 (self-hosted feed reader)"
	maxFeedBytes = 20 << 20
	maxPageBytes = 10 << 20
	maxImgBytes  = 8 << 20
	maxImages    = 60
)

type Fetcher struct {
	Store    *store.Store
	Interval time.Duration // how often each feed is polled
	Log      *slog.Logger

	client *http.Client
	mu     sync.Mutex // one refresh/preserve pass at a time
	pruned time.Time  // when expired articles were last deleted
}

func New(st *store.Store, interval time.Duration, log *slog.Logger) *Fetcher {
	return &Fetcher{Store: st, Interval: interval, Log: log, client: &http.Client{Timeout: 45 * time.Second}}
}

// Run polls due feeds and works through the preservation queue until ctx ends.
func (f *Fetcher) Run(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		f.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Pass refreshes every due feed, then preserves a batch of pending articles.
func (f *Fetcher) Pass(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	feeds, err := f.Store.FeedsDue(ctx, f.Interval)
	if err != nil {
		f.Log.Error("list due feeds", "err", err)
		return
	}
	for _, feed := range feeds {
		if ctx.Err() != nil {
			return
		}
		f.refresh(ctx, feed)
	}
	f.preservePending(ctx, 25)
	// Expiry is counted in days, so an hourly sweep is plenty.
	if time.Since(f.pruned) >= time.Hour {
		f.pruned = time.Now()
		if n, err := f.Store.PruneExpired(ctx); err != nil {
			f.Log.Error("delete expired articles", "err", err)
		} else if n > 0 {
			f.Log.Info("deleted expired articles", "count", n)
		}
	}
}

// Refresh fetches one feed now and preserves whatever that makes pending.
func (f *Fetcher) Refresh(ctx context.Context, feed *store.Feed) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.refresh(ctx, feed)
	f.preservePending(ctx, 25)
	return err
}

// Kick runs a pass in the background, e.g. after settings changed.
func (f *Fetcher) Kick() {
	go f.Pass(context.Background())
}

func (f *Fetcher) refresh(ctx context.Context, feed *store.Feed) error {
	title, site, etag, lastMod, items, err := f.fetchFeed(ctx, feed)
	if err != nil {
		f.Log.Warn("fetch feed", "feed", feed.URL, "err", err)
		f.Store.FeedFetched(ctx, feed.ID, "", "", "", "", err)
		return err
	}
	added, err := f.Store.AddEntries(ctx, feed.ID, items)
	if err != nil {
		f.Log.Error("store entries", "feed", feed.URL, "err", err)
		return err
	}
	if added > 0 {
		f.Log.Info("feed updated", "feed", feed.URL, "new", added)
	}
	return f.Store.FeedFetched(ctx, feed.ID, title, site, etag, lastMod, nil)
}

func (f *Fetcher) fetchFeed(ctx context.Context, feed *store.Feed) (title, site, etag, lastMod string, items []store.NewEntry, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feed.URL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/atom+xml, application/rss+xml, application/feed+json, application/xml;q=0.9, */*;q=0.8")
	if feed.ETag != "" {
		req.Header.Set("If-None-Match", feed.ETag)
	}
	if feed.LastModified != "" {
		req.Header.Set("If-Modified-Since", feed.LastModified)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return "", "", feed.ETag, feed.LastModified, nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("HTTP %d", resp.StatusCode)
		return
	}
	parsed, err := gofeed.NewParser().Parse(io.LimitReader(resp.Body, maxFeedBytes))
	if err != nil {
		err = fmt.Errorf("parse feed: %w", err)
		return
	}
	now := time.Now().UTC()
	for _, it := range parsed.Items {
		published := now
		switch {
		case it.PublishedParsed != nil:
			published = it.PublishedParsed.UTC()
		case it.UpdatedParsed != nil:
			published = it.UpdatedParsed.UTC()
		}
		if published.After(now) {
			published = now
		}
		content := it.Content
		if content == "" {
			content = it.Description
		}
		link := resolve(feed.URL, it.Link)
		guid := it.GUID
		if guid == "" {
			guid = link
		}
		if guid == "" {
			sum := sha256.Sum256([]byte(it.Title + content))
			guid = hex.EncodeToString(sum[:16])
		}
		author := ""
		if len(it.Authors) > 0 && it.Authors[0] != nil {
			author = it.Authors[0].Name
		}
		items = append(items, store.NewEntry{
			GUID: guid, URL: link, Title: strings.TrimSpace(it.Title), Author: author,
			PublishedAt: published.Format(store.TimeFormat),
			Content:     htmlx.Sanitize(absolutize(content, link)),
			Excerpt:     teaser(it.Description),
		})
	}
	return strings.TrimSpace(parsed.Title), parsed.Link, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), items, nil
}

// teaserMax is the longest feed description still treated as a teaser rather
// than the article itself.
const teaserMax = 500

// teaser returns the feed's own short description of an item, if it has one.
func teaser(description string) string {
	text := htmlx.Flat(htmlx.Sanitize(description))
	if text == "" || utf8.RuneCountInString(text) > teaserMax {
		return ""
	}
	return text
}

func resolve(base, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	u, err := b.Parse(ref)
	if err != nil {
		return ref
	}
	return u.String()
}

// absolutize rewrites relative link and image URLs in feed HTML against the article URL.
func absolutize(content, base string) string {
	if base == "" || !strings.Contains(content, "<") {
		return content
	}
	nodes, err := htmlx.ParseFragment(content)
	if err != nil {
		return content
	}
	htmlx.Walk(nodes, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		for _, key := range []string{"href", "src"} {
			if v := htmlx.Attr(n, key); v != "" && !strings.HasPrefix(v, "#") {
				htmlx.SetAttr(n, key, resolve(base, v))
			}
		}
	})
	return htmlx.Render(nodes)
}

// --- preservation -----------------------------------------------------------

func (f *Fetcher) preservePending(ctx context.Context, batch int) {
	entries, err := f.Store.PendingPreserve(ctx, batch)
	if err != nil {
		f.Log.Error("list pending preserve", "err", err)
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if err := f.preserve(ctx, e); err != nil {
			f.Log.Warn("preserve", "entry", e.ID, "url", e.URL, "err", err)
			f.Store.PreserveFailed(ctx, e.ID, err)
			continue
		}
		f.Log.Info("preserved", "entry", e.ID, "title", e.Title)
	}
}

// preserve stores the readable article and its images inside the database.
func (f *Fetcher) preserve(ctx context.Context, e *store.Entry) error {
	content := e.Content
	if e.URL != "" {
		// Feeds that publish full text are cleaner than a scraped page, so the page
		// only wins when it has clearly more to offer (i.e. the feed is an excerpt).
		feedLen := len(htmlx.Text(content))
		full, err := f.readable(ctx, e.URL)
		switch {
		case err == nil && len(htmlx.Text(full))*10 > feedLen*13:
			content = full
		case err != nil && feedLen == 0:
			return err
		}
	}
	if strings.TrimSpace(content) == "" {
		return errors.New("nothing to preserve")
	}
	nodes, err := htmlx.ParseFragment(htmlx.Sanitize(content))
	if err != nil {
		return err
	}
	var assets []store.Asset
	seen := map[string]string{}
	htmlx.Walk(nodes, func(n *html.Node) {
		if n.Type != html.ElementNode || n.Data != "img" {
			return
		}
		htmlx.DelAttr(n, "srcset")
		htmlx.DelAttr(n, "sizes")
		src := resolve(e.URL, htmlx.Attr(n, "src"))
		if !strings.HasPrefix(src, "http") {
			return
		}
		sha, ok := seen[src]
		if !ok {
			if len(assets) >= maxImages {
				return
			}
			a, err := f.image(ctx, src)
			if err != nil {
				f.Log.Debug("image", "src", src, "err", err)
				seen[src] = ""
				return
			}
			assets = append(assets, *a)
			sha = a.SHA256
			seen[src] = sha
		}
		if sha != "" {
			htmlx.SetAttr(n, "src", "/assets/"+sha)
			htmlx.SetAttr(n, "data-original-src", src)
		}
	})
	return f.Store.SavePreserved(ctx, e.ID, htmlx.Render(nodes), assets)
}

func (f *Fetcher) get(ctx context.Context, rawURL string, limit int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", errors.New("response too large")
	}
	return data, resp.Header.Get("Content-Type"), nil
}

func (f *Fetcher) readable(ctx context.Context, pageURL string) (string, error) {
	u, err := url.Parse(pageURL)
	if err != nil {
		return "", err
	}
	data, _, err := f.get(ctx, pageURL, maxPageBytes)
	if err != nil {
		return "", err
	}
	article, err := readability.FromReader(bytes.NewReader(data), u)
	if err != nil {
		return "", err
	}
	if article.Node == nil {
		return "", errors.New("no readable content found")
	}
	var b strings.Builder
	if err := article.RenderHTML(&b); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (f *Fetcher) image(ctx context.Context, src string) (*store.Asset, error) {
	data, ctype, err := f.get(ctx, src, maxImgBytes)
	if err != nil {
		return nil, err
	}
	mime := http.DetectContentType(data)
	if strings.Contains(ctype, "svg") || bytes.Contains(data[:min(len(data), 512)], []byte("<svg")) {
		mime = "image/svg+xml"
	}
	if !strings.HasPrefix(mime, "image/") {
		return nil, fmt.Errorf("not an image (%s)", mime)
	}
	sum := sha256.Sum256(data)
	return &store.Asset{SHA256: hex.EncodeToString(sum[:]), MIME: mime, Data: data}, nil
}
