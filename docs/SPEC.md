# Readroom — specification

What Readroom is, how it behaves, and why it is built this way. This document describes the state of
the code as of 2026-11-16; the README remains the quick start and API reference.

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
- listening: an article read aloud on request (added 2026-11-15)
- saving a single article by its address, without subscribing to anything (added 2026-11-16)
- going on with a long article on another device from where it was left (added 2026-11-16)
- export to EPUB or Markdown for reading on a reMarkable
- the whole collection exportable as one file

Non-goals: multiple users, a hosted service, calling an LLM or a speech model from the server, recommendation or
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
| QR codes | `rsc.io/qr` | small, no dependencies of its own; for the printed article |
| EPUB | written in-house (`internal/export`) | full control over valid XHTML and embedding images from the database |
| UI | server-rendered `html/template` + small vanilla scripts (`app.js` drives a page, `player.js` the player and in-place page changes, `prefs.js`, `folds.js`, `transitions.js`) | no build step, no vendored framework; the script is a client of the same REST API |

Packages: `cmd/readroom` (entry point: `serve`, `worker`, `clean`, `summarize`, `translate`, `speak`), `internal/store` (schema, queries),
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
  health (`last_error`, `fail_count`, `failing_since`, `last_ok_at`), and `kind`: empty for a
  subscription, `saved` for the one built-in feed that holds the articles saved by address.
- **entries** — everything about an article except its text: `feed_id`, `guid` (unique per feed),
  `url`, `title`, `author`, `published_at`, `created_at` (when it arrived here), `preserved_at` and
  preservation bookkeeping (`preserve_requested`, `preserve_attempts`, `preserve_error`), `excerpt`,
  `image` (thumbnail of a picture-only entry), `text_length`, `summary` + `summary_model` (written by a
  model through the API), `read` + `read_at`, `starred`, `hidden_at`, and `position` + `position_at`
  (how far it has been read, as a share of its text, and when that was reported).
- **entry_bodies** — one row per entry with its HTML: `content` (sanitised, as the feed sent it) and
  `preserved_content` (the local full copy). Kept apart so that lists, counts and searches, which
  read only **entries**, never touch article text.
- **assets** / **entry_assets** — preserved images as blobs, keyed by SHA-256, linked to the entries
  that use them.
- **readlists** / **readlist_entries** — named, ordered-by-added collections of entries.
- **entry_audio** — at most one row per entry: a request to have it read aloud (`requested_at`) and,
  once made, the recording as a blob (`data`, `mime`, `voice`, `created_at`), or why it failed (`error`).
- AI clean-up adds to **feeds** `ai_cleanup`; to **entries** `language` (as the model judged it),
  `held` (waiting for its clean copy, and so in no list), `cleaned_at`, `cleanup_model`,
  `cleanup_error`; and to **entry_bodies** `clean` (the clean copy, Markdown) and `clean_html` (what
  that Markdown renders to, kept so that showing an article costs nothing).
- **entry_translations** — at most one row per entry: a request to have it translated (`language`,
  `requested_at`) and, once made, the translation (`title`, `summary`, `content` as HTML, `model`,
  `created_at`), or why it failed (`error`).

Entries are not deleted when they drop out of a feed, only when they expire (see Retention). Unsubscribing from a
feed deletes its entries and any images no other entry uses. Timestamps are UTC text (`2006-01-02T15:04:05Z`), sortable as strings.

Indexes follow the lists that are shown: by publication date overall and per feed; partial indexes
over unread, visible entries in arrival order (overall and per feed), which also answer the sidebar's
unread counts; a partial index over starred entries; and lookups from an entry to its readlists and
from an image to its entries. After a migration the database is vacuumed and analysed.

## 4. Behaviour

### Feeds

- The interface speaks of subscribing and unsubscribing, not of adding and deleting feeds.
- Subscribed to by address. The address is checked before anything is saved: it must be a feed, or a page that
  leads to one (a `<link rel="alternate">`, or a feed at a customary path such as `/feed.xml`), so a
  site's home page or an article is enough. Otherwise the form says why. Changing a feed's address is
  checked the same way; OPML import is not, and what fails there shows up as below.
- Subscribing says how much of what it already holds starts unread: the last 30 days (the default,
  and never fewer than the 3 newest), the latest 10, everything, or nothing. The rest arrives already
  read — in the archive, not in the way. Imported feeds get the default.
- A new subscription is confirmed: a message names it and says how many articles it brought, and
  its row in the list is briefly highlighted and scrolled into view.
- Each feed is polled every `READROOM_INTERVAL` (default 30 minutes) using conditional requests; a
  background pass runs every minute and handles whatever is due.
- Per feed, the owner can set: **name**, **category**, **keep local copies** with an optional
  **since** date, **keep articles for** (30 days to 2 years, or forever; one year by default), and
  **AI summary**. A new feed starts with AI summary on, whether subscribed to or imported from
  OPML; `ai_summary: false` in the request, or the switch afterwards, turns it off.

### Retention ("keep articles for")

- Each feed keeps its articles for `keep_days` days, counted from publication; 0 keeps them forever.
  Expired articles are deleted hourly, and at once when the setting is saved.
- Never deleted: articles in a readlist, starred articles, and articles with a local copy (made,
  requested, or due under the feed's "keep local copies" setting). Being unread does not protect one.
- A fetch skips items that would be deleted again, so a feed that still lists old articles does not
  bring them back as unread. A newly added feed therefore starts with at most the last `keep_days` days.
- OPML import and export; categories map to OPML folders. The export names each feed as the owner
  sees it, and the import keeps that name as the owner's own, so a rename survives a move to another
  instance; the first fetch drops the name again where it is only the feed's own title.
  - A feed's settings travel too, as attributes in Readroom's own namespace
    (`xmlns:readroom="https://github.com/defkode/readroom"`), which OPML allows and other readers
    ignore: `readroom:aiSummary`, `readroom:aiCleanup`, `readroom:preserve` (`true`/`false`),
    `readroom:preserveSince` (a timestamp, only when set) and `readroom:keepDays` (0 = forever).
    The import applies those it finds and understands; a feed without them, or with a value that
    makes no sense, gets the defaults of a new feed.
  - The import only adds: a feed already subscribed to is left exactly as it is, settings included,
    and none is removed. Articles and what was read do not travel in OPML — "Download everything"
    is for that.
- **Reset database** (Manage feeds → Collection, `POST /reset`) empties the database: every feed
  with its settings, every article with what was read and starred, readlists, saved articles, local
  copies, summaries, translations and recordings. What is left is a fresh installation; the access
  token and each device's display settings are not in the database and stay. The sheet says what
  goes, offers "Download everything" first, and wants the word `reset` typed; the API refuses
  without `{"confirm": "reset"}`. It waits for a running fetch to end, then gives the file's space
  back.

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

### Saved articles

- An article can be saved by its address alone, whether or not any feed carries it:
  `POST /api/v1/articles` with `{url}`. The page is read once, at that moment, and its readable text,
  title, author and date are stored; its images follow in the background as a local copy. It arrives
  unread, at the top of Unread.
- Saved articles live under one built-in feed, "Saved articles", created by the first save. It is
  never fetched, keeps its articles forever, has AI summaries on, belongs to no category, and is
  not in Manage feeds or the OPML export; it cannot be unsubscribed from or refreshed. Everything
  else an article can do — readlists, stars, summaries, listening, export, hiding — works as usual.
  Where a list names an article's feed, a saved article names its site (`onet.pl`).
- The same address saved again is recognised (ignoring the fragment and tracking parameters such
  as `utm_*`) and answers with the article already here, shown again if it had been hidden.
- Nothing is stored when the page cannot be read: the address does not answer, or yields under 200
  characters of text (a sign-in wall, a page drawn by scripts). The reply says which.
- **Pages that hide what they send.** The page is asked for as a first-time visitor would ask: no
  cookies, no session. Sites that count free articles per browser therefore send the whole text —
  but some hide most of it with a `hidden` attribute or an inline `display: none` until their
  script decides the reader may see it. So every page is read twice from the same download, once
  as sent and once with nothing hidden, and the second reading is taken only when it holds over
  1.5 times the text. The same applies to local copies of feed articles. A site that sends only
  the opening to visitors who are not signed in cannot be saved whole this way.
- **In the app:** "Saved articles" sits under Starred in the feed list, and its page starts with a
  field for an address; Manage feeds has the same field. The page opens on All, like a readlist, in
  the order the articles were saved, so one already read is still where it was put. After saving,
  the list is shown with the new article at its top, highlighted.
- **From a phone:** the installed app registers as a share target (`share_target` in the manifest,
  Android only). Sharing a page from the browser opens `/save`, which finds the address in what was
  shared and hands it to the Saved articles page, whose form saves it through the API at once.

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
- In a list a summary is cut to three lines; a click on it (or `e` on the selected row) shows all of
  it in place, and again folds it. That neither opens the article nor marks it read — the summary is
  there to decide whether to. The rest of the row still opens the article.
- In the article the summary folds away with its heading; whether it is folded is remembered per
  device, so it stays that way for every article.
- **Queue:** `GET /api/v1/summaries/pending` returns the articles that need a summary — the feed has
  *AI summary* on, there is no summary yet, and the text is at least `READROOM_SUMMARY_MIN_CHARS`
  (default 1500) long. If the feed also keeps local copies, an article is queued only once its full
  copy is stored. Each item carries plain text ready for a prompt (no HTML), truncated to `max_chars`.
- `PUT /api/v1/entries/{id}/summary` stores `{summary, model}`; `DELETE` removes it and re-queues.
- The queue has no locking or leasing: one summariser at a time. `exclude` lets a client step past an
  article it could not summarise.
- `readroom summarize` works through the queue one article at a time (fetch, summarise, write back),
  up to `-limit` per run, and does not contact the model when the queue is empty.
- **Schedule:** the worker (below) checks the queue every 5 minutes on the owner's desktop and runs it
  against the local model.
- `readroom summarize` is a reference client: any OpenAI-compatible server (tested with a local
  `qwen3.6-fast` through llama-server); the `claude` CLI with Haiku, tools disabled so text inside an
  article cannot make it act; or any command (`-cmd`) that reads the instructions and the article on
  standard input and prints the summary.

### Listening

- An article is read aloud **only when asked**: the headphones button in its toolbar. Nothing is
  recorded for a feed as a whole.
- The server never runs a speech model. Asking puts the article in a queue; a client outside takes the
  text from it, makes a recording however it likes, and uploads it.
- Under its title the article shows where that stands: "Audio in progress…" with Cancel (the page asks
  every 5 seconds while it is open, and shows the recording when it arrives); a Listen button, with
  the voice's name and the day the recording will be deleted; or the reason it failed, with Try again.
- **The player** is one bar at the bottom of the app (above the tab bar on a phone): play and pause,
  the article's title (which leads to it), a position slider, the speed (1× to 2×, remembered per
  device) and a button to close it. A row's ⋮ menu has Listen too, so a recording can be started from
  a list without opening the article. The lock screen and headphone buttons control it (play, pause,
  skip back 15 s and forward 30 s).
- **It keeps playing while the app is used.** Every link here loads a new page, which would end the
  sound. So while something is in the player, moving about swaps the page's content in place instead:
  the same HTML is fetched and put where the old one was, the address and history change as usual, and
  the script that drives a page starts again for the new one. With the player closed, pages load the
  ordinary way. A real page load (a refresh, signing out) stops the sound; the player comes back
  paused where it was, one tap from going on.
- **Queue:** `GET /api/v1/audio/pending` returns the requested articles, oldest request first, as plain
  text for speech: one paragraph per line; code listings, tables, figures, addresses written out and
  the list of links a clean copy ends with left out; every line
  closed by punctuation so a heading does not run into the next sentence. Hidden articles are skipped,
  and one whose full local copy is still on its way waits for it, as with summaries.
- `PUT /api/v1/entries/{id}/audio?voice=…` stores the recording: the body is the file, in any `audio/*`
  type, up to 64 MB. `POST …/audio/error` records a failure, which takes the article out of the queue
  until asked for again. `GET` serves the recording with byte ranges, or with `?download` as a file
  named after the article (the Download link beside Listen); `DELETE` removes it or withdraws
  the request.
- **Recordings are deleted 30 days after they were made**, by the hourly sweep; so are requests nobody
  answered in that time. The article is untouched and can be asked for again.
- `readroom speak` is the reference client: it runs any command that reads text on standard input and
  writes audio to standard output, encodes the result to mono MP3 with ffmpeg (or uploads it as it is,
  with `-type`), and reports a failed command on the article. The title is read first.
- `deploy/tts/tiny-voca-speak` is one such command, around the tiny-voca model: English, two voices,
  far faster than real time on a CPU. It splits paragraphs into sentences, the unit the model reads.
- **What is read is what the article opens in:** asked for while the article has a translation, the
  recording is of the translation, title included; otherwise of the article. The queue says which
  `language` the text is in — the translation's, else the one AI clean-up found, else empty. One
  recording per article: to hear the other version, remove the recording and ask again.
- **A voice per language:** the worker's `[speech.for.<code>]` sections give a language its own command
  and voice name; a text in any other language, or of unknown language, is read by `[speech]` itself.
  The command is told the language in `READROOM_SPEECH_LANGUAGE`.
- Readroom ships no voice for any language but English and knows nothing of what a per-language
  command is: a script of the owner's own, around whatever engine or service they choose. `worker
  check` does not make a per-language voice speak, since it may cost money; it only looks for the
  command.
- **Schedule:** the worker (below) checks the queue every minute on the owner's desktop.

### AI clean-up

- A per-feed switch, **off by default**. For a feed that has it on, every article is read by a model
  before it is shown. The model does two things: says what language the article is in, and says which
  of its blocks are the article and what each one is.
- **The model never writes the article's text.** The server cuts the article's HTML into a flat list
  of blocks — heading (h2–h6; an h1 becomes h2, the title being the page's h1), text, list item,
  quotation, code listing, image with its caption, table, rule — each reduced to the inline marks worth
  keeping: link, bold, italic, inline code, image, sub- and superscript, strikethrough. Every other
  element gives way to its contents; an empty link or emphasis disappears.
- A table is a block of its own only when it is a table of data: no table inside it, and at least two
  rows of the same two or more cells. Any other table is one that lays the page out, as newsletters
  written for e-mail are built, and is cut into the blocks its cells hold. (Before this, a whole
  newsletter issue was two blocks, and clean-up could only keep or drop all of it.)
- A client gives each block a **role**: `keep`, `drop`, or for text `h2`, `h3`, `text`, `quote`, `ref`. The
  server assembles the kept blocks, converts the result to **Markdown**, and stores that as the clean
  copy beside the original, which is never changed. What is shown is the HTML that Markdown renders
  to, so the Markdown is the copy and not a by-product.
- **Every clean copy ends with its links**, under the heading "Links": one numbered list of whatever
  the text links to — in order, each once, named by its words and its site — followed by the entries
  of the author's own list of sources or further reading (role `ref`; the heading the author gave it
  gives way to "Links"). Some authors index their links and some do not; this makes it the same
  everywhere. The links in the text stay where they are, each followed by its number in the list
  as plain text, "[3]" — what Markdown can carry; speech leaves such numbers out. The list is there to be clicked: it is not
  handed out for speech, translation or summaries, and does not count towards the reading time. It
  is recognised by its place and heading (`htmlx.linksSection`), so the Markdown needs no marker.
- **The clean copy is what everything uses** once it exists: the reader, EPUB and Markdown export, the
  text handed out for summaries, translation and speech, the reading time. "Show unedited" in the
  article (`?unedited=1`) shows the article as it came.
- **The gate:** an article that arrives while its feed has clean-up on is *held* — in no list, no
  count, no search — until its clean copy is stored. It then appears as arriving at that moment, at
  the top of Unread. If the computer with the model is off, held articles wait. Articles saved by
  address are never held: they were asked for by name.
- Turning clean-up on does not hide anything: the feed's existing articles, read ones included, are
  queued and replaced by their clean copies in turn. Turning it off lets held articles through as they
  are and keeps the clean copies already made.
- **A failure lets the article through.** If a client reports that it cannot judge an article, the
  article is shown as it came, with the reason, and leaves the queue; removing the failure or the clean
  copy (`DELETE …/cleanup`) queues it again.
- **Queue:** `GET /api/v1/cleanup/pending`: held articles first, then unread, then read, newest first;
  each as its blocks (`kind`, plain `text`). Hidden articles are skipped; an article whose full local
  copy is on its way waits for it. `PUT /api/v1/entries/{id}/cleanup` takes `{of, roles, language,
  model}`; `409` when the text has changed since, `422` for the wrong number of roles, an unknown role,
  or roles that would leave nothing.
- When an article's text is replaced — a local copy kept or removed — its clean copy is forgotten and
  it is queued again (not held again). A summary waits for the clean copy; a translation made of the
  old text is removed.
- The article's language is put on the page (`lang`), and an article already in the owner's language
  offers no translation.
- `readroom clean` is the reference client. It shows the model the title and each block's kind and
  first 280 characters, 80 blocks to a question, and asks for `language: xx` and one line per block
  that needs a change; blocks it does not mention are kept. An article under 200 characters is passed
  without asking. An answer without a language is not trusted at all, and one that would drop more than
  60% of a longer article's text is refused; both are reported on the article. A model out of reach
  stops the run and leaves the queue as it is.
- **Schedule:** the worker (below) checks the queue every minute, when it has a `[cleanup]` section.

### Translation

- Optional, and off unless the server is told the owner's language: `READROOM_LANGUAGE=pl`. Without it
  the app shows nothing about translation and the API refuses a request for one (`422`).
- An article is translated **only when asked**: the Translate button under its title. Nothing is
  translated for a feed as a whole.
- The server never calls a model. Asking puts the article in a queue; a client outside takes it from
  there, translates it however it likes, and sends it back.
- Under the title the article shows where that stands: "Translating into Polish…" with Cancel (the
  page asks every 5 seconds while it is open); the reason it failed, with Try again; or, once made,
  which version is on the page and a button for the other, with Remove.
- **An article that has a translation opens in it** — title, AI summary and text — marked with the
  language (`lang`), and `?original=1` shows the original. When a translation arrives while the article
  is open, the text being read is left alone and the notice offers the translation. Switching replaces
  the page in the history, so Back still leads to the list.
- **Queue:** `GET /api/v1/translations/pending` returns the requested articles, oldest request first,
  each as its title, its AI summary and its text cut into **pieces**: every run of text with its inline
  markup (links, emphasis, inline code), in order. The blocks around them — paragraphs, headings, lists,
  figures, tables — are not sent, and code listings and embedded media are left out altogether. Hidden
  articles are skipped, and one whose full local copy is on its way waits for it.
- `PUT /api/v1/entries/{id}/translation` takes the pieces back translated, in the same order, and puts
  each where the original run was; the result is sanitised like a feed's HTML and stored whole. `of`,
  handed out with the pieces, names the text they were cut from: a mismatch is `409`, the wrong number
  of pieces `422`. A blank piece keeps its original. `POST …/translation/error` records a failure,
  `GET` returns the translation, `DELETE` removes it or withdraws the request.
- One translation per article. Asking again leaves a finished one alone unless the server's language
  has changed since, in which case it is made anew.
- A finished translation is removed when the article's text is replaced — a local copy kept or
  removed — since it would no longer say what the article says. It is otherwise kept as long as the
  article: text is small, unlike speech.
- `readroom translate` is the reference client, with the same choice of model as `readroom summarize`.
  It sends the title, the summary and the pieces to the model up to 12 at a time (about 2,500
  characters), each in a numbered tag, and takes the answer apart by those tags; an answer it cannot
  take apart is asked for again one piece at a time. A model out of reach stops the run and leaves the
  article queued; a model that answers with nothing is reported on the article.
- **Schedule:** the worker (below) checks the queue every minute, when it has a `[translations]` section.

### The worker

- `readroom worker` is the client side in one piece: one process on a computer that has the models,
  keeping both queues empty, set up by one file (`~/.config/readroom/worker.toml`: the server, its
  token or a file holding it, and a section each for `[cleanup]`, `[summaries]`, `[translations]` and `[speech]`). A section left out is
  work left to someone else. It adds nothing to the API and does nothing `summarize`, `translate` and
  `speak` do not; those stay for single runs and scripts.
- A model is reached in one of two general ways, so no engine needs code of its own: an
  OpenAI-compatible address (Ollama, llama.cpp, LM Studio, hosted services) or a command from standard
  input to standard output (summaries and speech alike). The `claude` backend is the one exception,
  kept because it switches the CLI's tools off.
- Each queue has its own loop: work until it is empty, wait `every` (5 minutes for summaries, 1 for
  translations and speech, where somebody is waiting), again. A failure — server out of reach, model not started — is
  logged once, retried at the same pace, and its end is logged too.
- `worker init` writes a commented file; `worker check` tries the server and token, one sample clean-up
  (which must not drop the article's own text), one sample summary,
  one sample translation (which must keep its link) and one sample sentence of speech, and writes
  nothing to the server; `worker install` writes and
  starts a systemd user service (with the installing shell's `PATH`, which a user service otherwise
  lacks) and stops the two timers that did this work before; `worker uninstall` removes it.
- An unknown setting in the file is an error, not ignored: a mistyped name would otherwise silently
  do nothing.

### Reading, organising, searching

- Views: Unread, All, Starred; by feed, by category, by readlist, each tab showing how many articles
  it holds. Unread lists show what arrived in the reader last first (and, within one fetch, the newest
  first), so a newly added feed's articles sit together at the top instead of being scattered among
  older ones; All and Starred are an archive, by publication date. Opening an article marks it read and records when (`read_at`); from the second
  visit on, the article shows how long ago that was under its title, next to "Mark as unread".
  Back on a list, a message says "Marked as read" with Undo for a few seconds — for every article
  first read on the way, when several were stepped through — so a mistaken tap, or the last unread
  article of a feed, can be put back without finding it again.
- **Going on where it was left.** While an article is read, the page reports how far the reader
  has got (`PATCH /entries/{id}` with `position`, 0 to 1), a little after each scroll and when the
  page is left. The position is a share of the article's text — the text above the top of the
  screen — not of the page's height, so it means the same sentence on a phone and on a wide screen.
  Articles that fit on a screen and a half are not tracked.
  - Opened again, here or on another device, an article read between 3% and 97% **asks**: "You
    stopped reading at 43%", with Continue there and Dismiss. It does not scroll by itself. While
    the question stands, the kept position only moves forward, so looking at the top loses nothing;
    scrolling to that place or past it answers the question.
  - The main Unread list starts with **Continue reading**: up to three such articles, the one read
    last first, each with its percentage and a ✕ that takes it off (its position goes back to 0).
    That is the way back to an article which opening has already taken out of Unread.
- Lists and articles show an estimated reading time (about 1,400 characters a minute, from the best
  text available: the local copy if there is one, else what the feed sent, which may be only a teaser).
- A row marks what the owner did to its article with the same filled, accent-coloured icons as the
  article's toolbar, always in the same order: in a readlist, starred, local copy kept, and headphones when it has a recording.
- Every row has a ⋮ menu: mark as read or unread, add to a readlist, star, keep a local copy, "Mark this and older as read", and in a readlist
  "Remove from readlist". The list's own ⋮ menu has "Mark all N as read" and, for a
  single feed, "Feed settings" (the same sheet as in Manage feeds); for a readlist it has "Readlist
  settings" (a sheet to rename or delete it) and its EPUB and Markdown exports. Both bulk actions apply to the
  current feed or category (or everything), are not offered in a readlist, in Starred or in search
  results, and can be undone from the message that follows for a few seconds.
- An article ends with links to the previous and next article of the list it was opened from (the
  list as it was shown, further pages included), and going back returns to that list.
- Page changes are animated by the browser (cross-document view transitions): a quick cross-fade, with
  the content sliding forward into an article or the next one and back to the list or the previous one.
  Browsers without them, and reduced-motion settings, simply switch pages.
- Keyboard shortcuts on a list (`j`/`k` select, `Enter` open, `m` read, `s` star, `e` whole summary, `/` search) and in an
  article (`j`/`k` next and previous, `u` back, `m` unread, `s` star, `v` original); `?` lists them.
- Readlists: create, rename, delete; add or remove articles (from the article or from the row menu).
  Renaming is in the readlist's own ⋮ menu; Manage readlists creates, exports and deletes.
- **Hiding:** an article can be hidden for good — from its row menu, from the article, or with `h` —
  to keep the collection to what is worth keeping. A hidden article is in no list, search, count,
  readlist, export or summary queue, and a refresh does not bring it back. Nothing else about it
  changes, so "Show again" restores it exactly, star and readlists included. Hiding is followed by an
  Undo; later, hidden articles are listed under Settings → Hidden articles (Manage on a phone). They
  expire with the feed's other articles.
- Search matches the title, the AI summary and the excerpt (substring, not full text). It filters as
  the owner types, a second after the last key (at once on Enter, or when the field is emptied), and
  the search glass pulses while it waits.
- **Period:** beside the search, a list can be narrowed by publication date — Today (since midnight
  on the server's clock, the one list dates are shown by), Last week (7 days), Last month — or left
  at Any time. In the API it is `period=today|week|month` on `GET /entries`.
- A search and a period change the list where it stands: the page for the new address is fetched
  and its list, view tabs, menus and "Continue reading" replace the ones shown, while the form is
  left alone — so the field keeps its focus and a phone its keyboard. The address follows
  (replacing, not adding to, history), so going back from an article returns to the filtered list.
  The view tabs count within the search and period and their links keep both. "Mark all as read"
  is not offered in a filtered list.

### Export

- **EPUB 3** for one article or a whole readlist (one chapter per article, with the summary, source
  link and embedded preserved images; WebP is converted to PNG; images that were never downloaded are
  left out).
- **Markdown** with YAML front matter, for one article or a readlist.
- **Print:** printing an article from the browser (or saving it as a PDF) gives the article alone,
  in dark ink on white whatever the theme: no navigation, buttons, reading status, player or
  offers. The AI summary stays if it is unfolded. In their place the page gains what paper lacks —
  a footer after the last line, with a QR code of the original's address and the address in full. The
  code is drawn by the server as inline SVG, so nothing is loaded from elsewhere. For the time of
  printing the page's title becomes a short name, the source and the headline's first sentence
  (`onet.pl - Polski profesor pracuje z twórcami AI w USA`), because that is what browsers call
  the saved file.
- **Whole collection:** `GET /api/v1/export/db` streams a consistent snapshot of the SQLite file —
  feeds, articles, images and readlists in one file.

## 5. Web app

- Installable (manifest, icons); bottom tab bar on phones, sidebar from 900 px up, which can be hidden (remembered per device). In the sidebar, Manage feeds, Manage readlists and Display sit in one Settings menu next to the name, and Sign out at the foot; on a phone they are a tab and a section of the Feeds page.
- A category in the feed list (the sidebar, and the Feeds tab on a phone) folds away with the arrow on
  its line; its name still leads to the category's articles. Folded, it shows the category's unread
  count. Everything starts unfolded, and which ones are folded is remembered per device. Categories
  are in alphabetical order, with the feeds that have none after them, under "Uncategorised".
- **Themes:** light and dark in neutral greys with a blue accent, after macOS; no others. The default follows
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
| `READROOM_LANGUAGE` | *(none)* | the owner's language as a code (`pl`); turns translation on |
| `READROOM_LOG` | `info` | `debug` logs every request |

## 7. Deployment

- **Image:** multi-stage build to a distroless static image, about 32 MB, around 16 MB of RAM idle.
- **The owner's installation:** a small arm64 QNAP, as the Container Station application in
  `deploy/qnap-compose.yaml` — the app plus its own Tailscale node, reachable only inside the tailnet
  over HTTPS. No port is published on the NAS. Data lives on a NAS share covered by the volume's
  snapshots. The specifics of that installation (addresses, which version runs there) are kept in
  the owner's own runbook, not in this repository.
- **Token on the QNAP:** set directly as `READROOM_TOKEN` in the application's YAML and kept in the
  owner's password manager. The token-file option is not used there, because a file written by the
  container is root-only on the share and File Station cannot open it. The repository's
  `deploy/qnap-compose.yaml` carries a `CHANGE_ME` placeholder; the real value is never committed.
- **Updates are pulled, when the owner chooses:** a push to `main` that changes more than
  documentation runs the tests and publishes
  `ghcr.io/defkode/readroom` for arm64 and amd64 (`:latest`, and `:sha-<commit>` to go back to; a
  `v*` tag adds a version tag). On the NAS, recreating the application pulls the newest image —
  nothing updates by itself. The repository and the package are public, so the NAS pulls without a
  login. `make image-qnap` remains as the offline route: an archive in the classic
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
| AI summaries are set per feed, on for a new one, with a length threshold | the owner wants a summary nearly everywhere, so turning it off is the exception; short notes and comics are skipped by length |
| A dedicated queue endpoint that returns ready plain text | a script or agent needs one request and no HTML parsing |
| Summariser pulls one article at a time, checking every 5 minutes, from the desktop with the local model | no work is held locally, an interrupted run loses nothing, and the NAS is too small to run a model |
| One `readroom worker` with one file replaces the per-queue timers, flags and environment file | the loose units worked, but setting them up meant copying four files and editing commands inside them; anyone else running Readroom needs one thing to configure, one to check and one to install |
| Every push to `main` publishes the binary for Linux and macOS as one release that moves forward (`rolling`) | somebody setting up a worker needs a file to download, not a toolchain; there are no version numbers here to cut releases by, and one address that always holds the newest build keeps the instructions to a single command |
| The worker is a command of the same binary, not a separate program | one thing to build, release and keep in step with the API; the size is no burden on a desktop |
| Models are reached by an OpenAI-compatible address or by a command, not by a plugin per engine | those two cover nearly everything people run, and a new engine needs no change here |
| `worker check` exists and writes nothing | a first set-up fails in one of four places (server, token, model, voice); finding which should not take reading a journal |
| One summariser at a time, no leasing | simplest thing that works for one person; add leasing if several workers are ever needed |
| Excerpt from the feed's teaser, else the first real paragraph, else the image caption | every article shows something useful without waiting for a model |
| Bulk "mark as read" is a labelled menu item with Undo, not a bare icon with a confirmation | a lone check mark did not say it meant everything; confirmations get clicked through, undo actually recovers a slip |
| Opening an article still marks it read at once, and the list it leads back to offers Undo | waiting or scrolling before marking would make "read" a guess; the slip is only noticed back on the list, where the row is already gone, so that is where the way back belongs |
| Removing a local copy asks for confirmation instead of offering Undo, and marks the article as out of fetch attempts | undo would mean fetching the page again, which is exactly what may no longer work; and without the mark a feed that keeps local copies would quietly bring it back on the next pass |
| "Mark this and older as read" instead of checkboxes for partial marking | one tap closes the rest of a list once nothing below is of interest; multi-select is fiddly on a phone |
| Next and previous article come from the list as the browser showed it, kept per tab | an article has no order of its own; a server-side "next unread" would skip what was just read and could not tell which list the reader came from |
| Articles expire per feed after a year by default, with "forever" as a choice (after HEY's per-contact recycling) | the database would otherwise only grow; what is worth keeping is starred, put in a readlist or given a local copy, and those never expire |
| Light and dark themes are neutral greys with a blue accent; the warm sepia theme was removed (2026-11-15) | the owner disliked the brown, paper-like palette and prefers macOS-style greys; a device that had sepia chosen falls back to following the system |
| Stylesheet and scripts are linked by a content version and cached for good; text responses are gzipped | on the NAS every page view re-downloaded 55 KB of assets and an uncompressed 64 KB page through the Tailscale proxy, about half a second per click |
| A feed is reported as broken only after 3 failed fetches in a row, on the Unread list rather than only in Manage feeds | one timeout is not news; and the place the owner looks every day is the only place a silent failure gets noticed |
| Adding a feed validates first and finds the feed from a page address | a mistyped address used to leave a dead feed behind, and guessing `/feed.xml` by hand was the common way to mistype |
| Unwanted articles are hidden, not deleted, and hiding touches nothing else | a deleted article would come back as new on the next fetch; leaving its star and readlists alone makes "Show again" an exact undo |
| Unread is ordered by arrival, not publication, and a new feed brings only its recent articles as unread | in publication order a new feed's backlog was scattered deep in the list and missed; in arrival order a whole backlog would push everything else down |
| Teaser-only feeds are detected by reading two article pages, not guessed from length | short posts are common and complete (notes, quotes, comics); only the page can tell an opening line from the whole text |
| Article text lives in its own table, and the Unread indexes hold unread rows only | every list had to step over each article's HTML to read its flags; and over time nearly everything is read, so an index of what is not stays small |
| Articles are read aloud on request only, by a client outside the server, through a queue like the summaries' | speech for every article would be made mostly for nothing; and the NAS is too small for a model, while the desktop makes 13 minutes of speech in 10 seconds |
| AI clean-up asks the model which blocks belong and what they are, never for the text | a model that rewrites an article can shorten, rephrase or lose a paragraph, and nobody would notice in a reader; judging blocks cannot change a word, takes seconds instead of a minute, and gives an answer whose shape can be checked |
| New articles of a feed with clean-up on are held until they have their clean copy (the owner's choice over a time limit) | the owner wants never to see an article that has not been through it; the price, accepted knowingly, is that nothing new from those feeds appears while the desktop is off |
| A reported failure releases a held article | the alternative is an article that stays invisible for good because one model could not read it |
| Clean-up is per feed and off by default | it changes when articles appear and spends model time on every one; a feed whose markup is already clean gains little |
| The clean copy is stored as Markdown beside the original, with its rendered HTML | Markdown's vocabulary is exactly what a clean article needs, so nothing else can creep in, and it is what translation, speech and export want; keeping the original means an article can be cleaned again by a better model, and "Show unedited" always has an answer |
| Translation is on request, per article, through a queue like listening's | the owner reads English and wants the odd article in Polish; translating everything would occupy the desktop's model for hours to make text mostly never opened |
| The owner's language is a server setting (`READROOM_LANGUAGE`), not a per-device preference or a parameter of each request | it is one fact about the owner, set once like the token; the queue then tells any client what to translate into, so clients need no language setting of their own |
| An article is handed out as pieces of inline HTML and put back together by the server | a model given a whole page of HTML loses images, breaks nesting and runs out of room on a long article; pieces keep the structure out of its hands, can be done a few at a time by a small local model, and make "as many back as went out" a check any client can be held to |
| Pieces carry their inline markup rather than plain text | a link or an emphasis moves within a sentence when it is translated; only the translator knows where it belongs |
| A translated article opens in the translation, and a translation that arrives mid-reading does not replace the text | it was asked for in order to be read; but swapping the words under someone's eyes would lose their place |
| A recording is of the version the article opens in, and voices are chosen per language in the worker | the owner listens to what they would read; no free model the owner tried reads Polish acceptably, so Polish goes to a command of their own choosing while English stays free and local; neither the server nor the worker knows what that command is |
| The audio API takes any audio file and knows nothing about what made it; `readroom speak` runs any command from text to audio | text-to-speech models change quickly — the first one used, tiny-voca, was days old — and replacing one must not touch the server |
| Speech is synthesised on the desktop rather than in the browser | tiny-voca's browser build needs a Python runtime in WebAssembly for its phonemizer, about 50 MB to embed and to download, and playback would stop with the phone's screen; a stored file plays like any other audio |
| Recordings live in the database and are deleted after 30 days | the collection stays one file; speech is large (about 0.36 MB a minute) and listened to once, and can be made again in seconds |
| MP3 from the reference client | plays in every browser and seeks without an index; at 48 kbit/s mono it is transparent for a 24 kHz voice |
| One player for the whole app, and while it is on, pages are swapped in place rather than loaded | the owner listens while doing other things in the app, and a page load ends any sound; resuming on the next page would leave a gap at every click and is refused by phones without a tap. Only while playing, so ordinary use keeps plain page loads |
| The swap replaces the whole page body and starts the page script again, instead of a framework or hand-made partial updates | every page keeps being written as a page; the one rule is that listeners are registered through the page's `on()` so they end with it |
| Saved articles belong to a built-in feed of their own kind, instead of entries without a feed | every list, count, queue and export joins an article to its feed; one more feed, never fetched, gives saved articles all of that with a handful of exceptions rather than a second kind of article everywhere |
| That feed is created by the first save, not by the migration | a reader that never saves an article shows nothing new, and feed ids in an empty database still start at 1 |
| Saving reads the page at once and stores nothing when that fails | the answer — saved, or why not — is on the screen while the page is still at hand; a stub to retry later would sit in Unread as a title-less row |
| A page is also read with its hidden parts shown, and that reading wins only when it is much longer | a metered paywall sends the whole article and hides it with a style; showing everything on every page would let menus and dialogs into the text, so the plain reading stays the default |
| Sharing from the phone goes through a page that fills in the app's own form | the web app stays a client of the REST API: the share target only carries the address to the same form a typed address uses |
| The reading position is a share of the article's text, kept on the server, and reported by the page | a scroll offset or a share of the page's height means a different place at every width and type size; the text above the screen's top is the same on all of them. On the server because the point is another device |
| A half-read article asks before it scrolls, and the kept place only moves forward until answered | the owner's choice, after the Kindle: jumping by itself is wrong whenever the article is reopened to look something up near the top, and that look must not overwrite the place |
| Half-read articles are listed at the top of Unread | opening an article marks it read, so on the second device it is no longer where the owner would look for it |
| Printing is a stylesheet for the article page, not another export format | the browser already makes a good PDF of the page as the owner set its type; it only had to lose the interface and gain the way back to the original |
| The QR code is drawn on the server (`rsc.io/qr`) into the page | a script library would have to be vendored, and a picture service would break "everything embedded" |
| Name "Readroom" | a plain noun for the thing, in the spirit of ONCE product names; "feedkeep" was the working name |
| Vanilla script instead of htmx | the UI stays a thin client of the public API, with nothing vendored |
| Everything embedded, enforced by CSP | the app must work on the local network with no internet |
| Categories and the article's AI summary fold with plain `<details>`, and a few lines of script (`folds.js`) remember which | the browser already does the folding, with keyboard and accessibility; but every click loads a new page, which would unfold everything again |
| Display settings stored per device, not on the server | the phone and the desktop want different type and theme; the same goes for whether the desktop sidebar is hidden |
| Tailscale-only access on the NAS | no public IP at home; HTTPS is needed to install the web app on a phone |
| Token set in the YAML on the QNAP rather than a readable file | the owner preferred keeping the generated file owner-only over loosening its permissions |
| Manual updates for the first month | changes are batched during testing; a registry adds moving parts not yet needed |
| Images are built by GitHub Actions and pulled from ghcr.io; the NAS still updates only when the application is recreated | building locally and carrying a 20 MB archive to a share for every update was the tedious part; pulling on demand keeps the NAS on a version the owner chose |
| CI compiles the binaries on the runner and the image build only copies them in; the same Dockerfile still compiles by itself everywhere else | inside the image build every change recompiled everything for both platforms, most of a five-minute run; the runner keeps Go's build cache between runs |
| The specifics of the owner's installation live in a private runbook, not here | the repository is public; an address or the deployed version helps nobody else and dates quickly |
| MIT licence | the repository is public, and without a licence nobody else may use the code; MIT is the simplest permissive one for a small personal tool. The bundled font keeps its own Open Font License |
| A clean copy ends with one list of the article's links, kept out of speech and translation | the owner lost minutes of a paid recording to a bibliography read out address by address; and whether an article indexes its links should not depend on its author |
| Articles saved by address have AI clean-up on from the start | one saved by hand is one the owner means to read, often aloud; the same reasoning as for summaries |
| Search filters as you type by swapping parts of the fetched page in, not by loading it | a page load, even a swapped-in one, takes the focus from the field and closes a phone's keyboard in the middle of a word |
| The period filter is three fixed spans in the search field, not a date range | the owner asked for today, last week, last month; a native select costs no extra row on a phone |
| Feed settings ride in the OPML export as namespaced attributes, not in a format of our own | the owner moved their feeds to a second instance and lost every per-feed switch; OPML is extensible this way, and the file stays a plain feed list for any other reader |
| Reset database asks for the word "reset" to be typed, in the sheet and in the API | the one action that destroys everything should not be one stray tap or one mistyped request away; the owner wanted it for starting a trial instance over |
| Testing phase: preview locally, deploy and commit only on request | the NAS keeps a stable version while the UI is being tried out |

## 9. Known limitations and open questions

- The NAS runs whatever image was newest when its application was last recreated; later commits run
  locally only until then.
- **Local network without internet:** the NAS installation is reachable only through its Tailscale
  address, and whether that keeps working during an internet outage has not been tested. Publishing
  port 8090 on the NAS as well would settle it.
- Summaries are produced only while the owner's desktop is on: the worker and the model run there,
  not on the NAS.
- Recordings are made only while the owner's desktop is on, like summaries; a request made meanwhile
  waits (up to 30 days).
- The bundled free text-to-speech script is English only, and mispronounces some names and unusual
  words. Without a `[speech.for.…]` voice for it, a translation is read by that English voice, badly.
- A per-language voice may be a paid service; nothing in the app shows what a recording will cost
  before it is asked for.
- With AI clean-up on, a feed's new articles appear only while the owner's desktop is on. Nothing in
  the app says so outside Manage feeds.
- Clean-up judges whole blocks: it cannot remove an advertisement from the middle of a paragraph. It
  is as good as the model's judgment — seen so far: bylines, credits and "follow us" lines go, and
  occasionally the line naming who said a pull quote. Figures become a picture with an italic caption,
  and markup inside tables beyond text is lost.
- Translations are made only while the owner's desktop is on, like summaries, and take a local model
  about a minute for a short article.
- Only the article page is translated: lists, search, exports (EPUB, Markdown), print's file name and
  the spoken version all use the original. Text that sits directly beside block elements in unusual
  markup is translated as its own piece, without the context of its neighbours.
- A translation is as good as the model that made it; nothing checks it beyond its shape.
- A recording is read into memory whole to be served (a few MB each); fine for one listener.
- EPUB output is checked structurally (well-formed XML, correct container) but has not been opened on
  a reMarkable.
- Thumbnails and article images of entries without a local copy load from the original site.
- Saving an article reads what the site sends to an anonymous visitor. Articles a site shows only
  to signed-in subscribers, or builds with scripts, cannot be saved; sending the page's HTML from
  the owner's own browser (a bookmarklet posting to the API) would cover those.
- The share target appears on Android only, and only for the installed app; a change to the
  manifest reaches an already installed app when the browser next refreshes it (up to a day or so).
- A saved article's author is taken from the page and left empty when the guess looks wrong; its
  excerpt comes from the first paragraphs, which on some sites is an info box.
- The reading position is one per article, not per device: the last report wins (or the furthest,
  while the question to continue is unanswered). It is not kept for the listening player.
- Search is substring matching over title, summary and excerpt — no full-text search of article bodies.
- No off-site backup of the NAS data yet.

## 10. Later

- Telling the owner about broken feeds outside the app (a webhook or ntfy), if the notice in the Unread
  list proves too easy to miss.
- Full-text search (SQLite FTS5).
- Polish interface.
- Leasing in the summary queue, if more than one summariser runs.
- A playlist of recordings: playing the next one when one ends.
- Saving a page as the owner's own browser sees it (`{url, html}`), for sites that need a sign-in.
