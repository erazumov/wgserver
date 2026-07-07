# wgserver

Self-hosted WireGuard server with a web admin UI, Telegram-bot user provisioning, and a single-instance auto-update flow.

## What this project is

A complete, one-binary WireGuard server that:

- exposes a small web UI where one or more **admins** can create / revoke peers and download their `.conf`;
- routes **all client traffic** through a local xray-core (VLESS Reality) client, so the box is effectively a WireGuard→VLESS gateway / proxy;
- ships a Telegram bot that hands a `.conf` to members of a configured chat/group, with a **per-Telegram-user quota** on how many configs that person can claim;
- deploys to a target host via a single bootstrap script and keeps itself on the latest GitHub release.

## Stack (decided)

- **Language:** Go (single static binary, no CGO).
- **HTTP router:** `chi` (added in `go.mod` as the only router; do not mix with echo).
- **DB:** SQLite via `modernc.org/sqlite` (pure Go, single file on disk, e.g. `/var/lib/wgserver/db.sqlite`).
- **Templates / UI:** `html/template` + vanilla CSS, HTMX-ready (CSRF meta tag is already emitted). No templ.
- **WireGuard control plane:** shelling out to `wg` / `wg-quick` (kernel module). No userspace.
- **Exit proxy:** `xray-core` (downloaded as a static binary from the official GitHub release; config at `/etc/xray/config.json` is operator-managed). xray runs as a dedicated `xray` system user. Transparent proxying from `wg0` is done with iptables `REDIRECT` (PREROUTING) + `OUTPUT` owner match, NOT with policy routing or TPROXY.
- **CSRF:** own HMAC-SHA256 implementation, ~30 LOC, no external dep.
- **Sessions:** in-memory map, lost on restart (admin must re-login after a process restart).

## Directory layout (current)

```
cmd/wgserver/                # subcommands: serve, create-admin
cmd/wgserver-updater/        # wgserver-updater binary
internal/
  config/                    # YAML loader
  store/                     # sqlite repo (no business logic). TDD.
  wg/                        # keys (Runner-abstracted), .conf generation, AddPeer/RemovePeer
  web/                       # chi router, sessions, csrf, middleware, handlers, html templates
  syncer/                    # pending_sync=1 → wg set/remove loop. TDD.
  ipam/                      # shared CIDR allocator used by web and telegram. TDD.
  telegram/                  # long-poll bot: /start in group → claim .conf via DM. TDD.
  updater/                   # github releases polling, sha256 verify, atomic swap. TDD.
  deploy/
  install.sh                 # apt deps, ip_forward, wg0 keypair + wg-quick@wg0,
                             # xray install + config validation,
                             # iptables REDIRECT (PREROUTING wg0 + OUTPUT wgserver uid),
                             # wgserver binary + service
  systemd/wgserver.service
  systemd/wgserver-updater.{service,timer}
  wgserver.env.example
```

## Key invariants (do not break)

- **Admins are separate from Telegram users.** A Telegram user claiming a config is *not* an admin. The `/admin` route group must require admin auth; the Telegram bot must not.
- **One WireGuard interface, ever.** The server has exactly one WG interface (`wg0`). All client peers live on `wg0` and are applied by the syncer via `wg set wg0 peer ...`. `wg0.conf` MUST contain zero `[Peer]` sections. Do not introduce a second WG interface — the "two-interface relay" pattern is gone.
- **xray is the exit, not WireGuard.** All egress for `wg0` clients is achieved by a single iptables NAT rule on PREROUTING: `iptables -t nat -A PREROUTING -i wg0 -p tcp -j REDIRECT --to-ports 10808` (and the matching UDP rule). xray's inbound is `dokodemo-door` on `127.0.0.1:10808` with `network: tcp,udp` and `followRedirect: true` so it reads `SO_ORIGINAL_DST` and dials the real target. There is no MASQUERADE, no policy routing, no second routing table, no `Table = off` trick — all of that was scaffolding for the WG-as-exit topology and is obsolete.
- **xray must NOT run as the wgserver user.** If it did, its own outbound VLESS connection to the remote would match the `OUTPUT -m owner --uid-owner wgserver` REDIRECT rule, xray would dial itself, and you'd get a silent infinite loop that looks like "xray is up but every connection times out". xray runs as the dedicated `xray` system user (or root) — never as `wgserver`. The systemd unit enforces this.
- **Daemon outbound (Telegram, GitHub, anything from the wgserver process) goes through xray.** The `iptables -t nat -A OUTPUT -m owner --uid-owner wgserver -p tcp -j REDIRECT --to-ports 10808` rule (and its UDP twin) catches every packet the `wgserver` uid generates and tunnels it through xray. This replaces the old uidrange ip rule. Do NOT weaken this by (a) running wgserver as root, (b) removing the owner-match rule, (c) running xray as the wgserver uid, or (d) running the bot / updater under a different uid. Failure modes are silent — the daemon appears to work but every API call leaks the host's public IP. The check is `sudo -u wgserver curl -sS https://api.telegram.org/...` and comparing the resolved / connected IP to the host's public IP via `curl https://ifconfig.io` from the host's main routing table — they MUST differ.
- **Host's own traffic (SSH, ping, apt, /healthz polling) is NOT proxied.** Root / system uids don't match `--uid-owner wgserver`, and packets from the public NIC don't match `-i wg0`, so they keep using the system's main routing table and the public NIC. This is the entire reason the wgserver daemon has its own system user.
- **wgserver runs as a dedicated system user.** `wgserver.service` is started with `User=wgserver` and `AmbientCapabilities=CAP_NET_ADMIN` (the only privilege the syncer needs to call `wg set` on `wg0`). The `wgserver` user owns `/var/lib/wgserver` (DB + CSRF key) and `/etc/wgserver/wgserver.yaml`; do not run the daemon as root — that would defeat the owner-match rule above and break the transparent proxy for the daemon's own outbound traffic.
- **xray config is operator-managed, not generated.** `/etc/xray/config.json` is hand-written by the operator (copy-paste from their xray client profile, with the inbound swapped to `dokodemo-door`). `install.sh` validates that it exists, parses as JSON, and the first inbound is `dokodemo-door` on `127.0.0.1:10808`. `install.sh` does NOT rewrite the operator's VLESS Reality secrets. To rotate VLESS credentials, edit `/etc/xray/config.json` and `systemctl restart xray` — no wgserver restart needed.
- **Per-Telegram-user quota** is a hard limit, enforced in the store layer (`store.ClaimPeerForTelegramUser` is the only path).
- **Config generation is deterministic.** `peer private key` is generated once and persisted (`store.CreatePeer` always sets `pending_sync=1`; `wg.GenerateClientConfig` re-derives the .conf from stored data). Never bake a freshly generated key into a response without saving it first.
- **The updater restarts the service, it does not run the app.** The updater binary writes the new binary to a temp path, `systemctl restart wgserver`, and rolls back on failed `/healthz`. It never execs the wgserver binary in-process.
- **Bind to a UNIX socket or localhost for the admin UI** unless TLS is configured. Do not expose the admin port to the public internet.
- **WireGuard state changes are not transactional with the DB.** Always: write to DB first, then call `wg set` / `wg-quick`. On WG failure, leave the row as `pending_sync=true` and retry — never silently desync. `internal/syncer` enforces this — failed peers stay pending across ticks.

## Development workflow (superpowers)

- Use the **superpowers** workflow for every non-trivial change: clarify intent, design the smallest interface, write a failing test or scripted check, implement, review. Do not jump straight to code.
- Before writing code, state in chat: (1) which invariant above is affected, (2) which file(s) you will touch, (3) the manual verification step. If you can't answer all three, the design isn't ready.
- TDD preferred for `internal/store`, `internal/wg`, `internal/syncer`, `internal/ipam`, `internal/telegram`, `internal/updater` — these are where the easy-to-screw-up invariants live. The HTTP layer can be tested with `net/http/httptest` + `chi.NewRouter`.
- Manual smoke test after any change touching WG state or xray routing: bring up two peers, confirm traffic from peer A reaches the internet via VLESS (`curl https://ifconfig.io` from a peered client should return the remote VLESS server's public IP, NOT the wgserver host's public IP).
- Cross-compile for deploy: `GOOS=linux GOARCH=amd64 go build -ldflags='-s -w' -o bin/wgserver-linux-amd64 ./cmd/wgserver`.

## Commands

- `make build` — produce `./bin/wgserver` (host GOOS/GOARCH)
- `make test` — `go test ./...`
- `make lint` — `gofmt -l .` + `go vet ./...`; `make golangci-lint` runs the full lint suite from `.golangci.yml` (errcheck, govet, staticcheck, revive, bodyclose, ineffassign, unused, misspell, gocritic, gosimple). Install: `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`.
- `make dev` — `go run ./cmd/wgserver -config ./wgserver.example.yaml`
- `./deploy/deploy.sh` — one-shot deploy to a remote host. Reads `deploy/deploy.env` (gitignored, copy from `deploy.env.example`). Builds linux/amd64, scp's binaries + install.sh, runs install.sh, enables wgserver-updater.timer, verifies `/healthz`. Idempotent. Run `./deploy/deploy.sh --dry-run` to preview.
- `sudo ./deploy/install.sh ./bin/wgserver-linux-amd64` — bootstrap a fresh Debian 12 host (called by deploy.sh, rarely by hand)
- `bash -n deploy/install.sh` — syntax check the installer

## Deploy

- `install.sh` is idempotent: re-running on a working host regenerates the binary, the systemd units, and the iptables rules, but preserves `/etc/wireguard/wg0.conf` (existing client .confs stay valid), `/etc/wgserver/wgserver.yaml`, and `/etc/xray/config.json` (operator-managed).
- Required on first install: `/etc/xray/config.json` must exist and have a `dokodemo-door` inbound on `127.0.0.1:10808` (see AGENTS.md invariant "xray config is operator-managed"). `install.sh` aborts with a clear error otherwise.
- Optional env vars: `WGSERVER_LISTEN_ADDR` (default 127.0.0.1:8080), `WGSERVER_HEALTH_ADDR` (default 127.0.0.1:9090; required when admin is on a UNIX socket or TLS), `WGSERVER_TG_BOT_TOKEN`, `WGSERVER_TG_CHAT_ID`, `WGSERVER_TG_QUOTA`.
- The installer does **not** bring up the `wgserver-updater` automatically — the operator opts in via `systemctl enable --now wgserver-updater.timer` after the first release tag exists. TLS and UNIX-socket configurations are supported via `wgserver.yaml`; the installer writes a default plain-HTTP localhost config.
- Releases will be cut as GitHub Releases with a versioned binary asset and corresponding git tag. The updater polls `https://api.github.com/repos/<owner>/wgserver/releases/latest`, downloads, sha256-verifies, atomically swaps, `systemctl restart wgserver`, and rolls back on failed `/healthz`. Pin the version in `/etc/wgserver/wgserver.env` (`WGSERVER_VERSION=`) to opt out.

## What's left to first usable release

- [x] Decide `chi` vs `echo` and add it to `go.mod` as the only HTTP router.
- [x] Decide `html/template`+HTMX vs `templ` and add a single empty page.
- [x] Add `Makefile` with the targets above.
- [x] Add `golangci-lint` config (or document the linter command in `AGENTS.md`).
- [x] Add `cmd/wgserver/main.go` that reads config (now with subcommand dispatch).
- [x] Add `deploy/install.sh` skeleton — full installer, see `deploy/install.sh`.
- [x] Telegram bot (long-poll, group allowlist, `/start` → claim).
- [x] Auto-updater (`wgserver-updater` binary + `.service`/`.timer`).
- [x] TLS / UNIX socket for the admin UI.
- [x] HTMX-ified create-form (csrf meta is in place, ready to wire).
- [x] Preshared key in admin-created peers.
- [x] Switch exit from WireGuard to xray-core (VLESS Reality transparent proxy via iptables REDIRECT).
