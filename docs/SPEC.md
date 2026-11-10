# Readroom — specification

What Readroom is, how it behaves, and why it is built this way. This document describes the state of
the code as of 2026-11-10 (commit `d2ec66b`); the README remains the quick start and API reference.

## 1. Purpose

A personal, self-hosted RSS/Atom reader for one person.

Goals, in the owner's words at the start of the project:

- self-hosted on a small NAS (QNAP Container Station) or Proxmox
- very light
- manages only the owner's feed collection — no social features, no discovery, no accounts
- a web UI for phone, tablet and desktop, used as an installed web app
- a REST API, so an external script or agent can write article summaries (local model or Claude),
  mark articles read, star them, organise them into readlists
- preservation: an article stays readable even after the original disappears
- export to EPUB or Markdown for reading on a reMarkable
- the whole collection exportable as one file

Non-goals: multiple users, a hosted service, calling an LLM from the server, recommendation or
ranking, a native mobile app.

Why build it: Folo has no self-hosted backend and cannot reach a local model; Miniflux is the closest
existing reader but needs Postgres and has no readlists, EPUB export or single-file backup.

## 2. Architecture

One static Go binary and one SQLite file.

| Part | Choice | Reason |
|---|---|---|
| Language | Go, `CGO_ENABLED=0` | cross-compiles to the NAS's arm64 with no toolchain on the target; tens of MB of RAM |
| Storage | SQLite (WAL) via `modernc.org/sqlite` | pure Go, no separate database container |
| HTTP | standard library `net/http` | nothing more is needed |
| Feeds | `gofeed` | RSS, Atom and JSON Feed |
| Article extraction | `go-readability` (Readeck fork) | pulls the readable text out of a page |
| Sanitising | `bluemonday` UGC policy | feed and page HTML is untrusted |
| Markdown | `html-to-markdown` | |
| EPUB | written in-house (`internal/export`) | full control over valid XHTML and embedding images from the database |
| UI | server-rendered `html/template` + a small vanilla script | no build step, no vendored framework; the script is a client of the same REST API |

Packages: `cmd/readroom` (entry point: `serve`, `summarize`), `internal/store` (schema, queries),
`internal/fetch` (polling, preservation), `internal/export` (EPUB, Markdown), `internal/htmlx` (HTML
helpers, excerpts), `internal/server` (API, web app, embedded templates and static files).

**Everything is embedded.** Templates, styles, scripts, the reading font and the app icons are compiled
into the binary. Pages load nothing from another host; a Content-Security-Policy header makes the
browser enforce it and a test checks it. The one exception is images inside articles that were not
preserved, which still point at their original sites.

## 3. Data model

SQLite, schema version in `PRAGMA user_version`, forward-only migrations applied at start-up.

- **feeds** — `url` (unique), `site_url`, `feed_title` (from the feed), `custom_title` (the owner's
  rename; empty = use the feed's), `category` (free text), `preserve`, `preserve_since`, `ai_summary`,
  HTTP validators (`etag`, `last_modified`), `last_fetched_at`, `last_error`.
- **entries** — `feed_id`, `guid` (unique per feed), `url`, `title`, `author`, `published_at`,
  `content` (sanitised HTML from the feed), `preserved_content` + `preserved_at` (the local full copy),
  preservation bookkeeping (`preserve_requested`, `preserve_attempts`, `preserve_error`), `excerpt`,
  `image` (thumbnail of a picture-only entry), `text_length`, `summary` + `summary_model` (written by a
  model through the API), `read`, `starred`.
- **assets** / **entry_assets** — preserved images as blobs, keyed by SHA-256, linked to the entries
  that use them.
- **readlists** / **readlist_entries** — named, ordered-by-added collections of entries.

Entries are never deleted when they drop out of a feed. Deleting a feed deletes its entries and any
images no other entry uses. Timestamps are UTC text (`2006-01-02T15:04:05Z`), sortable as strings.

## 4. Behaviour

### Feeds

- Added by feed URL (no autodiscovery from a site's home page yet). A new feed is fetched at once.
- Each feed is polled every `READROOM_INTERVAL` (default 30 minutes) using conditional requests; a
  background pass runs every minute and handles whatever is due.
- Per feed, the owner can set: **name**, **category**, **keep local copies** with an optional
  **since** date, and **AI summary**.
- OPML import and export; categories map to OPML folders.

### Preservation ("keep local copies")

- When on, each article published on or after the *since* date (or every article if no date) gets a
  local copy: the readable text plus its images, stored in the database. Changing the setting back-fills
  existing articles that now qualify.
- Any single article can also be preserved on request, whatever the feed's setting.
- Feeds that already publish full text are cleaner than a scraped page, so the page only replaces the
  feed's content when it has clearly more text (over 1.3×).
- Limits: page 10 MB, image 8 MB, 60 images per article, 3 attempts before giving up (the error is
  kept and shown).
- Preserved images are served from `/assets/<sha256>`.

### Excerpts

Every entry has a short `excerpt` shown under the title in lists:

1. the feed's own teaser, when it publishes a separate short description (up to 500 characters);
2. otherwise the first real paragraph of the text, up to 280 characters, cut at a sentence end —
   skipping a repeated title and the short label-like lines that often open an article (site name,
   byline, date);
3. for an entry that is only an image (under 200 characters of text), the image's caption (`title`,
   else `alt`), plus a thumbnail in the list.

The paragraph rule is a heuristic and can misjudge unusually structured feeds.

### AI summaries

- The server never calls a model. A summary is written from outside, through the API.
- A summary always takes precedence over the excerpt wherever one exists, and is marked with an AI
  badge (in the list and in the article, with the model's name).
- **Queue:** `GET /api/v1/summaries/pending` returns the articles that need a summary — the feed has
  *AI summary* on, there is no summary yet, and the text is at least `READROOM_SUMMARY_MIN_CHARS`
  (default 1500) long. If the feed also keeps local copies, an article is queued only once its full
  copy is stored. Each item carries plain text ready for a prompt (no HTML), truncated to `max_chars`.
- `PUT /api/v1/entries/{id}/summary` stores `{summary, model}`; `DELETE` removes it and re-queues.
- The queue has no locking or leasing: one summariser at a time.
- `readroom summarize` is a reference client: any OpenAI-compatible server (tested with a local
  `qwen3.6-fast` through llama-server) or the `claude` CLI with Haiku, tools disabled so text inside an
  article cannot make it act.

### Reading, organising, searching

- Views: Unread, All, Starred; by feed, by category, by readlist. Opening an article marks it read;
  it can be marked unread again. "Mark all as read" applies to the current feed or category.
- Readlists: create, rename, delete; add or remove articles.
- Search matches the title, the AI summary and the excerpt (substring, not full text).

### Export

- **EPUB 3** for one article or a whole readlist (one chapter per article, with the summary, source
  link and embedded preserved images; WebP is converted to PNG; images that were never downloaded are
  left out).
- **Markdown** with YAML front matter, for one article or a readlist.
- **Whole collection:** `GET /api/v1/export/db` streams a consistent snapshot of the SQLite file —
  feeds, articles, images and readlists in one file.

## 5. Web app

- Installable (manifest, icons); bottom tab bar on phones, sidebar from 900 px up.
- **Themes:** light (warm paper), sepia and a soft dark (dark grey, cream text). The default follows
  the system. Colours are deliberately low-contrast with a muted accent — the first version (pure
  black, saturated orange) was hard to read on a phone.
- **Display settings**, Kindle-style, from the "Aa" button in an article or "Display" in the menu:
  theme, font (Literata, classic serif, sans), text size 14–28 px, line spacing, page width,
  alignment. They are stored in the browser (`localStorage`), so each device keeps its own.
- Reading font: Literata (SIL Open Font License), bundled with Latin Extended for Polish.
- The interface is in English.
- No service worker: the app needs the server to be reachable; there is no offline mode in the
  browser.

## 6. API and configuration

The REST API lives under `/api/v1`; the README lists every endpoint. Authentication is one shared
token: `Authorization: Bearer <token>` for the API, a cookie set by the login page for the web app
(about 400 days). With no token configured, access is open.

| Variable | Default | |
|---|---|---|
| `READROOM_ADDR` | `:8090` | listen address |
| `READROOM_DB` | `./readroom.db` (`/data/readroom.db` in the image) | database path |
| `READROOM_TOKEN` | none | access token |
| `READROOM_TOKEN_FILE` | none | read the token from a file; a random one is generated there on first run |
| `READROOM_INTERVAL` | `30m` | polling interval |
| `READROOM_SUMMARY_MIN_CHARS` | `1500` | shortest article queued for an AI summary |
| `READROOM_LOG` | `info` | `debug` logs every request |

## 7. Deployment

- **Image:** multi-stage build to a distroless static image, about 32 MB, around 16 MB of RAM idle.
- **Current installation:** QNAP TS-233 (arm64), Container Station application `readroom` — the app
  plus its own Tailscale node, reachable only inside the tailnet at
  `https://readroom.tail264521.ts.net`. No port is published on the NAS. Data lives on the NAS share
  (`/share/Container/readroom`), which is covered by the volume's snapshots.
- **Token on the QNAP:** set directly as `READROOM_TOKEN` in the application's YAML and kept in the
  owner's password manager. The token-file option is not used there, because a file written by the
  container is root-only on the share and File Station cannot open it. The repository's
  `deploy/qnap-compose.yaml` carries a `CHANGE_ME` placeholder; the real value is never committed.
- **Updates are manual:** `make image-qnap` → copy the archive to a NAS share → Container Station
  Images → Import → recreate the application. Container Station only imports the classic
  `docker save` layout, hence the conversion step in that target. Data survives updates; migrations
  run forward only, so going back to an older image after a schema change means restoring the
  database from a snapshot.
- Development needs only Docker: every `make` target runs the Go toolchain in a container.

## 8. Decisions (2026-11-10)

| Decision | Why |
|---|---|
| Build Readroom instead of using Folo or Miniflux | Folo cannot be self-hosted or use a local model; Miniflux lacks readlists, EPUB and single-file backup and needs Postgres |
| Go, SQLite, one binary | light enough for a 2 GB ARM NAS; one container, one file to back up |
| Images stored as blobs in the same database | the collection really is one file; revisit only if images grow to many gigabytes |
| Server never calls a model; summaries come through the API | the reader stays independent of which model is used and where it runs |
| AI summaries are opt-in per feed, with a length threshold | the model works only where it adds something; short notes and comics are skipped |
| A dedicated queue endpoint that returns ready plain text | a script or agent needs one request and no HTML parsing |
| One summariser at a time, no leasing | simplest thing that works for one person; add leasing if several workers are ever needed |
| Excerpt from the feed's teaser, else the first real paragraph, else the image caption | every article shows something useful without waiting for a model |
| Name "Readroom" | a plain noun for the thing, in the spirit of ONCE product names; "feedkeep" was the working name |
| Vanilla script instead of htmx | the UI stays a thin client of the public API, with nothing vendored |
| Everything embedded, enforced by CSP | the app must work on the local network with no internet |
| Display settings stored per device, not on the server | the phone and the desktop want different type and theme |
| Tailscale-only access on the NAS | no public IP at home; HTTPS is needed to install the web app on a phone; mirrors the existing password-vault setup |
| Token set in the YAML on the QNAP rather than a readable file | the owner preferred keeping the generated file owner-only over loosening its permissions |
| Manual updates for the first month | changes are batched during testing; a registry adds moving parts not yet needed |
| Testing phase: preview locally, deploy and commit only on request | the NAS keeps a stable version while the UI is being tried out |

## 9. Known limitations and open questions

- The NAS runs the first deployed version; everything after commit `65722c6` has only run locally.
- **Local network without internet:** the NAS installation is reachable only through its Tailscale
  address, and whether that keeps working during an internet outage has not been tested. Publishing
  port 8090 on the NAS as well would settle it.
- The summariser is run by hand; no timer or cron job is set up.
- EPUB output is checked structurally (well-formed XML, correct container) but has not been opened on
  a reMarkable.
- Thumbnails and article images of entries without a local copy load from the original site.
- Search is substring matching over title, summary and excerpt — no full-text search of article bodies.
- No off-site backup of the NAS data yet.

## 10. Later

- Feed autodiscovery from a site's home page.
- Full-text search (SQLite FTS5).
- Polish interface.
- Automated multi-arch images in a registry with versioned tags, so the NAS updates by pulling and
  other people can try it; that also needs a licence and a decision on making the repository public.
- Leasing in the summary queue, if more than one summariser runs.
- Before the repository goes public: move the installation specifics (tailnet address, NAS paths)
  out of this document and `CLAUDE.md`.
