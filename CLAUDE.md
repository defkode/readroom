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
  Port 8080 is taken on the owner's machine.
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

The repository is public. The specifics of the owner's installation — addresses, the deployed
version, which model summarises — belong in their private runbook
(`~/Documents/qnap-databox-runbook.md`, section "Readroom"), not in this repository. Update it there
after a deployment.

1. Push to `main` (when asked). Unless only documentation or `deploy/` changed,
   `.github/workflows/image.yml` runs the tests and publishes
   `ghcr.io/defkode/readroom:latest` plus `:sha-<commit>`; wait for the run to go green
   (`gh run watch`).
2. Container Station → Applications → `readroom` → recreate with `deploy/qnap-compose.yaml`, which
   pulls the new image. Replace `CHANGE_ME` with the real token in the editor — the owner pastes it;
   it lives in their password manager and must never be committed or echoed.
3. Check `https://readroom.<tailnet>.ts.net/healthz`.

Without the registry: `make image-qnap` → `dist/readroom-arm64.tar`, import it under Container Station
→ Images, and run it with `image: readroom:latest` and `pull_policy: never`. Container Station rejects
the buildx OCI layout ("invalid file format"); the target converts to the classic `docker save` layout.

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

The owner runs it from a systemd user timer on their desktop (`readroom-summarize.timer`, units and
setup in `deploy/systemd/`). After changing the summariser, reinstall the binary there
(`install -D bin/readroom ~/.local/bin/readroom`). Check it with
`systemctl --user status readroom-summarize` and `journalctl --user -u readroom-summarize`.
