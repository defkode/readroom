# Readroom

A small self-hosted RSS/Atom reader. One static Go binary, one SQLite file.

- Web app for phone, tablet and desktop (installable: "Add to Home Screen" / "Install app")
- REST API for everything the UI does — summaries, read/starred, readlists, exports
- AI summaries, articles read aloud and articles translated into your own language, made by models
  you run yourself on another computer — the server stays small enough for a NAS or a Raspberry Pi
- Save any single article by its address — from a field in the app, or by sharing a page to the installed app on Android
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
| `READROOM_LANGUAGE` | *(none)* | your own language, e.g. `pl`: lets an article be [translated](#translation) into it on request. Optional; without it nothing about translation appears |

For Proxmox (amd64), `make image-multiarch` writes loadable image archives to `dist/`.
For the QNAP, every push to `main` publishes `ghcr.io/defkode/readroom:latest` (arm64 and amd64) from
`.github/workflows/image.yml`, and `deploy/qnap-compose.yaml` is the application definition that pulls
it (Readroom behind its own Tailscale HTTPS node). `make image-qnap` still builds an arm64 archive
that Container Station can import, for when the registry is out of reach.

## The worker: summaries, listening and translation

The server can sit on something very small because the models run elsewhere: it only keeps queues —
articles waiting for a summary, articles asked to be read aloud, articles asked for in your own
language. `readroom worker` is that elsewhere in one piece. Install it on a computer that has the
models (a desktop, usually), and it keeps the queues empty whenever that computer is on. Each part is
optional.

**1. Download the binary** and put it somewhere on your `PATH`. It is the same single `readroom`
file as the server, with nothing else to install; `~/.local/bin` is the usual place for a program of
your own. On Linux or macOS, paste this into a terminal:

```sh
mkdir -p ~/.local/bin
os=$(uname -s | tr A-Z a-z); arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fL -o ~/.local/bin/readroom \
  "https://github.com/defkode/readroom/releases/latest/download/readroom-$os-$arch"
chmod +x ~/.local/bin/readroom
readroom version
```

If the last line says `command not found`, `~/.local/bin` is not on your `PATH`: add
`export PATH="$HOME/.local/bin:$PATH"` to `~/.bashrc` or `~/.zshrc` and open a new terminal.
The files can also be picked by hand from the
[latest release](https://github.com/defkode/readroom/releases/latest): `readroom-linux-amd64` (most
PCs), `readroom-linux-arm64` (Raspberry Pi and other ARM), `readroom-darwin-arm64` (Apple silicon),
`readroom-darwin-amd64` (Intel Mac). Running the same commands again updates it.

To build it yourself instead: clone this repository and run `make install` (needs Docker, not Go),
which puts `readroom` and the bundled text-to-speech script in `~/.local/bin`, and restarts the worker
if it is already running.

**2. Have something to do the work.**

- For summaries: a model behind an OpenAI-compatible address — [Ollama](https://ollama.com),
  llama.cpp's `llama-server`, LM Studio, a hosted service — or the `claude` CLI, or any command that
  takes a prompt and prints an answer.
- For translation: the same kind of model as for summaries, but one that writes your language well.
- For listening: `ffmpeg`, and a text-to-speech command — see [Listening](#listening) for using your
  own. The bundled one, `tiny-voca-speak` (English), needs only [uv](https://docs.astral.sh/uv/) and
  downloads its model (27 MB) on first use. It is a second file to put beside the first:

  ```sh
  curl -fL -o ~/.local/bin/tiny-voca-speak \
    https://github.com/defkode/readroom/releases/latest/download/tiny-voca-speak
  chmod +x ~/.local/bin/tiny-voca-speak
  ```

**3. Write the configuration.** `readroom worker init` creates `~/.config/readroom/worker.toml` with
every setting explained; edit it. The token is the server's `READROOM_TOKEN`.

```toml
server = "https://readroom.example.ts.net"
token_file = "~/.config/readroom/token"   # or token = "…"

[summaries]                               # remove a section to leave that work to someone else
api = "http://127.0.0.1:11434/v1"         # any OpenAI-compatible server: Ollama, llama.cpp, LM Studio…
model = "llama3.2"
# backend = "claude"                      # or the claude CLI (model = "haiku"), tools switched off
# command = "llm -m gpt-5-mini"           # or any command: prompt in, summary out
# every = "5m"

# [translations]                          # same settings as [summaries]; into the server's READROOM_LANGUAGE
# api = "http://127.0.0.1:11434/v1"
# model = "llama3.2"

[speech]
command = "tiny-voca-speak"               # any command: text in, audio out
voice = "tiny-voca M2"
# type = "audio/ogg"                      # upload as it is, without ffmpeg
# every = "1m"
```

**4. Try it.** `readroom worker check` tries every part once and changes nothing on the server:

```
ok    server       https://readroom.example.ts.net answers and accepts the token
ok    summaries    llama3.2 in 3.0s: The harbour board voted against replacing the lamp…
ok    translations llama3.2 into Polish in 2.1s: Latarnia morska zachowuje swoją lampę / …
ok    speech       9 kB of audio/mpeg in 0.4s
```

**5. Leave it running.** `readroom worker install` sets it up as a systemd user service that starts
with your session. From then on:

```sh
systemctl --user status readroom-worker       # is it running
journalctl --user -u readroom-worker -f       # what it did
systemctl --user restart readroom-worker      # after changing worker.toml
readroom worker uninstall                     # stop and remove it
```

Things to know:

- `install` writes the service with the `PATH` of the shell it is run from, so the commands found
  there are found by the service too. Run it again after moving the binary or changing your `PATH`.
- It checks each queue as often as `every` says and works until the queue is empty. When something is
  out of reach — the server, a model not started yet — it says so once and keeps trying.
- Where there is no systemd (macOS, for one), start `readroom worker` by whatever runs programs in the
  background there, or run `readroom worker -once` from a scheduler.
- Run one worker per server: the queues have no locking, so two would do the same work twice.
- To update: download the binary again (step 1), then `systemctl --user restart readroom-worker`.

## API

All under `/api/v1`, JSON, `Authorization: Bearer $READROOM_TOKEN`.

| | |
|---|---|
| `GET /feeds` · `POST /feeds` | list · add `{url, title?, category?, preserve?, preserve_since?, ai_summary?, keep_days?, unread?}` (`ai_summary` is on unless sent as `false`; `unread`: how much of the backlog starts unread — `recent` by default, `latest`, `all` or `none`); `url` may be a feed or a page that leads to one, and `422` explains an address that is neither. Feeds report their health (`fail_count`, `failing_since`, `last_ok_at`, `broken`, `stale`) and `teaser_only`, set when their articles are far longer on the site than in the feed |
| `GET/PATCH/DELETE /feeds/{id}` | `PATCH` takes any of `title`, `category`, `url`, `preserve`, `preserve_since` (`"2026-10-01"` or `null`), `ai_summary`, `keep_days` (default 365; `0` keeps articles forever) |
| `POST /feeds/{id}/refresh` | fetch now |
| `POST /articles` | `{url}` — save the article at an address, no feed needed: the page is read as an anonymous visitor would get it (text hidden by a style included), and stored with its images under the built-in "Saved articles" feed (`saved: true` in `GET /feeds`). Answers `201` with `{created: true, entry}`, `200` with `created: false` when that address is already saved, `422` with the reason when no article could be read |
| `GET /entries` | hidden articles are left out unless `hidden=true` asks for them instead; filters: `feed_id`, `category`, `readlist_id`, `read`, `starred`, `preserved`, `summary=missing\|present`, `q`, `order=asc` or `order=arrival` (newest arrivals first), `limit`; page with `before=<next>` |
| `GET /entries/{id}` | includes `content` and `preserved_content`; every entry carries `reading_minutes`, an estimate from its text length |
| `PATCH /entries/{id}` | any of `read`, `starred`, `hidden`, `summary`, `summary_model`, `position` (how far it has been read, as a share of its text from `0` to `1`; `GET /entries?reading=true` lists the articles begun and not finished) |
| `GET /summaries/pending` | work queue for a summarising script or agent — see below; `limit`, `max_chars`, `feed_id` |
| `PUT /entries/{id}/summary` | `{summary, model}`; `DELETE` removes it and re-queues the article |
| `POST /entries/{id}/audio` | ask for the article to be read aloud; the entry's `audio` is then `queued`, later `ready` or `failed` |
| `GET /audio/pending` | work queue for a text-to-speech client — see below; `limit`, `max_chars`, `exclude` |
| `PUT /entries/{id}/audio?voice=…` | the recording itself as the body, with its `audio/*` content type; `GET` plays it back (byte ranges supported), `DELETE` removes it or withdraws the request |
| `POST /entries/{id}/audio/error` | `{error}` — a client reporting that it could not make the recording |
| `POST /entries/{id}/translation` | ask for the article to be translated into `READROOM_LANGUAGE`; the entry's `translation` is then `queued`, later `ready` or `failed` |
| `GET /translations/pending` | work queue for a translating client — see below; `limit`, `exclude` |
| `PUT /entries/{id}/translation` | `{of, pieces, title?, summary?, model?, language?}` — the translated pieces; `GET` returns the translation (`language`, `model`, `title`, `summary`, `content`), `DELETE` removes it or withdraws the request |
| `POST /entries/{id}/translation/error` | `{error}` — a client reporting that it could not translate the article |
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
readroom summarize -cmd "llm -m gpt-5-mini"                            # any command: prompt in, summary out
```

It takes one article at a time — fetch, summarise, write back — until the queue is empty or `-limit`
(default 20) is reached, so it never holds work locally and can be interrupted safely. With an empty
queue it does not touch the model at all.

A `-cmd` command gets the instructions and the article on standard input and prints the summary. The
article is untrusted text: use a command that can only write text, not one that can act on what it
reads (`-backend claude` runs the claude CLI with its tools switched off for that reason).

To have this done all the time, see [the worker](#the-worker-summaries-listening-and-translation).

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

To have this done all the time, see [the worker](#the-worker-summaries-listening-and-translation).

## Translation

Tell the server your own language — `READROOM_LANGUAGE=pl` — and every article gains a **Translate**
button. Pressing it asks for that one article in your language; the article shows that the translation
is in progress and, when it has arrived, opens in it from then on, with the original one tap away.
Without the variable nothing about translation appears anywhere. The codes it accepts:
`bg cs da de el en es et fi fr hr hu it ja ko lt lv nb nl pl pt ro sk sl sr sv tr uk zh`.

As with summaries and recordings the server translates nothing itself. `GET /api/v1/translations/pending`
returns the articles waiting, oldest request first, each cut into **pieces**: runs of text as HTML, with
their inline markup (links, emphasis) but none of the structure around them.

```json
{"entries": [{"id": 42, "language": "pl", "language_name": "Polish", "of": "3f9c0a1b77d2",
              "title": "…", "summary": "…",
              "pieces": ["First <a href=\"…\">paragraph</a>.", "A heading", "A list item", "…"]}],
 "remaining": 1, "language": "pl", "language_name": "Polish"}
```

Send them back translated, in the same order, with `PUT /api/v1/entries/42/translation`:

```json
{"of": "3f9c0a1b77d2", "model": "qwen3.6", "title": "…", "summary": "…",
 "pieces": ["Pierwszy <a href=\"…\">akapit</a>.", "Nagłówek", "Element listy", "…"]}
```

The server puts each piece where the original was, so paragraphs, lists, images and code listings stay
exactly as they are, and cleans what it is given like any other HTML from outside. `of` names the text
the pieces were cut from: if the article has changed since, the answer is `409` and it should be taken
from the queue again; the wrong number of pieces is `422`. A piece sent back empty keeps its original.
If it cannot be done, `POST /api/v1/entries/42/translation/error` with `{"error": "…"}` shows the reason
on the article, where "Try again" queues it once more.

`readroom translate` is an ordinary client of that queue, with the same choice of model as
`readroom summarize`:

```sh
readroom translate -llm http://127.0.0.1:8080/v1 -model qwen3.6-fast
readroom translate -backend claude
```

It sends the pieces to the model a dozen at a time, so that it sees some context and a long article
does not have to fit in one answer, and asks again piece by piece when an answer comes back jumbled. To
have this done all the time, add `[translations]` to [the worker](#the-worker-summaries-listening-and-translation).

## Layout

```
cmd/readroom        entry point: serve, worker, summarize, translate, speak
internal/store      SQLite schema and queries
internal/fetch      feed polling, article preservation
internal/export     EPUB and Markdown
internal/server     REST API, web app (templates/, static/)
deploy              NAS compose file, a text-to-speech script
```

`make test` runs the suite (toolchain in Docker; Go is not needed on the host).

## Licence

[MIT](LICENSE). The bundled Literata font is under the SIL Open Font License
(`internal/server/static/fonts/OFL.txt`).
