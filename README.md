# Readroom

A small self-hosted RSS/Atom reader. One static Go binary, one SQLite file.

- Web app for phone, tablet and desktop (installable: "Add to Home Screen" / "Install app")
- REST API for everything the UI does — summaries, read/starred, readlists, exports
- Per-feed settings: name, category, and whether to keep full local copies of articles (and since when)
- Preserved articles (text + images) live inside the database, so they survive the original going away
- Export an article or a readlist as EPUB or Markdown; download the whole collection as one `.db` file

What it does and why it is built this way: [`docs/SPEC.md`](docs/SPEC.md).

## Run

```sh
echo "READROOM_TOKEN=$(openssl rand -hex 24)" > .env
docker compose up -d --build        # http://<host>:8090, sign in with the token
```

Without Docker on the target: `make build` produces `bin/readroom` (static, no dependencies).
`make run` starts it locally on :8090 with `./readroom.db` and no token.

| Variable | Default | |
|---|---|---|
| `READROOM_ADDR` | `:8090` | listen address |
| `READROOM_DB` | `./readroom.db` (`/data/readroom.db` in the image) | database path |
| `READROOM_TOKEN` | *(none — open access)* | token for the UI login and `Authorization: Bearer` |
| `READROOM_TOKEN_FILE` | | read the token from this file instead; a random one is generated there on first run |
| `READROOM_INTERVAL` | `30m` | how often each feed is polled |
| `READROOM_LOG` | `info` | `debug` logs every request |

For Proxmox (amd64), `make image-multiarch` writes loadable image archives to `dist/`.
For the QNAP, `make image-qnap` builds the arm64 archive in the layout Container Station can import, and
`deploy/qnap-compose.yaml` is the application definition (Readroom behind its own Tailscale HTTPS node).

## API

All under `/api/v1`, JSON, `Authorization: Bearer $READROOM_TOKEN`.

| | |
|---|---|
| `GET /feeds` · `POST /feeds` | list · add `{url, title?, category?, preserve?, preserve_since?, keep_days?}`; `url` may be a feed or a page that leads to one, and `422` explains an address that is neither. Feeds report their health: `fail_count`, `failing_since`, `last_ok_at`, `broken`, `stale` |
| `GET/PATCH/DELETE /feeds/{id}` | `PATCH` takes any of `title`, `category`, `url`, `preserve`, `preserve_since` (`"2026-10-01"` or `null`), `ai_summary`, `keep_days` (default 365; `0` keeps articles forever) |
| `POST /feeds/{id}/refresh` | fetch now |
| `GET /entries` | filters: `feed_id`, `category`, `readlist_id`, `read`, `starred`, `preserved`, `summary=missing\|present`, `q`, `order=asc`, `limit`; page with `before=<next>` |
| `GET /entries/{id}` | includes `content` and `preserved_content`; every entry carries `reading_minutes`, an estimate from its text length |
| `PATCH /entries/{id}` | any of `read`, `starred`, `summary`, `summary_model` |
| `GET /summaries/pending` | work queue for a summarising script or agent — see below; `limit`, `max_chars`, `feed_id` |
| `PUT /entries/{id}/summary` | `{summary, model}`; `DELETE` removes it and re-queues the article |
| `POST /entries/{id}/preserve` | keep a local copy of this one article |
| `POST /entries/mark-read` | optional `{feed_id}` or `{category}`; `{through: id}` limits it to that article and older ones; returns `{marked, ids}` |
| `POST /entries/mark-unread` | `{ids}` — undoes a mark-read |
| `GET /entries/{id}/export?format=epub\|md` | |
| `GET/POST /readlists` · `GET/PATCH/DELETE /readlists/{id}` | |
| `PUT/DELETE /readlists/{id}/entries/{entry}` | add / remove |
| `GET /readlists/{id}/export?format=epub\|md` | one book, one chapter per article |
| `GET /export/db` | consistent snapshot of the whole collection (SQLite) |
| `GET /export/opml` · `POST /import/opml` | feed list in/out |

```sh
curl -H "Authorization: Bearer $READROOM_TOKEN" -X PATCH localhost:8090/api/v1/entries/42 \
  -d '{"summary": "Two sentences.", "starred": true}'
```

## Summaries

Every article has an `excerpt`: the feed's own teaser when it publishes a short one, otherwise the opening
of the text. An AI `summary` replaces it wherever one exists, marked with an AI badge.

The server never calls a model. It keeps a queue instead: `GET /api/v1/summaries/pending` returns articles
whose feed has **AI summary** switched on, that have no summary yet, and whose text is at least
`READROOM_SUMMARY_MIN_CHARS` (default 1500) long. Feeds that also keep local copies are queued once the full
copy is stored. Each item carries the plain text, ready for a prompt:

```json
{"entries": [{"id": 42, "feed_title": "The Go Blog", "title": "…", "url": "…", "published_at": "…",
              "text_length": 12084, "text": "…", "truncated": false}],
 "remaining": 7, "min_chars": 1500}
```

Write the result back with `PUT /api/v1/entries/42/summary` and `{"summary": "…", "model": "qwen3.6"}`.
`exclude=4,9` leaves out ids a client wants to step past. The queue has no locking: run one summariser
at a time.

`readroom summarize` is an ordinary client of that queue:

```sh
export READROOM_TOKEN=…
readroom summarize -llm http://127.0.0.1:8080/v1 -model qwen3.6-fast   # any OpenAI-compatible server
readroom summarize -backend claude                                     # claude CLI, Haiku, tools disabled
```

It takes one article at a time — fetch, summarise, write back — until the queue is empty or `-limit`
(default 20) is reached, so it never holds work locally and can be interrupted safely. With an empty
queue it does not touch the model at all.

To have the queue checked every 5 minutes on the machine that has the model, install the user units in `deploy/systemd/`
(instructions at the top of the service file). The job needs a single environment variable,
`READROOM_TOKEN`, read from `~/.config/readroom/env`; until that is filled in, runs are skipped.

## Layout

```
cmd/readroom        entry point: serve, summarize
internal/store      SQLite schema and queries
internal/fetch      feed polling, article preservation
internal/export     EPUB and Markdown
internal/server     REST API, web app (templates/, static/)
```

`make test` runs the suite (toolchain in Docker; Go is not needed on the host).
