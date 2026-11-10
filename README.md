# Readroom

A small self-hosted RSS/Atom reader. One static Go binary, one SQLite file.

- Web app for phone, tablet and desktop (installable: "Add to Home Screen" / "Install app")
- REST API for everything the UI does — summaries, read/starred, readlists, exports
- Per-feed settings: name, category, and whether to keep full local copies of articles (and since when)
- Preserved articles (text + images) live inside the database, so they survive the original going away
- Export an article or a readlist as EPUB or Markdown; download the whole collection as one `.db` file

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
| `READROOM_INTERVAL` | `30m` | how often each feed is polled |
| `READROOM_LOG` | `info` | `debug` logs every request |

For the QNAP (arm64) or Proxmox (amd64), `make image-multiarch` writes loadable image archives to `dist/`.

## API

All under `/api/v1`, JSON, `Authorization: Bearer $READROOM_TOKEN`.

| | |
|---|---|
| `GET /feeds` · `POST /feeds` | list · add `{url, title?, category?, preserve?, preserve_since?}` |
| `GET/PATCH/DELETE /feeds/{id}` | `PATCH` takes any of `title`, `category`, `url`, `preserve`, `preserve_since` (`"2026-10-01"` or `null`) |
| `POST /feeds/{id}/refresh` | fetch now |
| `GET /entries` | filters: `feed_id`, `category`, `readlist_id`, `read`, `starred`, `preserved`, `summary=missing\|present`, `q`, `order=asc`, `limit`; page with `before=<next>` |
| `GET /entries/{id}` | includes `content` and `preserved_content` |
| `PATCH /entries/{id}` | any of `read`, `starred`, `summary`, `summary_model` |
| `POST /entries/{id}/preserve` | keep a local copy of this one article |
| `POST /entries/mark-read` | optional `{feed_id}` or `{category}` |
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

The server never calls a model. `readroom summarize` is an ordinary API client that fills in missing summaries:

```sh
export READROOM_TOKEN=…
readroom summarize -llm http://127.0.0.1:8080/v1 -model qwen3.6-fast   # any OpenAI-compatible server
readroom summarize -backend claude                                     # claude CLI, Haiku, tools disabled
```

Run it from cron or a systemd timer on the machine that has the model; `-api` points it at the NAS.

## Layout

```
cmd/readroom        entry point: serve, summarize
internal/store      SQLite schema and queries
internal/fetch      feed polling, article preservation
internal/export     EPUB and Markdown
internal/server     REST API, web app (templates/, static/)
```

`make test` runs the suite (toolchain in Docker; Go is not needed on the host).
