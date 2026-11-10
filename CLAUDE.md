# Working on Readroom

Readroom is the owner's personal self-hosted RSS/Atom reader: one Go binary, one SQLite file.
**Read `docs/SPEC.md` first** — it holds what the app does, every design decision with its reason, the
known limitations and the backlog. `README.md` is the quick start and API reference. Keep both current
when behaviour changes; add new decisions to the table in SPEC §8.

## How to work here

- **Go may not be installed on the host.** Every `make` target runs the toolchain in Docker
  (`golang:1.27-alpine`, caches in `~/.cache/readroom-go`). Use `make build`, `make test`, `make vet`.
- **Preview locally, don't deploy.** During the testing phase the owner wants to see changes on a
  locally running server, and wants commits, pushes and NAS deployments only when asked.
  `make run` serves on `127.0.0.1:8090` with `./readroom.db` (git-ignored) and no token.
  Port 8080 on the owner's machine belongs to a local LLM server.
- **Everything must be embedded.** No fonts, scripts or styles from other hosts — the app has to work
  on a local network with no internet. A CSP header and a test enforce this; don't weaken either.
- **The UI is a client of the REST API.** New behaviour goes into the API first; the web script calls
  it. No frontend framework, no build step.
- **Check the UI in a browser** at phone and desktop widths, in light, sepia and dark, before calling a
  visual change done. The owner reads mostly on a phone and found high-contrast colours tiring.
- Schema changes are forward-only migrations appended to `migrations` in `internal/store/store.go`.
  Derived columns are recomputed by `backfillText` when a migration resets `text_length` to -1.
- The owner writes in Polish; code, comments, commit messages and these documents are in English.

## Deployment (QNAP, when asked)

1. `make image-qnap` → `dist/readroom-arm64.tar`. Container Station rejects the buildx OCI layout
   ("invalid file format"); this target converts to the classic `docker save` layout with skopeo.
2. Copy the archive to a NAS share, then Container Station → Images → Import (from the local device).
3. Applications → `readroom` → recreate, pasting `deploy/qnap-compose.yaml`. Replace `CHANGE_ME` with
   the real token in the editor — the owner pastes it; it lives in their password manager and must
   never be committed or echoed.
4. Check `https://readroom.<tailnet>.ts.net/healthz`, then delete the archive from the share.

Gotchas learned the hard way:

- The Tailscale container waits only **60 seconds** for its login link to be approved, then restarts
  with a new link. Take the newest link from the container log and have the owner click Connect
  immediately; a late click leaves a dead machine in the tailnet that must be removed.
- A token file written by the container is root-only on the NAS share and cannot be downloaded through
  File Station — hence `READROOM_TOKEN` in the YAML there.
- The service inside the compose network is named `app`, not `readroom`: the Tailscale container's own
  hostname is `readroom` and would shadow it.
- The owner prefers to perform Tailscale account actions (removing machines, approving logins)
  themselves unless they hand one over explicitly.

## Summaries

The server never calls a model. `readroom summarize` (or any agent) reads
`GET /api/v1/summaries/pending` and writes back with `PUT /api/v1/entries/{id}/summary`. Feeds opt in
with their "AI summary" setting. See SPEC §4.

On the owner's desktop a systemd user timer (`readroom-summarize.timer`, units in `deploy/systemd/`)
runs `~/.local/bin/readroom summarize` hourly against the local llama-server (`qwen3.6-fast`). It reads
only `READROOM_TOKEN` from `~/.config/readroom/env`. After changing the summariser, reinstall the
binary there (`install -D bin/readroom ~/.local/bin/readroom`). Check it with
`systemctl --user status readroom-summarize` and `journalctl --user -u readroom-summarize`.
