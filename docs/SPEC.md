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
  `keep_days`, `initial_unread` (backlog policy for the first fetch), `teaser` (whether it sends only
  teasers; unknown until checked), HTTP validators (`etag`, `last_modified`), `last_fetched_at`, and
  health (`last_error`, `fail_count`, `failing_since`, `last_ok_at`).
- **entries** — everything about an article except its text: `feed_id`, `guid` (unique per feed),
  `url`, `title`, `author`, `published_at`, `created_at` (when it arrived here), `preserved_at` and
  preservation bookkeeping (`preserve_requested`, `preserve_attempts`, `preserve_error`), `excerpt`,
  `image` (thumbnail of a picture-only entry), `text_length`, `summary` + `summary_model` (written by a
  model through the API), `read` + `read_at`, `starred`, `hidden_at`.
- **entry_bodies** — one row per entry with its HTML: `content` (sanitised, as the feed sent it) and
  `preserved_content` (the local full copy). Kept apart so that lists, counts and searches, which
  read only **entries**, never touch article text.
- **assets** / **entry_assets** — preserved images as blobs, keyed by SHA-256, linked to the entries
  that use them.
- **readlists** / **readlist_entries** — named, ordered-by-added collections of entries.

Entries are not deleted when they drop out of a feed, only when they expire (see Retention). Deleting a
feed deletes its entries and any images no other entry uses. Timestamps are UTC text (`2006-01-02T15:04:05Z`), sortable as strings.

Indexes follow the lists that are shown: by publication date overall and per feed; partial indexes
over unread, visible entries in arrival order (overall and per feed), which also answer the sidebar's
unread counts; a partial index over starred entries; and lookups from an entry to its readlists and
from an image to its entries. After a migration the database is vacuumed and analysed.

## 4. Behaviour

### Feeds

- Added by address. The address is checked before anything is saved: it must be a feed, or a page that
  leads to one (a `<link rel="alternate">`, or a feed at a customary path such as `/feed.xml`), so a
  site's home page or an article is enough. Otherwise the form says why. Changing a feed's address is
  checked the same way; OPML import is not, and what fails there shows up as below.
- Adding a feed says how much of what it already holds starts unread: the last 30 days (the default,
  and never fewer than the 3 newest), the latest 10, everything, or nothing. The rest arrives already
  read — in the archive, not in the way. Imported feeds get the default.
- Each feed is polled every `READROOM_INTERVAL` (default 30 minutes) using conditional requests; a
  background pass runs every minute and handles whatever is due.
- Per feed, the owner can set: **name**, **category**, **keep local copies** with an optional
  **since** date, **keep articles for** (30 days to 2 years, or forever; one year by default), and
  **AI summary**.

### Retention ("keep articles for")

- Each feed keeps its articles for `keep_days` days, counted from publication; 0 keeps them forever.
  Expired articles are deleted hourly, and at once when the setting is saved.
- Never deleted: articles in a readlist, starred articles, and articles with a local copy (made,
  requested, or due under the feed's "keep local copies" setting). Being unread does not protect one.
- A fetch skips items that would be deleted again, so a feed that still lists old articles does not
  bring them back as unread. A newly added feed therefore starts with at most the last `keep_days` days.
- OPML import and export; categories map to OPML folders.

### Feed health

- Each feed records how many fetches in a row have failed, since when, and when one last worked. One
  that works clears it.
- After 3 failures in a row a feed counts as **broken** — at once if the server says it is gone for
  good (HTTP 410). A single timeout changes nothing the reader sees.
- A failing feed is retried less and less often: the interval doubles with each failure, up to once a
  day. "Refresh now" and "Retry" ignore that.
- A permanent redirect (301, 308) updates the stored address by itself.
- Broken feeds are shown where they will be seen: a notice at the top of the main Unread list (it can
  be dismissed per device and returns when the set of broken feeds changes), a "Needs attention"
  section at the top of Manage feeds with Retry, the feed's name in red in the sidebar, and a dot on
  the way to Manage feeds. Nothing is disabled or deleted automatically.
- A feed that answers but has published nothing for 180 days is marked "No new articles since …" in
  Manage feeds: it may have moved or stopped.

### Preservation ("keep local copies")

- When on, each article published on or after the *since* date (or every article if no date) gets a
  local copy: the readable text plus its images, stored in the database. Changing the setting back-fills
  existing articles that now qualify.
- Any single article can also be preserved on request, whatever the feed's setting.
- A local copy can be removed again, from the same toolbar button or row-menu item that made it: the
  saved text and images are deleted and the article goes back to what the feed sent. It asks first,
  because the original may be gone by then. A removed copy is not fetched again under the feed's
  setting — only when asked for.
- **Teaser-only feeds:** once per feed, after a fetch, the pages of its two newest articles are read
  and compared with what the feed sent. If each holds at least three times the text and a screenful
  more, the feed is marked as sending only teasers. Adding such a feed offers to keep local copies;
  its articles carry the same offer under the title ("Get the full article", "Always for this feed");
  and Manage feeds says so. Feeds that already keep local copies are not checked.
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
- The queue has no locking or leasing: one summariser at a time. `exclude` lets a client step past an
  article it could not summarise.
- `readroom summarize` works through the queue one article at a time (fetch, summarise, write back),
  up to `-limit` per run, and does not contact the model when the queue is empty.
- **Schedule:** a systemd user timer (`deploy/systemd/`) checks the queue every 5 minutes on the owner's desktop and runs it against
  the local model. The job needs only `READROOM_TOKEN`, from `~/.config/readroom/env`.
- `readroom summarize` is a reference client: any OpenAI-compatible server (tested with a local
  `qwen3.6-fast` through llama-server) or the `claude` CLI with Haiku, tools disabled so text inside an
  article cannot make it act.

### Reading, organising, searching

- Views: Unread, All, Starred; by feed, by category, by readlist, each tab showing how many articles
  it holds. Unread lists show what arrived in the reader last first (and, within one fetch, the newest
  first), so a newly added feed's articles sit together at the top instead of being scattered among
  older ones; All and Starred are an archive, by publication date. Opening an article marks it read and records when (`read_at`); from the second
  visit on, the article shows how long ago that was under its title, next to "Mark as unread".
  Back on a list, a message says "Marked as read" with Undo for a few seconds — for every article
  first read on the way, when several were stepped through — so a mistaken tap, or the last unread
  article of a feed, can be put back without finding it again.
- Lists and articles show an estimated reading time (about 1,400 characters a minute, from the best
  text available: the local copy if there is one, else what the feed sent, which may be only a teaser).
- A row marks what the owner did to its article with the same filled, accent-coloured icons as the
  article's toolbar, always in the same order: in a readlist, starred, local copy kept.
- Every row has a ⋮ menu: mark as read or unread, add to a readlist, star, keep a local copy, "Mark this and older as read", and in a readlist
  "Remove from readlist". The list's own ⋮ menu has "Mark all N as read" and, for a
  single feed, "Feed settings" (the same sheet as in Manage feeds). Both bulk actions apply to the
  current feed or category (or everything), are not offered in a readlist, in Starred or in search
  results, and can be undone from the message that follows for a few seconds.
- An article ends with links to the previous and next article of the list it was opened from (the
  list as it was shown, further pages included), and going back returns to that list.
- Page changes are animated by the browser (cross-document view transitions): a quick cross-fade, with
  the content sliding forward into an article or the next one and back to the list or the previous one.
  Browsers without them, and reduced-motion settings, simply switch pages.
- Keyboard shortcuts on a list (`j`/`k` select, `Enter` open, `m` read, `s` star, `/` search) and in an
  article (`j`/`k` next and previous, `u` back, `m` unread, `s` star, `v` original); `?` lists them.
- Readlists: create, rename, delete; add or remove articles (from the article or from the row menu).
- **Hiding:** an article can be hidden for good — from its row menu, from the article, or with `h` —
  to keep the collection to what is worth keeping. A hidden article is in no list, search, count,
  readlist, export or summary queue, and a refresh does not bring it back. Nothing else about it
  changes, so "Show again" restores it exactly, star and readlists included. Hiding is followed by an
  Undo; later, hidden articles are listed under Settings → Hidden articles (Manage on a phone). They
  expire with the feed's other articles.
- Search matches the title, the AI summary and the excerpt (substring, not full text).

### Export

- **EPUB 3** for one article or a whole readlist (one chapter per article, with the summary, source
  link and embedded preserved images; WebP is converted to PNG; images that were never downloaded are
  left out).
- **Markdown** with YAML front matter, for one article or a readlist.
- **Whole collection:** `GET /api/v1/export/db` streams a consistent snapshot of the SQLite file —
  feeds, articles, images and readlists in one file.

## 5. Web app

- Installable (manifest, icons); bottom tab bar on phones, sidebar from 900 px up, which can be hidden (remembered per device). In the sidebar, Manage feeds, Manage readlists and Display sit in one Settings menu next to the name, and Sign out at the foot; on a phone they are a tab and a section of the Feeds page.
- **Themes:** light and dark in neutral greys with a blue accent, after macOS, and a warm sepia. The default follows
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
- **Updates are pulled, when the owner chooses:** a push to `main` runs the tests and publishes
  `ghcr.io/defkode/readroom` for arm64 and amd64 (`:latest`, and `:sha-<commit>` to go back to; a
  `v*` tag adds a version tag). On the NAS, recreating the application pulls the newest image —
  nothing updates by itself. While the package is private the NAS pulls with a GitHub token limited
  to `read:packages`. `make image-qnap` remains as the offline route: an archive in the classic
  `docker save` layout, the only one Container Station imports. Data survives updates; migrations
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
| Summariser pulls one article at a time, checking every 5 minutes, from the desktop with the local model | no work is held locally, an interrupted run loses nothing, and the NAS is too small to run a model |
| The scheduled job needs only `READROOM_TOKEN` | one secret in one file; everything else is fixed in the unit |
| One summariser at a time, no leasing | simplest thing that works for one person; add leasing if several workers are ever needed |
| Excerpt from the feed's teaser, else the first real paragraph, else the image caption | every article shows something useful without waiting for a model |
| Bulk "mark as read" is a labelled menu item with Undo, not a bare icon with a confirmation | a lone check mark did not say it meant everything; confirmations get clicked through, undo actually recovers a slip |
| Opening an article still marks it read at once, and the list it leads back to offers Undo | waiting or scrolling before marking would make "read" a guess; the slip is only noticed back on the list, where the row is already gone, so that is where the way back belongs |
| Removing a local copy asks for confirmation instead of offering Undo, and marks the article as out of fetch attempts | undo would mean fetching the page again, which is exactly what may no longer work; and without the mark a feed that keeps local copies would quietly bring it back on the next pass |
| "Mark this and older as read" instead of checkboxes for partial marking | one tap closes the rest of a list once nothing below is of interest; multi-select is fiddly on a phone |
| Next and previous article come from the list as the browser showed it, kept per tab | an article has no order of its own; a server-side "next unread" would skip what was just read and could not tell which list the reader came from |
| Articles expire per feed after a year by default, with "forever" as a choice (after HEY's per-contact recycling) | the database would otherwise only grow; what is worth keeping is starred, put in a readlist or given a local copy, and those never expire |
| Light and dark themes are neutral greys with a blue accent; only sepia is warm | the owner disliked the brown, paper-like palette in both modes and prefers macOS-style greys |
| Stylesheet and scripts are linked by a content version and cached for good; text responses are gzipped | on the NAS every page view re-downloaded 55 KB of assets and an uncompressed 64 KB page through the Tailscale proxy, about half a second per click |
| A feed is reported as broken only after 3 failed fetches in a row, on the Unread list rather than only in Manage feeds | one timeout is not news; and the place the owner looks every day is the only place a silent failure gets noticed |
| Adding a feed validates first and finds the feed from a page address | a mistyped address used to leave a dead feed behind, and guessing `/feed.xml` by hand was the common way to mistype |
| Unwanted articles are hidden, not deleted, and hiding touches nothing else | a deleted article would come back as new on the next fetch; leaving its star and readlists alone makes "Show again" an exact undo |
| Unread is ordered by arrival, not publication, and a new feed brings only its recent articles as unread | in publication order a new feed's backlog was scattered deep in the list and missed; in arrival order a whole backlog would push everything else down |
| Teaser-only feeds are detected by reading two article pages, not guessed from length | short posts are common and complete (notes, quotes, comics); only the page can tell an opening line from the whole text |
| Article text lives in its own table, and the Unread indexes hold unread rows only | every list had to step over each article's HTML to read its flags; and over time nearly everything is read, so an index of what is not stays small |
| Name "Readroom" | a plain noun for the thing, in the spirit of ONCE product names; "feedkeep" was the working name |
| Vanilla script instead of htmx | the UI stays a thin client of the public API, with nothing vendored |
| Everything embedded, enforced by CSP | the app must work on the local network with no internet |
| Display settings stored per device, not on the server | the phone and the desktop want different type and theme; the same goes for whether the desktop sidebar is hidden |
| Tailscale-only access on the NAS | no public IP at home; HTTPS is needed to install the web app on a phone; mirrors the existing password-vault setup |
| Token set in the YAML on the QNAP rather than a readable file | the owner preferred keeping the generated file owner-only over loosening its permissions |
| Manual updates for the first month | changes are batched during testing; a registry adds moving parts not yet needed |
| Images are built by GitHub Actions and pulled from ghcr.io; the NAS still updates only when the application is recreated | building locally and carrying a 20 MB archive to a share for every update was the tedious part; pulling on demand keeps the NAS on a version the owner chose |
| Testing phase: preview locally, deploy and commit only on request | the NAS keeps a stable version while the UI is being tried out |

## 9. Known limitations and open questions

- The NAS was updated on 2026-11-10 to the code at commit `7f057d3`. Later commits have only run locally
  until the next manual update.
- **Local network without internet:** the NAS installation is reachable only through its Tailscale
  address, and whether that keeps working during an internet outage has not been tested. Publishing
  port 8090 on the NAS as well would settle it.
- Summaries are produced only while the owner's desktop is on: the scheduled job and the model run there,
  not on the NAS.
- EPUB output is checked structurally (well-formed XML, correct container) but has not been opened on
  a reMarkable.
- Thumbnails and article images of entries without a local copy load from the original site.
- Search is substring matching over title, summary and excerpt — no full-text search of article bodies.
- No off-site backup of the NAS data yet.

## 10. Later

- Telling the owner about broken feeds outside the app (a webhook or ntfy), if the notice in the Unread
  list proves too easy to miss.
- Full-text search (SQLite FTS5).
- Polish interface.
- Letting other people try it: the images are already in a registry, but that needs a licence and a
  decision on making the repository and the package public.
- Leasing in the summary queue, if more than one summariser runs.
- Before the repository goes public: move the installation specifics (tailnet address, NAS paths)
  out of this document and `CLAUDE.md`.
