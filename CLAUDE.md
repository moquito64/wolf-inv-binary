# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository overview

`wolf-inv` (module name `wolf-inv`, binary name `wacinv`) is a terminal UI client for a server inventory API, built with Bubble Tea/Bubbles/Lipgloss. This directory is its own **nested git repository** — it has its own `.git`, `.gitignore`, and `.github/workflows/release.yml` — living inside the parent `wolf-inv` repo but with independent history.

It is the client half of a client/server pair. The API it actually talks to in production is a separate repo at `/home/wolf/serverless-inv-api` (Vercel + Neon Postgres, `api/index.py`) — see that repo's own `CLAUDE.md` for its architecture. The Flask `server.py` one directory up in `wolf-inv/` is an earlier file-backed version of the same API and is not what this client is currently configured against; don't assume its behavior (e.g. lack of auth enforcement) applies to the real backend.

All application code is in the single file `wolf-inv.go`. `wolf-inv.old` is a stale previous version kept for reference only — never edit it or treat it as current behavior.

## Common commands

```bash
go build -o wacinv .   # build the wacinv binary
go run .               # run directly
go vet ./...           # closest thing to a lint check available
```

There is no test suite. Releases are cut by pushing a `v*` tag, which triggers `.github/workflows/release.yml` to cross-compile (linux amd64/arm/arm64, darwin arm64) and publish a GitHub release with assets named `wacinv-<goos>-<goarch>`.

### Configuration

The app requires a config file at `~/.config/wolf-inv/config.json` (the directory is auto-created by `loadConfig`, but the file itself must be supplied by the user), shaped like `config.example.json`:
```json
{ "apiBaseURL": "https://api-endpoint-url-goes-here/api", "apiToken": "secret-api-token-goes-here" }
```
`config.json` in this working copy contains a real endpoint/token and is gitignored — never print, log, or commit its contents, and don't propagate real values into examples or commits.

## Architecture notes

- **Single Bubble Tea `model`/`Update`/`View` state machine.** Everything lives in `wolf-inv.go`; there is no separate networking/service layer or package split to look for.
- **Top-level state** is a `State` enum: `Viewing`, `Adding`, `Editing`, `Deleting`, `Help`. `Update` dispatches to `updateViewing`/`updateAddingEditing`/`updateDeleting`/`updateHelp` based on it.
- **Add/Edit is a sub-state machine.** Within `Adding`/`Editing`, an `AddingState` enum (`InputName` → `InputIP` → `InputLocation` → `InputStatus` → `Confirm`) steps a single shared `textinput` (name/IP/location) and `statusList` (status picker) through a 4-field form before submitting on confirm.
- **All HTTP calls are `tea.Cmd`s** (`fetchServers`, `addOrEditServer`, `deleteServer`) that hit `GET /inventory`, `POST /report`, and `DELETE /delete/<name>` respectively, each sending `Authorization: Bearer <apiToken>`. They return typed messages (`serverMsg`, `errMsg`, `fetchServersMsg`) that `Update` consumes — this is the only way results flow back into the model.
- **Polling**: `pollForUpdates(30*time.Second)` is batched into `Init` alongside the initial fetch, so the table refreshes automatically every 30s in addition to manual refresh (`r` key).
- **Auth**: the client sends the Bearer token on every request. The real backend (`serverless-inv-api`) only actually validates it on `GET /api/inventory`; `POST /api/report` and `DELETE /api/delete/<name>` are currently unauthenticated server-side — that's a backend concern, not something to fix here.
- Transient status messages (success/cancel/error) are shown via `setTempMessage`, which sets a style + text and arms a 2s `time.Timer` that sends a `clearMessage` back through the running `tea.Program` (`p`, a package-level var) to reset it.
