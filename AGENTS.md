# wgserver

Self-hosted WireGuard server with a web admin UI, Telegram-bot user provisioning, and a single-instance auto-update flow.

## What this project is

A complete, one-binary WireGuard server that:

- exposes a small web UI where one or more **admins** can create / revoke peers and download their `.conf`;
- routes **all client traffic** through a single upstream WireGuard peer (the "exit WG"), so the box is effectively a WireGuard-to-WireGuard relay / VPN gateway;
- ships a Telegram bot that hands a `.conf` to members of a configured chat/group, with a **per-Telegram-user quota** on how many configs that person can claim;
- deploys to a target host via a single bootstrap script and keeps itself on the latest GitHub release.

## Stack (decided)

- **Language:** Go (single static binary, no CGO).
- **HTTP router:** `chi` (added in `go.mod` as the only router; do not mix with echo).
- **DB:** SQLite via `modernc.org/sqlite` (pure Go, single file on disk, e.g. `/var/lib/wgserver/db.sqlite`).
- **Templates / UI:** `html/template` + vanilla CSS, HTMX-ready (CSRF meta tag is already emitted). No templ.
- **WireGuard control plane:** shelling out to `wg` / `wg-quick` (kernel module). No userspace.
- **Telegram:** not built yet — placeholder in `wgserver.yaml`, no Go code.
- **Auto-update:** not built yet — no `wgserver-updater.{service,timer}` yet (would crash on enable until the updater binary exists).
- **CSRF:** own HMAC-SHA256 implementation, ~30 LOC, no external dep.
- **Sessions:** in-memory map, lost on restart (admin must re-login after a process restart).

## Directory layout (current)

```
cmd/wgserver/                # subcommands: serve, create-admin
internal/
  config/                    # YAML loader
  store/                     # sqlite repo (no business logic). TDD.
  wg/                        # keys (Runner-abstracted), .conf generation, AddPeer/RemovePeer
  web/                       # chi router, sessions, csrf, middleware, handlers, html templates
  syncer/                    # pending_sync=1 → wg set/remove loop. TDD.
  ipam/                      # shared CIDR allocator used by web and telegram. TDD.
  telegram/                  # long-poll bot: /start in group → claim .conf via DM. TDD.
  deploy/
  install.sh                 # apt deps, ip_forward, wg0/wg1 keypairs, wg-quick systemd units,
                             # PBR (Table=off + table 51820 + ip rule), iptables FORWARD+MASQUERADE,
                             # wgserver binary + service
  systemd/wgserver.service
  wgserver.env.example
```

## Key invariants (do not break)

- **Admins are separate from Telegram users.** A Telegram user claiming a config is *not* an admin. The `/admin` route group must require admin auth; the Telegram bot must not.
- **Single upstream WG peer.** The server's own `wg0.conf` has exactly one `[Peer]` — the exit WG. Do **not** add per-client peers to the server's interface; per-client peers live on a separate interface (e.g. `wg1`) so the kernel can route their traffic out via `wg0`. Getting this wrong leaks traffic to the wrong place or breaks NAT. `install.sh` enforces this — `wg1.conf` has only `[Interface]`.
- **Server is a transparent gateway, not a WG client.** The host's own traffic (SSH, ping, apt, `/healthz` polling) MUST keep using the system's main routing table and the public NIC. The wg0 [Peer] section uses `AllowedIPs = 0.0.0.0/0` (so the wg crypto table accepts MASQUERADEd return packets) **with `Table = off`** — that combination is what tells `wg-quick` "accept anything cryptographically, but do not install AllowedIPs as kernel routes". A dedicated routing table (51820) holding `default dev wg0` is installed by `wg0.conf`'s `PostUp`, plus two ip rules: a source-based one (`from <wg1 subnet> table 51820 priority 100`) for forwarded client traffic, and a uidrange one (`uidrange <wgserver uid> table 51820 priority 50`) for the daemon's own outbound (e.g. the Telegram long-poll to `api.telegram.org`). Everything else (root, system uids, plain user shells = SSH/ping/apt) keeps falling through to the main table and the public default route. Never put `0.0.0.0/0` AllowedIPs without `Table = off` — that is the bug that breaks SSH/ping.
- **wgserver runs as a dedicated system user.** `wgserver.service` is started with `User=wgserver` and `AmbientCapabilities=CAP_NET_ADMIN` (the latter is the only privilege the syncer needs to call `wg set` on wg1). The `wgserver` user owns `/var/lib/wgserver` (DB + CSRF key) and `/etc/wgserver/wgserver.yaml`; do not run the daemon as root — that would defeat the uidrange rule above and break the policy route for the daemon's own outbound traffic (Telegram, future GitHub polls, etc.).
- **Per-Telegram-user quota** is a hard limit, enforced in the store layer (`store.ClaimPeerForTelegramUser` is the only path). Not yet integrated with a Telegram bot.
- **Config generation is deterministic.** `peer private key` is generated once and persisted (`store.CreatePeer` always sets `pending_sync=1`; `wg.GenerateClientConfig` re-derives the .conf from stored data). Never bake a freshly generated key into a response without saving it first.
- **The updater restarts the service, it does not run the app.** (Not built yet — when built, it must write the new binary to a temp path, `systemctl restart wgserver`, and roll back on failed health check.)
- **Bind to a UNIX socket or localhost for the admin UI** unless TLS is configured. Do not expose the admin port to the public internet.
- **WireGuard state changes are not transactional with the DB.** Always: write to DB first, then call `wg set` / `wg-quick`. On WG failure, leave the row as `pending_sync=true` and retry — never silently desync. `internal/syncer` enforces this — failed peers stay pending across ticks.

## Development workflow (superpowers)

- Use the **superpowers** workflow for every non-trivial change: clarify intent, design the smallest interface, write a failing test or scripted check, implement, review. Do not jump straight to code.
- Before writing code, state in chat: (1) which invariant above is affected, (2) which file(s) you will touch, (3) the manual verification step. If you can't answer all three, the design isn't ready.
- TDD preferred for `internal/store`, `internal/wg`, `internal/syncer` — these are where the easy-to-screw-up invariants live. The HTTP layer can be tested with `net/http/httptest` + `chi.NewRouter`.
- Manual smoke test after any change touching WG state: bring up two peers, confirm traffic from peer A reaches the internet via the upstream WG (`curl https://ifconfig.io` from a peered client).
- Cross-compile for deploy: `GOOS=linux GOARCH=amd64 go build -ldflags='-s -w' -o bin/wgserver-linux-amd64 ./cmd/wgserver`.

## Commands

- `make build` — produce `./bin/wgserver` (host GOOS/GOARCH)
- `make test` — `go test ./...`
- `make lint` — `gofmt -l .` + `go vet ./...`; `make golangci-lint` runs the full lint suite from `.golangci.yml` (errcheck, govet, staticcheck, revive, bodyclose, ineffassign, unused, misspell, gocritic, gosimple). Install: `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`.
- `make dev` — `go run ./cmd/wgserver -config ./wgserver.example.yaml`
- `./deploy/deploy.sh` — one-shot deploy to a remote host. Reads `deploy/deploy.env` (gitignored, copy from `deploy.env.example`). Builds linux/amd64, scp's binaries + install.sh, runs install.sh, enables wgserver-updater.timer, verifies `/healthz`. Idempotent. Run `./deploy/deploy.sh --dry-run` to preview.
- `sudo ./deploy/install.sh ./bin/wgserver-linux-amd64` — bootstrap a fresh Debian 12 host (called by `deploy.sh`, rarely by hand)
- `bash -n deploy/install.sh` — syntax check the installer

## Deploy

- `install.sh` is idempotent: re-running on a working host regenerates the binary, the systemd unit, and the env file, but preserves `/etc/wireguard/wg{0,1}.conf` and `/etc/wgserver/wgserver.yaml` (so existing client .confs stay valid).
- Required env vars: `WGSERVER_EXIT_WG_ENDPOINT` (host:port of the upstream WG), `WGSERVER_EXIT_WG_PUBKEY` (base64). Prompts if missing and stdin is a TTY.
- Optional: `WGSERVER_LISTEN_ADDR` (default 127.0.0.1:8080), `WGSERVER_HEALTH_ADDR` (default 127.0.0.1:9090; required when admin is on a UNIX socket or TLS), `WGSERVER_TG_BOT_TOKEN`, `WGSERVER_TG_CHAT_ID`, `WGSERVER_TG_QUOTA`.
- The installer does **not** bring up the `wgserver-updater` (the updater systemd unit is installed but the timer is not auto-enabled — the operator opts in after the first release tag exists). TLS and UNIX-socket configurations are supported via `wgserver.yaml`; the installer writes a default plain-HTTP localhost config.
- Releases (when updater is built) will be cut as GitHub Releases with a versioned binary asset and corresponding git tag. The updater polls `https://api.github.com/repos/<owner>/wgserver/releases/latest`, downloads, sha256-verifies, atomically swaps, `systemctl restart wgserver`, and rolls back on failed `/healthz`. Pin the version in `/etc/wgserver/wgserver.env` (`WGSERVER_VERSION=`) to opt out.

## What's left to first usable release

- [x] Decide `chi` vs `echo` and add it to `go.mod` as the only HTTP router.
- [x] Decide `html/template`+HTMX vs `templ` and add a single empty page.
- [x] Add `Makefile` with the targets above.
- [x] Add `golangci-lint` config (or document the linter command in `AGENTS.md`).
- [x] Add `cmd/wgserver/main.go` that reads config (now with subcommand dispatch).
- [x] Add `deploy/install.sh` skeleton — now full installer, see `deploy/install.sh`.
- [x] Telegram bot (long-poll, group allowlist, `/start` → claim).
- [x] Auto-updater (`wgserver-updater` binary + `.service`/`.timer`).
- [x] TLS / UNIX socket for the admin UI.
- [x] HTMX-ified create-form (csrf meta is in place, ready to wire).
- [x] Preshared key in admin-created peers.
