# Readroom

A small self-hosted RSS/Atom reader. One static Go binary, one SQLite file.

- Web app for phone, tablet and desktop (installable: "Add to Home Screen" / "Install app")
- REST API for everything the UI does — summaries, read/starred, readlists, exports
- Articles read aloud on request, by whatever text-to-speech you run yourself
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
For the QNAP, every push to `main` publishes `ghcr.io/defkode/readroom:latest` (arm64 and amd64) from
`.github/workflows/image.yml`, and `deploy/qnap-compose.yaml` is the application definition that pulls
it (Readroom behind its own Tailscale HTTPS node). `make image-qnap` still builds an arm64 archive
that Container Station can import, for when the registry is out of reach.

## API

All under `/api/v1`, JSON, `Authorization: Bearer $READROOM_TOKEN`.

| | |
|---|---|
| `GET /feeds` · `POST /feeds` | list · add `{url, title?, category?, preserve?, preserve_since?, keep_days?, unread?}` (`unread`: how much of the backlog starts unread — `recent` by default, `latest`, `all` or `none`); `url` may be a feed or a page that leads to one, and `422` explains an address that is neither. Feeds report their health (`fail_count`, `failing_since`, `last_ok_at`, `broken`, `stale`) and `teaser_only`, set when their articles are far longer on the site than in the feed |
| `GET/PATCH/DELETE /feeds/{id}` | `PATCH` takes any of `title`, `category`, `url`, `preserve`, `preserve_since` (`"2026-10-01"` or `null`), `ai_summary`, `keep_days` (default 365; `0` keeps articles forever) |
| `POST /feeds/{id}/refresh` | fetch now |
| `GET /entries` | hidden articles are left out unless `hidden=true` asks for them instead; filters: `feed_id`, `category`, `readlist_id`, `read`, `starred`, `preserved`, `summary=missing\|present`, `q`, `order=asc` or `order=arrival` (newest arrivals first), `limit`; page with `before=<next>` |
| `GET /entries/{id}` | includes `content` and `preserved_content`; every entry carries `reading_minutes`, an estimate from its text length |
| `PATCH /entries/{id}` | any of `read`, `starred`, `hidden`, `summary`, `summary_model` |
| `GET /summaries/pending` | work queue for a summarising script or agent — see below; `limit`, `max_chars`, `feed_id` |
| `PUT /entries/{id}/summary` | `{summary, model}`; `DELETE` removes it and re-queues the article |
| `POST /entries/{id}/audio` | ask for the article to be read aloud; the entry's `audio` is then `queued`, later `ready` or `failed` |
| `GET /audio/pending` | work queue for a text-to-speech client — see below; `limit`, `max_chars`, `exclude` |
| `PUT /entries/{id}/audio?voice=…` | the recording itself as the body, with its `audio/*` content type; `GET` plays it back (byte ranges supported), `DELETE` removes it or withdraws the request |
| `POST /entries/{id}/audio/error` | `{error}` — a client reporting that it could not make the recording |
| `POST /entries/{id}/preserve` | keep a local copy of this one article |
| `DELETE /entries/{id}/preserve` | delete that local copy (text and images) and return the article; it is not fetched again unless asked for |
| `POST /entries/mark-read` | optional `{feed_id}` or `{category}`; `{through: id}` limits it to that article and those below it (older ones, or with `order: "arrival"` those that arrived earlier); returns `{marked, ids}` |
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

## Listening

An article can be read aloud. The headphones button in an article asks for it; the article shows that
the recording is in progress, and then a Listen button. It plays in a bar at the bottom of the app that
keeps playing while you read other pages, and from the lock screen. Recordings are deleted 30 days
after they were made.

As with summaries, the server produces nothing itself and does not care what does. It keeps a queue:
`GET /api/v1/audio/pending` returns the articles waiting, oldest request first, each with the text to
read — plain text, one paragraph per line, without code listings, tables and figures:

```json
{"entries": [{"id": 42, "feed_title": "The Verge", "title": "…", "author": "…", "url": "…",
              "published_at": "…", "text": "…", "truncated": false}],
 "remaining": 1}
```

Upload the result with `PUT /api/v1/entries/42/audio?voice=my-voice`: the body is the audio file, in
any format a browser plays, and `Content-Type` says which (`audio/mpeg`, `audio/ogg`, `audio/mp4`, …);
at most 64 MB. If it cannot be made, `POST /api/v1/entries/42/audio/error` with `{"error": "…"}` shows
the reason on the article, where "Try again" queues it once more. A recording can also be uploaded for
an article nobody asked about.

```sh
curl -H "Authorization: Bearer $READROOM_TOKEN" -H "Content-Type: audio/mpeg" \
  -X PUT --data-binary @article.mp3 "localhost:8090/api/v1/entries/42/audio?voice=my-voice"
```

`readroom speak` is an ordinary client of that queue, and brings no voice of its own: `-cmd` is any
program that reads text on standard input and writes audio to standard output.

```sh
readroom speak -cmd deploy/tts/tiny-voca-speak -voice "tiny-voca M2"           # the bundled script
readroom speak -cmd "my-tts --voice anna --wav -" -voice "anna"                  # any other engine
readroom speak -cmd my-tts-that-writes-opus -type audio/ogg                    # uploaded as it is
```

Without `-type` the command's output is taken to be anything `ffmpeg` reads (WAV, usually) and encoded
to mono MP3 at `-bitrate` (48k: about 0.36 MB a minute). It takes one article at a time until the queue
is empty or `-limit` (default 10) is reached.

[`deploy/tts/tiny-voca-speak`](deploy/tts/tiny-voca-speak) is one such command, for
[tiny-voca](https://github.com/n4ze3m/tiny-voca): a 4.5M-parameter English model that reads numbers,
dates and money sensibly and makes about 13 minutes of speech in 10 seconds on a desktop CPU. It runs
with [uv](https://docs.astral.sh/uv/), which installs what it needs, and downloads the model (27 MB)
on first use. English only.

To have the queue checked every minute, install `deploy/systemd/readroom-speak.*` (instructions at the
top of the service file); it uses the same `~/.config/readroom/env` as the summariser.

## Layout

```
cmd/readroom        entry point: serve, summarize, speak
internal/store      SQLite schema and queries
internal/fetch      feed polling, article preservation
internal/export     EPUB and Markdown
internal/server     REST API, web app (templates/, static/)
deploy              NAS compose file, systemd units for the desktop jobs, a text-to-speech script
```

`make test` runs the suite (toolchain in Docker; Go is not needed on the host).

## Licence

[MIT](LICENSE). The bundled Literata font is under the SIL Open Font License
(`internal/server/static/fonts/OFL.txt`).
