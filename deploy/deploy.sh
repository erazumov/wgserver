#!/usr/bin/env bash
# deploy.sh — one-shot deployment of wgserver to a remote host.
#
# Usage:
#   ./deploy/deploy.sh                    # reads deploy.env from same dir
#   ./deploy/deploy.sh --env prod.env     # custom env file
#   ./deploy/deploy.sh --dry-run          # print what would happen, do nothing
#
# Required in env (file or shell):
#   DEPLOY_HOST                  ssh target, e.g. root@taigaproxy
#   WGSERVER_EXIT_WG_ENDPOINT    host:port of the upstream WireGuard
#   WGSERVER_EXIT_WG_PUBKEY      base64 public key of the upstream peer
#   WGSERVER_TG_BOT_TOKEN        Telegram bot token (can be empty to disable bot)
#   WGSERVER_TG_CHAT_ID          group chat id (0 to disable bot)
# Optional:
#   WGSERVER_TG_QUOTA            per-user quota (default 2)
#   WGSERVER_LISTEN_ADDR         admin UI listen (default 127.0.0.1:8080)
#   WGSERVER_HEALTH_ADDR         /healthz listen (default 127.0.0.1:9090)
#   VERSION                      override version string (default: git describe)
#   REMOTE_TMP                   remote staging dir (default /tmp)
#   ENABLE_UPDATER               1 to install wgserver-updater + timer (default 1)
#
# This script is safe to re-run: install.sh is idempotent, the binaries
# are re-uploaded, the service is restarted. Existing /etc/wgserver/*
# and /etc/wireguard/* are preserved.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarn:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mfatal:\033[0m %s\n' "$*" >&2; exit 1; }

# ---- flags ----
ENV_FILE="$SCRIPT_DIR/deploy.env"
DRY_RUN=0
while [ $# -gt 0 ]; do
  case "$1" in
    --env)     ENV_FILE="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help)
      cat <<USAGE
Usage: deploy.sh [flags]

  --env FILE     path to env file (default: $SCRIPT_DIR/deploy.env)
  --dry-run      print what would happen, do nothing
  -h, --help     show this help

Reads deploy.env (or --env FILE) and uploads+installs wgserver on
the target host in one shot. Re-runnable; preserves /etc/wgserver/
and /etc/wireguard/.
USAGE
      exit 0
      ;;
    *)         die "unknown flag: $1 (use --help)" ;;
  esac
done

# ---- env file ----
if [ -f "$ENV_FILE" ]; then
  log "loading env from $ENV_FILE"
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
else
  warn "no env file at $ENV_FILE (using shell env)"
fi

# ---- prerequisites ----
command -v go  >/dev/null || die "go not in PATH"
command -v ssh >/dev/null || die "ssh not in PATH"
command -v scp >/dev/null || die "scp not in PATH"

# ---- required inputs ----
: "${DEPLOY_HOST:?DEPLOY_HOST is required (e.g. root@taigaproxy)}"
: "${WGSERVER_EXIT_WG_ENDPOINT:?WGSERVER_EXIT_WG_ENDPOINT is required}"
: "${WGSERVER_EXIT_WG_PUBKEY:?WGSERVER_EXIT_WG_PUBKEY is required}"
: "${WGSERVER_TG_BOT_TOKEN:=}"
: "${WGSERVER_TG_CHAT_ID:=0}"
: "${WGSERVER_TG_QUOTA:=2}"
: "${WGSERVER_LISTEN_ADDR:=127.0.0.1:8080}"
: "${WGSERVER_HEALTH_ADDR:=127.0.0.1:9090}"
: "${REMOTE_TMP:=/tmp}"
: "${ENABLE_UPDATER:=1}"

# ---- ssh access ----
log "checking ssh access to $DEPLOY_HOST"
if [ "$DRY_RUN" = "0" ]; then
  ssh -o BatchMode=yes -o ConnectTimeout=5 "$DEPLOY_HOST" true \
    || die "ssh $DEPLOY_HOST failed (no passwordless auth — add your pubkey to ~/.ssh/authorized_keys on the host)"
fi

# ---- version ----
if [ -z "${VERSION:-}" ]; then
  if command -v git >/dev/null && git -C "$REPO_ROOT" rev-parse --git-dir >/dev/null 2>&1; then
    VERSION=$(git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)
  else
    VERSION=dev
  fi
fi
log "version: $VERSION"

# ---- build ----
BIN_DIR="$REPO_ROOT/bin"
mkdir -p "$BIN_DIR"
LDFLAGS="-s -w -X main.version=$VERSION"

run() {
  if [ "$DRY_RUN" = "1" ]; then
    printf '  [dry-run] %s\n' "$*"
  else
    "$@"
  fi
}

log "building linux/amd64 binaries"
run env GOOS=linux GOARCH=amd64 go build -ldflags="$LDFLAGS" \
  -o "$BIN_DIR/wgserver-linux-amd64" ./cmd/wgserver
run env GOOS=linux GOARCH=amd64 go build -ldflags="$LDFLAGS" \
  -o "$BIN_DIR/wgserver-updater-linux-amd64" ./cmd/wgserver-updater

# ---- upload ----
log "uploading binaries + install.sh to $DEPLOY_HOST:$REMOTE_TMP"
run scp -q \
  "$BIN_DIR/wgserver-linux-amd64" \
  "$BIN_DIR/wgserver-updater-linux-amd64" \
  "$REPO_ROOT/deploy/install.sh" \
  "$DEPLOY_HOST:$REMOTE_TMP/"

# ---- install (server-side) ----
# Multiline env-via-ssh-string is fragile: different shells on the
# remote side parse newlines in a single ssh command argument
# differently, and install.sh then sees an empty WGSERVER_EXIT_WG_*
# even though we set it. Stash the env in a file on the server and
# source it. Works with any login shell (bash, zsh, dash).
log "uploading deploy.env to $DEPLOY_HOST:$REMOTE_TMP/"
run scp -q "$ENV_FILE" "$DEPLOY_HOST:$REMOTE_TMP/deploy.env"
log "running install.sh on $DEPLOY_HOST"
run ssh "$DEPLOY_HOST" "set -a && . $REMOTE_TMP/deploy.env && set +a && bash $REMOTE_TMP/install.sh $REMOTE_TMP/wgserver-linux-amd64"

# ---- updater ----
if [ "$ENABLE_UPDATER" = "1" ]; then
  log "installing wgserver-updater (binary + systemd unit + timer)"
  run ssh "$DEPLOY_HOST" "install -m 0755 $REMOTE_TMP/wgserver-updater-linux-amd64 /usr/local/bin/wgserver-updater"
  run scp -q \
    "$REPO_ROOT/deploy/systemd/wgserver-updater.service" \
    "$REPO_ROOT/deploy/systemd/wgserver-updater.timer" \
    "$DEPLOY_HOST:/etc/systemd/system/"
  run ssh "$DEPLOY_HOST" "systemctl daemon-reload && \
                          systemctl enable --now wgserver-updater.timer && \
                          systemctl restart wgserver-updater.service"
fi

# ---- verify ----
log "verifying /healthz on $DEPLOY_HOST"
HEALTH_URL="http://${WGSERVER_HEALTH_ADDR}/healthz"
ok=0
for i in $(seq 1 15); do
  if [ "$DRY_RUN" = "1" ]; then
    ok=1
    break
  fi
  if ssh "$DEPLOY_HOST" "curl -fsS --max-time 2 $HEALTH_URL >/dev/null 2>&1"; then
    ok=1
    log "/healthz OK (attempt $i)"
    break
  fi
  sleep 1
done
[ "$ok" = "1" ] || die "/healthz never came up. check: ssh $DEPLOY_HOST 'journalctl -u wgserver -n 100'"

# ---- summary ----
cat <<EOF

\033[1;32m== deploy complete ==\033[0m

  target:    $DEPLOY_HOST
  version:   $VERSION
  admin:     http://$WGSERVER_LISTEN_ADDR/admin/login
  healthz:   $HEALTH_URL
  tg group:  ${WGSERVER_TG_CHAT_ID:-(bot disabled)}
  updater:   $([ "$ENABLE_UPDATER" = "1" ] && echo enabled || echo disabled)

  Next:
    ssh $DEPLOY_HOST 'systemctl status wgserver wgserver-updater'
    ssh $DEPLOY_HOST 'journalctl -u wgserver -u wgserver-updater -f'
    In Telegram: send /start in the configured group to test the bot.
    Create first admin:
      ssh $DEPLOY_HOST '/usr/local/bin/wgserver create-admin -username <name>'
EOF
