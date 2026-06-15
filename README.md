# code-remote

A small REST API (Go) that launches detached [Claude Code](https://claude.com/claude-code)
sessions inside GNU `screen` and lets you drive them remotely — plus `crctl`, a
local CLI to list/start/stop them. The API and `crctl` are dependency-free
(stdlib only); the optional `ngrok-forward` helper uses the ngrok Go SDK.

Each session is pinned to a UUID passed as both `--session-id` and
`--remote-control`, and that same UUID is the `screen` session name suffix. So
the listing can always join **screen ↔ Claude session ↔ title** and show the
live display name you set inside Claude.

## How it works

`POST /sessions` runs, roughly:

```sh
screen -dmS <prefix>-<uuid> claude --session-id <uuid> --remote-control <uuid>
```

- `<uuid>` is the Claude session id (the file `~/.claude/projects/*/<uuid>.jsonl`)
  and the Remote Control name.
- The `title` is read live from that session log's latest `custom-title` record,
  so renaming the session **inside Claude** is reflected automatically — no
  second rename needed.
- Stopping/restarting the API does **not** kill running sessions (the systemd
  unit uses `KillMode=process`); they're rediscovered via `screen -ls`.

## Requirements

- Go 1.26+
- `screen`
- `claude` (Claude Code) on the host

## Build

```sh
go build -o claude-remote-api .                       # the API server
go build -o crctl ./cmd/crctl                         # the local CLI
go build -o ngrok-forward ./cmd/ngrok-forward         # optional: ngrok tunnel (Go SDK)
```

## Install (Debian/Ubuntu)

One-liner (latest release):

```sh
curl -fsSL https://raw.githubusercontent.com/pigri/code-remote/main/install.sh | sh
```

Or grab the `.deb` from [Releases](https://github.com/pigri/code-remote/releases)
and `sudo dpkg -i code-remote_<version>_<arch>.deb`. It installs the three
binaries to `/usr/bin`, per-user systemd units to `/usr/lib/systemd/user`, and
Synapse templates + `env.example` to `/usr/share/code-remote`. `crctl` then
works immediately; the postinst prints the steps to enable the API/WAF/ngrok
services.

Build a `.deb` locally: `make deb` (output in `./dist`).

## Releasing

Push a `vX.Y.Z` tag; CI (`.github/workflows/release.yaml`) cross-builds the
binaries for amd64/arm64, packages a `.deb` and a tarball per arch, and
publishes a GitHub release with `SHA256SUMS.txt` and generated notes.

```sh
git tag v0.1.0 && git push origin v0.1.0
```

## Run the server

```sh
export CLAUDE_REMOTE_API_TOKEN=$(openssl rand -hex 24)
./claude-remote-api
```

### Server configuration (env)

| Variable | Default | Purpose |
| --- | --- | --- |
| `CLAUDE_REMOTE_API_TOKEN` | — (**required**) | Bearer token; the server refuses to start without it |
| `CLAUDE_REMOTE_API_ADDR` | `:8080` | Listen address |
| `CLAUDE_REMOTE_SESSION_PREFIX` | `pigri-dev-remote` | Screen session name prefix (ownership scope) |
| `CLAUDE_BIN` | `claude` | Path/name of the claude binary |
| `SCREEN_BIN` | `screen` | Path/name of the screen binary |
| `CLAUDE_HOME` | `~/.claude` | Where session logs (titles) are read from |
| `CLAUDE_REMOTE_LOG_FORMAT` | `text` | Audit log format: `text` (key=value) or `json` |
| `CLAUDE_REMOTE_SESSION_SYNC` | `on` | Poll the server for archived sessions and quit their screens (`off` to disable) |
| `CLAUDE_REMOTE_SYNC_INTERVAL` | `30s` | How often to reconcile (Go duration, e.g. `30s`, `1m`) |
| `CLAUDE_REMOTE_ARCHIVE_GRACE` | `15m` | A session must stay archived this long before its screen is quit (`0` = immediate) |
| `CLAUDE_REMOTE_CREDENTIALS` | `$CLAUDE_HOME/.credentials.json` | OAuth credentials file (token source for the Sessions API) |
| `CLAUDE_REMOTE_CLOUD_BASE` | `https://api.anthropic.com` | Sessions API base URL |
| `CLAUDE_REMOTE_MATCH_TITLE` | `off` | Also match by title+cwd for sessions with no bridge id (titles are mutable — opt-in) |

## API

All routes except `/healthz` require `Authorization: Bearer <token>`.

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/healthz` | Liveness (no auth) |
| `POST` | `/sessions` | Start a session → `201` |
| `GET` | `/sessions` | List running sessions |
| `GET` | `/sessions/{id}` | One session (`404` if gone) |
| `DELETE` | `/sessions/{id}` | Stop a session (`screen -X quit`) |

`{id}` is the Claude session UUID. Session shape:

```json
{
  "id": "6fd0b321-a454-4b40-9aed-131afe120d36",
  "screen": "pigri-dev-remote-6fd0b321-a454-4b40-9aed-131afe120d36",
  "title": "Synapse - platform - k8s",
  "pid": "3993347",
  "status": "Detached",
  "created_at": "06/15/2026 08:41:26 AM"
}
```

Example:

```sh
curl -s -X POST -H "Authorization: Bearer $TOKEN" localhost:8080/sessions
curl -s        -H "Authorization: Bearer $TOKEN" localhost:8080/sessions
```

Attach to a session from a shell on the host: `screen -r <screen>`.

### From iPhone / Apple Watch (Shortcuts)

You can start, list, and stop sessions from the Apple **Shortcuts** app — each is
a one-action `Get Contents of URL` call to the public endpoint with the bearer
token. See [`docs/apple-shortcut.md`](docs/apple-shortcut.md).

## Audit log

The server writes a structured audit line to **stdout** for every request —
including rejected ones — plus explicit events when a session is created or
stopped. The bearer token is **never** logged.

```
msg=request        method=POST   path=/sessions  status=201 dur_ms=18 remote=127.0.0.1 auth=ok forwarded_for=203.0.113.9
msg=request        method=GET    path=/sessions  status=401 dur_ms=0  remote=127.0.0.1 auth=denied
msg=session_create remote=127.0.0.1 id=<uuid> screen=<prefix>-<uuid>
msg=session_delete remote=127.0.0.1 id=<uuid> existed=true
```

- `auth` is `ok` / `denied` / `n/a` (the latter for the unauthenticated
  `/healthz`).
- `remote` is the TCP peer (behind ngrok + Synapse that's `127.0.0.1`);
  `forwarded_for` carries the `X-Forwarded-For` client IP when present — only as
  trustworthy as the upstream that set it.
- Set `CLAUDE_REMOTE_LOG_FORMAT=json` for one JSON object per line (log
  shippers). Under systemd the trail is captured by journald:
  `journalctl --user -u code-remote-api`.

## Session sync (auto-archive)

"Archived" is a **server-side** state — it isn't written into `~/.claude` on the
host running the screens — so the server is the only source of truth. Every
`CLAUDE_REMOTE_SYNC_INTERVAL` (default **30s**) the server polls the Anthropic
Sessions API (`GET /v1/sessions`), and for any session reporting
`session_status: archived` it quits the matching local `screen`. It only ever
touches screens this service owns (prefix-scoped); unrelated screens are never
affected. Each action is audit-logged:

```
msg=session_sync_enabled interval=30s credentials=/home/you/.claude/.credentials.json
msg=auto_archive id=<uuid> screen=<prefix>-<uuid> reason=archived_on_server
```

- A **grace period** (`CLAUDE_REMOTE_ARCHIVE_GRACE`, default **15m**) protects
  against accidental archives: a screen is only quit after its session has been
  *continuously observed* archived for that long. Unarchiving within the window
  resets the clock (the API exposes no archive timestamp, so this is clocked
  locally). A pending quit logs `archive_pending` with the time remaining.
- Auth uses the local OAuth token from `CLAUDE_REMOTE_CREDENTIALS` (the same one
  `claude` uses). The token is **never logged**, and the file is re-read each
  cycle so refreshed tokens are picked up automatically.
- It **degrades gracefully**: on a host with no credentials file (e.g. a
  headless server where you've never logged in `claude`), sync logs
  `session_sync_disabled` once and the rest of the API runs normally.
- Disable with `CLAUDE_REMOTE_SESSION_SYNC=off`.

> The server never exposes our `--session-id` UUID, so local screens are joined
> to server sessions by the registry's **`bridgeSessionId` == server `id`** — a
> stable, rename-proof key (a session's `bridgeSessionId` doesn't change when you
> rename it). Sessions that never bridged have a `null` bridge id; those are
> matched only if you opt in with `CLAUDE_REMOTE_MATCH_TITLE=on`, which falls
> back to **title + cwd** (mutable, so off by default) and only fires when
> *every* server session sharing that title+cwd is archived. The API shape
> (`session_status`, `session_context`, `bridgeSessionId`) isn't a documented
> contract and may change across versions.

## crctl (local CLI)

```sh
crctl ls            # list running sessions (default)
crctl new           # start a new session
crctl rm <id>       # stop a session
```

By default `crctl` runs **locally** — it drives `screen`/`claude` directly, with
no API process, token, or URL. Set `CLAUDE_REMOTE_API_URL` to talk to a remote
API instead (then `CLAUDE_REMOTE_API_TOKEN` is required).

| Variable | Mode | Purpose |
| --- | --- | --- |
| _(none)_ | local (default) | drive `screen`/`claude` directly on this host |
| `CLAUDE_REMOTE_API_URL` | remote | API base URL (e.g. `http://127.0.0.1:9000`) |
| `CLAUDE_REMOTE_API_TOKEN` | remote | bearer token (required when the URL is set) |
| `CLAUDE_BIN` · `SCREEN_BIN` · `CLAUDE_HOME` · `CLAUDE_REMOTE_SESSION_PREFIX` | local | optional overrides |

```
$ crctl ls
ID                                    TITLE                     STATUS    ATTACH
6fd0b321-a454-4b40-9aed-131afe120d36  Synapse - platform - k8s  Detached  screen -r pigri-dev-remote-6fd0b321-...
```

## Deploy (systemd)

A hardened system unit and env template are in [`deploy/`](deploy/):

```sh
go build -o claude-remote-api .
sudo install -m0755 claude-remote-api /usr/local/bin/

sudo install -m0600 -o "$USER" -g "$USER" deploy/claude-remote-api.env.example /etc/claude-remote-api.env
sudo sed -i "s/replace-with-a-long-random-token/$(openssl rand -hex 24)/" /etc/claude-remote-api.env

sudo install -m0644 deploy/claude-remote-api.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now claude-remote-api
```

The unit runs as your user (it needs your `claude` install, `~/.claude`
credentials, and the per-user `screen` sockets), with `ProtectSystem=strict`,
no capabilities, a syscall filter, and `ReadWritePaths` limited to `~` and
`/run/screen`. `MemoryDenyWriteExecute` is intentionally **off** — the claude
binary's JIT needs writable+executable memory.

## Remote access: ngrok + Synapse WAF

Expose the API publicly at a reserved domain with a WAF in front. ngrok
terminates TLS and forwards to a single local port (8080), where
[Synapse](https://gen0sec.com) runs as an L7 reverse proxy + WAF and proxies to
the API. The API itself binds `127.0.0.1` only — it is never directly reachable.

```
Internet
  → ngrok  https://your-domain.ngrok.dev   (TLS terminated here)
  → :8080  Synapse (WAF: SQLi/XSS/traversal/oversize/method allow-list + rate limit)
  → 127.0.0.1:9000  claude-remote-api  (bearer-token auth)
```

| Port | Bound | Role |
| --- | --- | --- |
| `your-domain.ngrok.dev:443` | ngrok edge | public HTTPS |
| `8080` | localhost | Synapse WAF (the only port ngrok forwards to) |
| `9000` | `127.0.0.1` | the API (Synapse is its only client) |

Synapse configs live in [`deploy/synapse/`](deploy/synapse/) (`config.yaml`,
`upstreams.yaml`, `security_rules.yaml`). The ngrok tunnel is the `ngrok-forward`
helper (ngrok Go SDK) — it forwards the reserved domain to Synapse, so the WAF
stays in the path.

Config goes in a gitignored `.env` (secrets + your private domain); only
`deploy/.env.example` is committed:

```sh
cp deploy/.env.example .env && "$EDITOR" .env   # tokens + NGROK_DOMAIN
set -a; . ./.env; set +a
```

Run (three processes):

```sh
# Synapse routes by Host header, so render your real domain into the
# upstreams host (the repo keeps a placeholder):
sed "s/your-domain.ngrok.dev/$NGROK_DOMAIN/" deploy/synapse/upstreams.yaml \
  | sudo tee /etc/synapse/upstreams.yaml >/dev/null

# 1) API on an internal port (Synapse is the only thing that reaches it)
./claude-remote-api &

# 2) Synapse WAF on :8080 -> API
synapse --mode proxy -c deploy/synapse/config.yaml \
        --security-rules-config deploy/synapse/security_rules.yaml &

# 3) ngrok tunnel (Go SDK). Default upstream http://localhost:8080, so
#    traffic goes THROUGH the WAF.
./ngrok-forward &
```

The WAF blocks SQLi/XSS markers, path traversal/dotfile probes, oversized POSTs,
and non-API HTTP methods, and rate-limits `/sessions`. The API's bearer token
remains the primary access control (defense in depth). Behind an ngrok HTTP
tunnel all requests arrive from the local agent, so `ip.src`/threat-intel WAF
rules see `127.0.0.1` — the content/method/path rules carry the protection.

> The Synapse configs were authored against its documented schema but not run
> here; validate them against your installed Synapse version.

### Persistent deploy (systemd --user)

For a durable, boot-surviving deploy, three `--user` units in
[`deploy/systemd/`](deploy/systemd/) run the stack (API → Synapse → ngrok),
all reading `.env` via `EnvironmentFile`:

```sh
go build -o ~/.local/share/code-remote/claude-remote-api .
go build -o ~/.local/share/code-remote/ngrok-forward ./cmd/ngrok-forward
chmod +x deploy/render-config.sh

sudo loginctl enable-linger "$USER"          # run without an active login
cp deploy/systemd/code-remote-*.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now code-remote-{api,synapse,ngrok}.service
```

The synapse unit's `ExecStartPre` runs `render-config.sh`, which substitutes
`NGROK_DOMAIN` into the upstreams host at start. Note `CLAUDE_BIN` must be an
absolute path in `.env` — systemd's PATH doesn't include `~/.local/bin`.

## Security notes

- The token is checked in constant time; the server is fail-closed (won't start
  without a token).
- The default bind is `:8080` — put it behind TLS (reverse proxy / tunnel), or
  bind `127.0.0.1`, since the bearer token rides the wire.
- The manager only ever lists/kills `screen` sessions matching the prefix, and
  session ids are UUID-validated, so it can't touch unrelated screens.

## Caveat

The `title` is read from Claude's internal `~/.claude/.../<id>.jsonl`
(`type:custom-title`) format, which is **not a stable public API** and may
change across Claude versions. It's best-effort; everything else works without
it.
