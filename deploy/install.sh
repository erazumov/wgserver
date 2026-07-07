#!/usr/bin/env bash
# install.sh — end-to-end installer for wgserver.
#
# Brings up a Debian 12 host as a WireGuard gateway whose client
# traffic is transparently proxied through a local xray-core
# (VLESS Reality) client:
#
#   * wg0       — single WireGuard interface, clients peer here
#                 (no [Peer] sections in wg0.conf; peers are applied
#                 by the syncer via `wg set wg0 peer ...`)
#   * xray      — dokodemo-door inbound on $XRAY_LISTEN_IP:$XRAY_INBOUND_PORT (default 192.0.2.1:10808)
#                 VLESS Reality outbound to the remote
#   * iptables  — REDIRECT wg0 PREROUTING (tcp/udp) + wgserver uid
#                 OUTPUT (tcp/udp) → ${XRAY_LISTEN_IP}:${XRAY_INBOUND_PORT}
#   * wgserver  — admin UI + sync-loop, runs as the wgserver system
#                 user. iptables OUTPUT rule catches its outbound
#                 (Telegram long-poll, future GitHub polls) and
#                 tunnels it through xray.
#
# Idempotent: re-running on a working host regenerates xray, the
# iptables rules, the systemd units and the binary, but preserves
# /etc/wireguard/wg0.conf (existing client .confs stay valid),
# /etc/wgserver/wgserver.yaml and /etc/xray/config.json
# (operator-managed — wgserver does not touch VLESS secrets).
#
# Usage:
#   ./install.sh /path/to/wgserver-linux-amd64
#
# Required on first install: /etc/xray/config.json (operator-managed,
# must have a dokodemo-door inbound on $XRAY_LISTEN_IP:$XRAY_INBOUND_PORT; install.sh
# validates and aborts with a clear error otherwise). See
# AGENTS.md invariant "xray config is operator-managed".
#
# Optional env vars: WGSERVER_LISTEN_ADDR, WGSERVER_HEALTH_ADDR,
#                    WGSERVER_TG_BOT_TOKEN, WGSERVER_TG_CHAT_ID,
#                    WGSERVER_TG_QUOTA, WGSERVER_XRAY_VERSION.
# Anything not in the environment is prompted for (if stdin is a TTY).

set -euo pipefail

# -----------------------------------------------------------------------------
# paths
# -----------------------------------------------------------------------------
ETC_WG=/etc/wireguard
ETC_WGSERVER=/etc/wgserver
ETC_XRAY=/etc/xray
VAR_WGSERVER=/var/lib/wgserver
XRAY_PREFIX=/usr/local/xray
BIN_PATH=/usr/local/bin/wgserver
XRAY_SYMLINK=/usr/local/bin/xray
WG_SYSTEMD_UNIT=/etc/systemd/system/wg-quick@wg0.service.d
SYSTEMD_UNIT=/etc/systemd/system/wgserver.service
XRAY_UNIT=/etc/systemd/system/xray.service
IPTABLES_UNIT=/etc/systemd/system/wgserver-iptables.service
IPTABLES_RULES=/etc/wgserver/iptables.rules
ENV_FILE="${ETC_WGSERVER}/wgserver.env"
CONF_FILE="${ETC_WGSERVER}/wgserver.yaml"
SYSCTL_FILE=/etc/sysctl.d/99-wgserver.conf
WG0_CONF="${ETC_WG}/wg0.conf"
XRAY_CONF="${ETC_XRAY}/config.json"
XRAY_INBOUND_PORT=10808
# TPROXY requires a fwmark to be set on intercepted UDP packets,
# then an ip rule that sends packets with that mark to a local-route
# table. The wg0 PostUp uses these values; they MUST be defined
# before wg0.conf is written in section 4.
TPROXY_MARK=0x1
TPROXY_TABLE=100

# Non-loopback local IP that xray listens on. 192.0.2.0/24 is
# TEST-NET-1 (RFC 5737), reserved for documentation, not routable
# on the public internet, never assigned to any real host. We use it
# instead of 127.0.0.1 to bypass xray-core's anti-loopback check in
# app/proxyman/inbound/worker.go: when xray is bound to 127.0.0.1,
# the check `w.hub.Addr().String() == dest.NetAddr()` matches for
# REDIRECT'd peer traffic (the original destination read via
# SO_ORIGINAL_DST resolves to the same IP:port as xray's listener),
# and xray drops the connection with "loopback connection detected".
# A non-loopback IP makes the inequality always hold, so the check
# passes. /etc/xray/config.json MUST set `listen` to the same value.
XRAY_LISTEN_IP=192.0.2.1

# -----------------------------------------------------------------------------
# helpers
# -----------------------------------------------------------------------------
log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarn:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mfatal:\033[0m %s\n' "$*" >&2; exit 1; }

require_root() {
  [ "$(id -u)" -eq 0 ] || die "install.sh must be run as root (sudo ./install.sh ...)"
}

is_tty() { [ -t 0 ] && [ -t 1 ]; }

prompt() {
  local _var=$1 _label=$2 _default=${3:-}
  local _current="${!_var:-}"
  local _value
  if [ -n "$_current" ]; then
    eval "$_var=\$_current"
    return
  fi
  if ! is_tty; then
    if [ -n "$_default" ]; then
      eval "$_var=\$_default"
      return
    fi
    die "missing required value: $_label (set env var or run interactively)"
  fi
  if [ -n "$_default" ]; then
    read -r -p "$_label [$_default]: " _value
    _value=${_value:-$_default}
  else
    read -r -p "$_label: " _value
  fi
  [ -n "$_value" ] || die "$_label cannot be empty"
  eval "$_var=\$_value"
}

# -----------------------------------------------------------------------------
# input
# -----------------------------------------------------------------------------
require_root

LISTEN_ADDR=${WGSERVER_LISTEN_ADDR:-127.0.0.1:8080}
HEALTH_ADDR=${WGSERVER_HEALTH_ADDR:-127.0.0.1:9090}
TG_BOT_TOKEN=${WGSERVER_TG_BOT_TOKEN:-}
TG_CHAT_ID=${WGSERVER_TG_CHAT_ID:-0}
TG_QUOTA=${WGSERVER_TG_QUOTA:-2}
XRAY_VERSION=${WGSERVER_XRAY_VERSION:-}

# WGSERVER_PUBLIC_ENDPOINT is what every client .conf will get
# written into the [Peer] Endpoint = ... line. The default
# $(hostname) is almost always wrong: on most hosts $(hostname)
# returns the short local name (e.g. "vpn-host", "vpn1") that
# does not resolve from the public internet. The operator MUST
# either set this env var to a publicly-resolvable FQDN or to the
# host's public IP literal. We do not try to autodetect the
# public IP — it's network-specific and a wrong guess silently
# ships a useless .conf to every Telegram user.
WGSERVER_PUBLIC_ENDPOINT=${WGSERVER_PUBLIC_ENDPOINT:-}

if [ -z "${1:-}" ] && [ -z "${WGSERVER_BINARY:-}" ]; then
  die "pass the wgserver binary as \$1 or set WGSERVER_BINARY"
fi
WG_BINARY=${1:-${WGSERVER_BINARY}}
[ -f "$WG_BINARY" ] || die "binary not found: $WG_BINARY"

# Resolve the [Peer] Endpoint value used in every client .conf.
# Public IP / FQDN that the *client* dials — not the listen addr
# (which is set by wg-quick on the wireguard interface directly).
#
# We die on three obviously-bad values rather than silently
# continuing with a guess:
#   1. empty                    — operator forgot to set the env var
#   2. RFC 5737 TEST-NET ranges  — these are documentation-only IPs
#                                  that do not route, and silently
#                                  produce .conf files that connect
#                                  to nowhere
#   3. $(hostname):51820 fallback — the legacy default, which only
#                                  works if the operator's hostname
#                                  is a public FQDN (almost never)
if [ -z "$WGSERVER_PUBLIC_ENDPOINT" ]; then
  die "WGSERVER_PUBLIC_ENDPOINT is not set. Refusing to install with a guess — every .conf handed out by the Telegram bot would have a wrong endpoint. Set the env var to a public IP or FQDN (e.g. 89.191.225.59:51820) and re-run."
fi
case "$WGSERVER_PUBLIC_ENDPOINT" in
  192.0.2.*|198.51.100.*|203.0.113.*)
    die "WGSERVER_PUBLIC_ENDPOINT=$WGSERVER_PUBLIC_ENDPOINT is in a TEST-NET range (RFC 5737) — these are documentation-only IPs that do not route. Set a real public IP or FQDN."
    ;;
esac
short_host=$(hostname -s 2>/dev/null || hostname)
if [ "$WGSERVER_PUBLIC_ENDPOINT" = "${short_host}:51820" ]; then
  die "WGSERVER_PUBLIC_ENDPOINT=$WGSERVER_PUBLIC_ENDPOINT is the \$(hostname):51820 fallback — likely a copy-pasted example. Set a real public IP or FQDN, not the local hostname."
fi

# -----------------------------------------------------------------------------
# 1. apt deps
# -----------------------------------------------------------------------------
log "installing apt deps (wireguard-tools, iptables, curl, ca-certificates, xz-utils, jq)"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
# curl: deploy.sh's /healthz check + xray download.
# ca-certificates: TLS to github.com / api.github.com.
# xz-utils: xray releases are distributed as .zip that contains a xz-compressed geoip.dat.
# jq: validation of /etc/xray/config.json (the operator's VLESS Reality profile).
# iptables: REDIRECT rules for transparent proxying.
# wireguard-tools: wg + wg-quick (kernel module handles the WG data plane).
# sudo: not strictly required for the wgserver daemon (it runs as a
#   dedicated user with CAP_NET_ADMIN), but the healthcheck script
#   (deploy/wgserver-healthcheck.sh) is intended to be run by an
#   operator and uses sudo internally to read iptables, wg show,
#   journalctl, systemctl — install it so the healthcheck works
#   on a fresh minimal host. We do NOT configure any sudoers
#   rules here; that's the operator's call.
apt-get install -y -qq wireguard-tools iptables curl ca-certificates xz-utils jq sudo

command -v wg >/dev/null         || die "wg not in PATH after install"
command -v wg-quick >/dev/null   || die "wg-quick not in PATH after install"
command -v iptables >/dev/null   || die "iptables not in PATH after install"
command -v jq >/dev/null         || die "jq not in PATH after install"
command -v curl >/dev/null       || die "curl not in PATH after install"
command -v xz >/dev/null         || die "xz not in PATH after install"

# -----------------------------------------------------------------------------
# 2. ip_forward
# -----------------------------------------------------------------------------
log "enabling net.ipv4.ip_forward"
mkdir -p /etc/sysctl.d
cat > "$SYSCTL_FILE" <<'EOF'
# managed by wgserver install.sh
net.ipv4.ip_forward = 1
EOF
sysctl --system >/dev/null

# -----------------------------------------------------------------------------
# 3. system users
#
# Two users, two reasons:
#   wgserver — runs the admin UI + syncer. iptables OUTPUT rule
#              matches its uid to REDIRECT its outbound (Telegram,
#              future GitHub API) into xray. MUST stay a separate
#              system user; running it as root would defeat that
#              rule. See AGENTS.md invariant.
#   xray     — runs the xray-core process. MUST NOT be the wgserver
#              uid, otherwise xray's own outbound VLESS connection
#              would match the OUTPUT rule → infinite loop
#              ("xray is up but every connection times out"). Root
#              is also fine; xray user is preferred for least
#              privilege.
# -----------------------------------------------------------------------------
if ! id -u wgserver >/dev/null 2>&1; then
  log "creating wgserver system user"
  useradd --system --no-create-home --shell /usr/sbin/nologin --user-group wgserver
fi
WGSERVER_UID=$(id -u wgserver)
log "wgserver uid = ${WGSERVER_UID}"

if ! id -u xray >/dev/null 2>&1; then
  log "creating xray system user"
  useradd --system --no-create-home --shell /usr/sbin/nologin --user-group xray
fi
XRAY_UID=$(id -u xray)
log "xray uid = ${XRAY_UID}"

if [ "$WGSERVER_UID" = "$XRAY_UID" ]; then
  die "wgserver and xray must have distinct uids (both = $WGSERVER_UID). refuse to install."
fi

# -----------------------------------------------------------------------------
# 4. wg0 keypair + wg0.conf
#
# wg0 is the SINGLE WireGuard interface. wg0.conf contains zero
# [Peer] sections — per-client peers are added by the syncer via
# `wg set wg0 peer <pubkey> allowed-ips <ip>`. The wg0 server pubkey
# here is what we hand out in every client .conf.
# -----------------------------------------------------------------------------
mkdir -p "$ETC_WG"
chmod 0700 "$ETC_WG"

write_keypair() {
  local _out=$1
  local _priv _pub
  _priv=$(wg genkey)
  _pub=$(printf '%s' "$_priv" | wg pubkey)
  printf '%s' "$_priv" > "$_out"
  chmod 0600 "$_out"
  printf '%s' "$_pub"
}

if [ -f "$WG0_CONF" ]; then
  log "wg0.conf already exists; preserving server keys"
  WG0_PRIV=$(awk '/^PrivateKey/ {print $3; exit}' "$WG0_CONF")
  if [ -z "$WG0_PRIV" ]; then
    warn "wg0.conf has no PrivateKey; generating fresh keypair (existing peers will be orphaned)"
    WG0_PUB=$(write_keypair /etc/wireguard/wg0.key)
    WG0_PRIV=$(cat /etc/wireguard/wg0.key)
  else
    WG0_PUB=$(printf '%s' "$WG0_PRIV" | wg pubkey)
  fi
else
  log "generating wg0 server keypair"
  WG0_PUB=$(write_keypair /etc/wireguard/wg0.key)
  WG0_PRIV=$(cat /etc/wireguard/wg0.key)
fi

CLIENTS_SUBNET=10.0.1.0/24   # MUST match Address = 10.0.1.1/24 below
CLIENTS_ADDR=10.0.1.1/24

# Build the new wg0.conf in a temp file, then decide whether to
# install it. This protects operator manual edits (e.g. an SSH-to-
# self exemption added to PostUp) from being silently clobbered on
# every install.sh run — the previous version is timestamped-back
# up before overwrite, and the operator can `diff` to recover their
# changes.
NEW_WG0_CONF=$(mktemp)
chmod 0600 "$NEW_WG0_CONF"
cat > "$NEW_WG0_CONF" <<EOF
# managed by wgserver install.sh
# SINGLE WireGuard interface. Per-client peers are NOT listed here
# — the wgserver sync-loop calls 'wg set wg0 peer <pubkey>
# allowed-ips <ip>' after each admin action. See AGENTS.md
# invariant: "One WireGuard interface, ever."
#
# All client traffic is transparently proxied through the local
# xray-core (VLESS Reality) via the PREROUTING REDIRECT rule in
# PostUp. There is no MASQUERADE, no policy routing and no second
# WG interface.
[Interface]
PrivateKey = ${WG0_PRIV}
ListenPort = 51820
Address = ${CLIENTS_ADDR}

PostUp =  iptables -t nat -C PREROUTING -i %i -p tcp -j DNAT --to-destination ${XRAY_LISTEN_IP}:${XRAY_INBOUND_PORT} 2>/dev/null || iptables -t nat -A PREROUTING -i %i -p tcp -j DNAT --to-destination ${XRAY_LISTEN_IP}:${XRAY_INBOUND_PORT}; iptables -t mangle -C PREROUTING -i %i -p udp -j TPROXY --tproxy-mark ${TPROXY_MARK}/${TPROXY_MARK} --on-port ${XRAY_INBOUND_PORT} --on-ip ${XRAY_LISTEN_IP} 2>/dev/null || iptables -t mangle -A PREROUTING -i %i -p udp -j TPROXY --tproxy-mark ${TPROXY_MARK}/${TPROXY_MARK} --on-port ${XRAY_INBOUND_PORT} --on-ip ${XRAY_LISTEN_IP}
PreDown = iptables -t nat -D PREROUTING -i %i -p tcp -j DNAT --to-destination ${XRAY_LISTEN_IP}:${XRAY_INBOUND_PORT} 2>/dev/null || true; iptables -t mangle -D PREROUTING -i %i -p udp -j TPROXY --tproxy-mark ${TPROXY_MARK}/${TPROXY_MARK} --on-port ${XRAY_INBOUND_PORT} --on-ip ${XRAY_LISTEN_IP} 2>/dev/null || true
EOF

if [ -f "$WG0_CONF" ] && cmp -s "$WG0_CONF" "$NEW_WG0_CONF"; then
  log "wg0.conf unchanged; skipping rewrite"
  rm -f "$NEW_WG0_CONF"
elif [ -f "$WG0_CONF" ]; then
  WG0_CONF_BACKUP="${WG0_CONF}.bak.$(date +%Y%m%d-%H%M%S)"
  cp -p "$WG0_CONF" "$WG0_CONF_BACKUP"
  log "wg0.conf regenerated; previous version backed up to $WG0_CONF_BACKUP"
  mv "$NEW_WG0_CONF" "$WG0_CONF"
else
  log "wg0.conf written (fresh install)"
  mv "$NEW_WG0_CONF" "$WG0_CONF"
fi

# -----------------------------------------------------------------------------
# 5. bring up wg0
#
# wg-quick@wg0 PostUp installs the PREROUTING REDIRECT. Safe to
# enable unconditionally — bringing up an empty [Interface] only
# adds a local interface; the default route is not touched.
#
# The `restart` (not just `enable --now`) is required on upgrade:
# enable --now is a no-op when the service is already running, so
# the OLD PostUp (with the old exit_wg PBR rules) would stay loaded
# in the kernel. restart brings wg0 down and back up with the new
# conf. Warning: drops all active client connections for ~1s.
# -----------------------------------------------------------------------------
log "enabling wg-quick@wg0"
systemctl enable --now wg-quick@wg0.service
if systemctl is-active --quiet wg-quick@wg0.service; then
  log "restarting wg-quick@wg0 to apply new PostUp"
  systemctl restart wg-quick@wg0.service
fi

# -----------------------------------------------------------------------------
# 6. install xray-core
#
# Downloaded from the official XTLS/Xray-core GitHub release
# (`Xray-linux-64.zip`). Pinned by WGSERVER_XRAY_VERSION (default:
# latest). The .zip is unpacked into /usr/local/xray/ and symlinked
# into /usr/local/bin/xray.
# -----------------------------------------------------------------------------
resolve_xray_version() {
  if [ -n "$XRAY_VERSION" ]; then
    printf '%s' "$XRAY_VERSION"
    return
  fi
  local _tag
  _tag=$(curl -fsSL https://api.github.com/repos/XTLS/Xray-core/releases/latest \
         | jq -r '.tag_name // empty')
  [ -n "$_tag" ] || die "could not resolve latest xray release tag (set WGSERVER_XRAY_VERSION)"
  printf '%s' "$_tag"
}

# Returns the currently-installed xray version, e.g. "1.8.20" (no
# leading 'v'), or empty if xray is not installed or the binary
# can't be queried. Output of `xray version` is something like
# "Xray 26.3.27 (Xray, Penetrates Everything.) ...", so awk '{print $2}'.
current_xray_version() {
  local raw
  raw=$("$XRAY_PREFIX/xray" version 2>/dev/null | head -1) || return 0
  [ -z "$raw" ] && return 0
  echo "$raw" | awk '{print $2}'
}

install_xray() {
  local _ver=$1
  local _zip="/tmp/xray-${_ver}.zip"
  log "downloading xray ${_ver}"
  curl -fsSL -o "$_zip" \
    "https://github.com/XTLS/Xray-core/releases/download/${_ver}/Xray-linux-64.zip" \
    || die "xray download failed (check network / WGSERVER_XRAY_VERSION=${_ver})"

  mkdir -p "$XRAY_PREFIX"
  unzip -oq "$_zip" -d "$XRAY_PREFIX" \
    || die "xray zip extraction failed (maybe unzip is missing — apt install unzip)"

  chmod 0755 "$XRAY_PREFIX/xray"
  ln -sf "$XRAY_PREFIX/xray" "$XRAY_SYMLINK"
  rm -f "$_zip"
  log "xray installed at $XRAY_PREFIX/xray → $XRAY_SYMLINK"
}

# Install/upgrade decision:
#   - xray missing                         → install (target = env or latest)
#   - xray installed, WGSERVER_XRAY_VERSION set → that version is target, install if differs
#   - xray installed, no WGSERVER_XRAY_VERSION → latest from GitHub is target
#   - GitHub API fails AND xray installed → warn and keep current
#   - GitHub API fails AND xray missing    → die
current=$(current_xray_version)

if [ -z "$current" ]; then
  log "xray not installed, installing"
  command -v unzip >/dev/null || apt-get install -y -qq unzip
  install_xray "$(resolve_xray_version)"
elif [ -n "$XRAY_VERSION" ]; then
  target="${XRAY_VERSION#v}"
  if [ "$current" = "$target" ]; then
    log "xray already at requested version $current, skipping"
  else
    log "xray upgrade: $current → $target"
    command -v unzip >/dev/null || apt-get install -y -qq unzip
    install_xray "v$target"
  fi
else
  latest_tag=$(curl -fsSL https://api.github.com/repos/XTLS/Xray-core/releases/latest \
               | jq -r '.tag_name // empty') || latest_tag=""
  if [ -z "$latest_tag" ]; then
    warn "could not query GitHub for latest xray release; staying on current version $current"
  else
    target="${latest_tag#v}"
    if [ "$current" = "$target" ]; then
      log "xray already at latest version $current, skipping"
    else
      log "xray upgrade: $current → $target (latest from GitHub)"
      command -v unzip >/dev/null || apt-get install -y -qq unzip
      install_xray "v$target"
    fi
  fi
fi

# -----------------------------------------------------------------------------
# 7. xray config
#
# /etc/xray/config.json is operator-managed. wgserver does NOT
# rewrite the operator's VLESS Reality secrets — see AGENTS.md
# invariant "xray config is operator-managed". The only thing
# install.sh does is validate that the file exists, parses as
# JSON, and the first inbound is dokodemo-door on
# ${XRAY_LISTEN_IP}:${XRAY_INBOUND_PORT} (the port the iptables DNAT/TPROXY rewrites dst to).
# -----------------------------------------------------------------------------
if [ ! -f "$XRAY_CONF" ]; then
  die "/etc/xray/config.json not found. The operator must write it BEFORE running install.sh. Minimal example:
    {
      \"log\": { \"loglevel\": \"warning\" },
      \"inbounds\": [{
        \"listen\": \"${XRAY_LISTEN_IP}\", \"port\": ${XRAY_INBOUND_PORT},
        \"protocol\": \"dokodemo-door\",
        \"settings\": { \"network\": \"tcp,udp\", \"followRedirect\": true },
        \"tag\": \"transparent\"
      }],
      \"outbounds\": [{
        \"protocol\": \"vless\",
        \"settings\": {
          \"vnext\": [{
            \"address\": \"<VLESS_SERVER_HOST>\", \"port\": 443,
            \"users\": [{ \"id\": \"<UUID>\", \"encryption\": \"none\", \"flow\": \"xtls-rprx-vision\" }]
          }]
        },
        \"streamSettings\": {
          \"network\": \"tcp\", \"security\": \"reality\",
          \"realitySettings\": {
            \"serverName\": \"<SNI>\", \"fingerprint\": \"chrome\",
            \"shortId\": \"\", \"publicKey\": \"\"
          }
        }
      }]
    }"
fi

if ! jq -e . "$XRAY_CONF" >/dev/null 2>&1; then
  die "$XRAY_CONF is not valid JSON"
fi

# Validate first inbound. The transparent-proxy REDIRECT only works
# with dokodemo-door; socks/mixed/http require a protocol handshake
# that the REDIRECTed packet never sends.
FIRST_INBOUND_PROTO=$(jq -r '.inbounds[0].protocol // ""' "$XRAY_CONF")
FIRST_INBOUND_LISTEN=$(jq -r '.inbounds[0].listen // ""' "$XRAY_CONF")
FIRST_INBOUND_PORT=$(jq -r '.inbounds[0].port // ""' "$XRAY_CONF")

[ "$FIRST_INBOUND_PROTO" = "dokodemo-door" ] \
  || die "first inbound protocol must be \"dokodemo-door\" (got \"$FIRST_INBOUND_PROTO\"). SOCKS / mixed / http do not work with iptables REDIRECT." \
       "Fix: jq '.inbounds[0] |= (.protocol = \"dokodemo-door\" | .settings = {\"network\": \"tcp,udp\", \"followRedirect\": true})' $XRAY_CONF > /tmp/xc && mv /tmp/xc $XRAY_CONF && chown root:xray $XRAY_CONF && chmod 0640 $XRAY_CONF && systemctl restart xray"

[ "$FIRST_INBOUND_LISTEN" = "$XRAY_LISTEN_IP" ] \
  || die "first inbound listen must be \"$XRAY_LISTEN_IP\" (got \"$FIRST_INBOUND_LISTEN\"). Set \`listen\` to match XRAY_LISTEN_IP in deploy/install.sh (default 192.0.2.1 — see AGENTS.md 'xray is the exit')."

[ "$FIRST_INBOUND_PORT" = "$XRAY_INBOUND_PORT" ] \
  || die "first inbound port must be ${XRAY_INBOUND_PORT} (got \"$FIRST_INBOUND_PORT\")."

FIRST_INBOUND_FOLLOW=$(jq -r '.inbounds[0].settings.followRedirect // false' "$XRAY_CONF")
[ "$FIRST_INBOUND_FOLLOW" = "true" ] \
  || die "first inbound settings.followRedirect must be true (so xray reads SO_ORIGINAL_DST)."

FIRST_INBOUND_NET=$(jq -r '.inbounds[0].settings.network // ""' "$XRAY_CONF")
[ "$FIRST_INBOUND_NET" = "tcp,udp" ] \
  || warn "first inbound settings.network is \"${FIRST_INBOUND_NET}\" (recommended \"tcp,udp\" to cover both client traffic types)"

log "xray config validated: dokodemo-door on ${XRAY_LISTEN_IP}:${XRAY_INBOUND_PORT}"

# Verify that every VLESS outbound's vnext address is resolvable
# from the host. If not, xray will sit silently retrying
# net.LookupHost and the daemon's first curl will getaddrinfo-timeout
# (and the operator will spend 2 hours debugging "DNS hangs").
# Warn, don't die: the operator might intentionally have a
# split-horizon DNS where the name resolves only via the tunnel
# (rare for a VLESS client config), or might be running install.sh
# from a network different from the deployed host.
VLESS_ADDRS=$(jq -r '.outbounds[]? | select(.protocol=="vless") | .settings.vnext[]?.address' "$XRAY_CONF" 2>/dev/null)
if [ -n "$VLESS_ADDRS" ]; then
  while IFS= read -r _addr; do
    [ -z "$_addr" ] && continue
    # Skip if it's already a literal IP — no resolution needed.
    if echo "$_addr" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
      :
    elif command -v getent >/dev/null && getent hosts "$_addr" >/dev/null 2>&1; then
      :
    else
      warn "VLESS address \"${_addr}\" does not resolve from this host. xray will fail to dial it."
      warn "    Fix: replace with a literal IP in $XRAY_CONF (e.g. .outbounds[0].settings.vnext[0].address = \"1.2.3.4\")"
    fi
  done <<< "$VLESS_ADDRS"
fi

# Lock down perms so the xray user (not root) can read the config
# but other users cannot. Always re-applied on every install run,
# because editing the file with `cat >`, `tee`, or a default-umask
# `nano` save will reset ownership to root:root 0644 — and the
# systemd unit runs xray as the xray user, which then fails to
# read the file. (Edit-in-place with `nano`/`vi`/`sed -i` keeps
# mode and ownership.)
chown root:xray "$XRAY_CONF"
chmod 0640 "$XRAY_CONF"

# -----------------------------------------------------------------------------
# 8. iptables / ip rules — transparent proxy into xray
#
# Two transports, one per protocol family, because they have
# different kernel requirements:
#
#   TCP: iptables -t nat -j REDIRECT (nat table)
#     — for TCP, getsockopt(SO_ORIGINAL_DST) returns the
#       pre-NAT destination, so xray can read where the client
#       wanted to go. Works out of the box, no special routing.
#
#   UDP: iptables -t mangle -j TPROXY (mangle table)
#     — for UDP, REDIRECT only updates the post-NAT dst in
#       conntrack (the kernel does NOT populate SO_ORIGINAL_DST
#       the same way as for TCP), so xray sees dst=${XRAY_LISTEN_IP}:${XRAY_INBOUND_PORT}
#       and tunnels to itself instead of the real target.
#       TPROXY preserves the original dst in the socket the
#       listener receives. Trade-off: TPROXY needs IP_TRANSPARENT
#       on the listener (we have that via CAP_NET_ADMIN on the
#       xray service) and a routing trick — a fwmark rule that
#       redirects TPROXY-marked packets to a local-route table,
#       otherwise the kernel routes them normally and they never
#       reach the xray socket.
#
#   OUTPUT -m owner --uid-owner wgserver: must persist across
#     reboots regardless of wg0 state. Saved to
#     /etc/wgserver/iptables.rules and loaded at boot by
#     wgserver-iptables.service.
#
#   PREROUTING -i wg0: only meaningful when wg0 is up. Installed
#     and removed by wg-quick@wg0 PostUp/PreDown (section 4).
# -----------------------------------------------------------------------------

install_output_rules() {
  # TCP — nat DNAT to the non-loopback xray IP (see comment block
  # above install_masquerade_peers and AGENTS.md "xray is the exit").
  # Using a non-loopback IP bypasses xray-core's anti-loopback check
  # in app/proxyman/inbound/worker.go (w.hub.Addr() == dest.NetAddr()
  # would otherwise reject REDIRECTed traffic whose GetOriginalDestination
  # resolves to the same IP:port as xray's listener).
  iptables -t nat -C OUTPUT -m owner --uid-owner "$WGSERVER_UID" -p tcp -j DNAT --to-destination "$XRAY_LISTEN_IP:$XRAY_INBOUND_PORT" 2>/dev/null \
    || iptables -t nat -A OUTPUT -m owner --uid-owner "$WGSERVER_UID" -p tcp -j DNAT --to-destination "$XRAY_LISTEN_IP:$XRAY_INBOUND_PORT"
  # UDP — mangle TPROXY (preserves original dst for xray; only way
  # to make dokodemo-door read the real destination for UDP packets).
  # --on-ip is the local address the kernel rewrites dst to; using
  # $XRAY_LISTEN_IP (not 0.0.0.0) keeps UDP in the same anti-loopback-
  # safe pattern as TCP DNAT above.
  iptables -t mangle -C OUTPUT -m owner --uid-owner "$WGSERVER_UID" -p udp -j TPROXY --tproxy-mark "$TPROXY_MARK/$TPROXY_MARK" --on-port "$XRAY_INBOUND_PORT" --on-ip "$XRAY_LISTEN_IP" 2>/dev/null \
    || iptables -t mangle -A OUTPUT -m owner --uid-owner "$WGSERVER_UID" -p udp -j TPROXY --tproxy-mark "$TPROXY_MARK/$TPROXY_MARK" --on-port "$XRAY_INBOUND_PORT" --on-ip "$XRAY_LISTEN_IP" \
    || warn "mangle OUTPUT TPROXY failed (likely kernel/nf_tables limitation; daemon UDP DNS will go direct via host NIC, TCP still proxied)"
}

install_tproxy_routes() {
  # Idempotent: `|| true` so re-running on a host that already
  # has the rule is a no-op rather than a failure.
  ip -4 rule add fwmark "$TPROXY_MARK/$TPROXY_MARK" lookup "$TPROXY_TABLE" 2>/dev/null || true
  ip -4 route add local 0.0.0.0/0 dev lo table "$TPROXY_TABLE" 2>/dev/null || true
}

install_masquerade_peers() {
  # Opt-in: classic WG-as-router behaviour for traffic that xray
  # doesn't proxy (ICMP, anything xray-core rejects). Source-NATs
  # peer traffic from $CLIENTS_CIDR going anywhere except back to
  # the peer subnet, so the public internet can route replies.
  # Default off — see AGENTS.md "no MASQUERADE" and the comment
  # in deploy.env.example for the design trade-off. Idempotent.
  if [ "${WGSERVER_MASQUERADE_PEERS:-0}" = "1" ]; then
    iptables -t nat -C POSTROUTING -s "$CLIENTS_CIDR" ! -d "$CLIENTS_CIDR" -j MASQUERADE 2>/dev/null \
      || iptables -t nat -A POSTROUTING -s "$CLIENTS_CIDR" ! -d "$CLIENTS_CIDR" -j MASQUERADE
    log "MASQUERADE for peer subnet (WGSERVER_MASQUERADE_PEERS=1)"
  else
    log "MASQUERADE for peer subnet: disabled (WGSERVER_MASQUERADE_PEERS=0)"
  fi
}

log "installing OUTPUT transparent-proxy rules (uid=${WGSERVER_UID} → :${XRAY_INBOUND_PORT})"
install_output_rules
log "installing TPROXY routing rules (fwmark=${TPROXY_MARK} → table ${TPROXY_TABLE} → local)"
install_tproxy_routes
install_masquerade_peers

# Bind XRAY_LISTEN_IP to lo so DNAT/TPROXY to that address resolves
# to a local socket. Idempotent: `ip addr show` exits 0 if the
# secondary IP is already there (filter + grep returns the matching
# line), so the add is skipped. The kernel auto-installs a `local`
# route for any address on lo, so no extra `ip route add local …` is
# needed.
if ip -4 addr show dev lo | grep -qF " ${XRAY_LISTEN_IP}/"; then
  log "XRAY_LISTEN_IP ${XRAY_LISTEN_IP} already on lo; skipping"
else
  ip -4 addr add "${XRAY_LISTEN_IP}/32" dev lo || die "failed to add ${XRAY_LISTEN_IP} to lo (required for xray inbound; see AGENTS.md 'xray is the exit')"
  log "added ${XRAY_LISTEN_IP}/32 to lo for xray inbound"
fi

# Persist the running iptables ruleset (nat + mangle + filter)
# across reboots via wgserver-iptables.service. iptables-save
# captures the mangle table (TPROXY rules) along with nat and
# filter, so the operator's other rules survive too. The ip
# rules / routes for TPROXY are NOT in iptables-save — those
# are loaded by a small helper script from the systemd unit
# (see section 13).
#
# iptables-save writes the file to /etc/wgserver/iptables.rules, but
# the /etc/wgserver/ directory itself is created in section 9
# (wgserver dirs). Without this mkdir -p, the first install on a
# fresh host dies with "No such file or directory" before section 9
# ever runs. mkdir -p is idempotent — section 9's later mkdir
# does no harm.
log "saving iptables ruleset → $IPTABLES_RULES"
mkdir -p "$(dirname "$IPTABLES_RULES")"
iptables-save -c > "$IPTABLES_RULES"
chmod 0600 "$IPTABLES_RULES"
chown root:wgserver "$IPTABLES_RULES"

# -----------------------------------------------------------------------------
# 9. wgserver dirs
#
# Per-directory perms (matters — xray runs as the xray user, not
# root, so it must be able to traverse /etc/xray and read
# /etc/xray/config.json):
#
#   /var/lib/wgserver/   0700  root→wgserver:wgserver
#   /var/lib/wgserver/psk 0700 wgserver:wgserver   (per-peer PSK files)
#   /etc/wgserver/       0750  root:wgserver       (wgserver reads its YAML)
#   /etc/xray/           0750  root:xray           (xray needs to traverse
#                                                  and read config.json;
#                                                  no `x` bit = EACCES
#                                                  on open(), which is the
#                                                  classic "permission
#                                                  denied" trap on dirs)
#   /etc/xray/config.json 0640 root:xray          (xray reads; nobody else)
# -----------------------------------------------------------------------------
mkdir -p "$ETC_WGSERVER" "$VAR_WGSERVER" "$ETC_XRAY"
chmod 0700 "$VAR_WGSERVER"
chown -R wgserver:wgserver "$VAR_WGSERVER"
mkdir -p "$VAR_WGSERVER/psk"
chown wgserver:wgserver "$VAR_WGSERVER/psk"
chmod 0700 "$VAR_WGSERVER/psk"
chown root:wgserver "$ETC_WGSERVER"
chmod 0750 "$ETC_WGSERVER"
chown root:xray "$ETC_XRAY"
chmod 0750 "$ETC_XRAY"

# -----------------------------------------------------------------------------
# 10. binary
# -----------------------------------------------------------------------------
log "installing binary to $BIN_PATH"
# When the operator passes the same path as both source ($1) and
# destination ($BIN_PATH) — e.g. `bash install.sh /usr/local/bin/wgserver`
# to reinstall in place — GNU install refuses with 'are the same file'.
# Detect and handle: just ensure the file is executable.
src_real=$(readlink -f "$WG_BINARY" 2>/dev/null || echo "$WG_BINARY")
dst_real=$(readlink -f "$BIN_PATH" 2>/dev/null || echo "$BIN_PATH")
if [ "$src_real" = "$dst_real" ]; then
  log "binary already at $BIN_PATH (same as source); chmod 0755, no copy"
  chmod 0755 "$BIN_PATH"
else
  install -m 0755 "$WG_BINARY" "$BIN_PATH"
fi

# -----------------------------------------------------------------------------
# 11. wgserver.env
# -----------------------------------------------------------------------------
if [ ! -f "$ENV_FILE" ]; then
  log "writing $ENV_FILE"
  cat > "$ENV_FILE" <<'EOF'
# Managed by wgserver install.sh.
# Pin to opt out of auto-update. Leave empty to always update.
# WGSERVER_VERSION=
EOF
  chmod 0640 "$ENV_FILE"
fi

# -----------------------------------------------------------------------------
# 12. wgserver.yaml
# -----------------------------------------------------------------------------
if [ ! -f "$CONF_FILE" ]; then
  log "writing $CONF_FILE"
  cat > "$CONF_FILE" <<EOF
# wgserver configuration. Generated by install.sh.
http:
  addr: "${LISTEN_ADDR}"
  # /healthz on a separate plain-HTTP TCP port so the auto-updater
  # can poll it. Leave empty to mount /healthz on the admin listener
  # (only works for plain HTTP, not for UNIX-socket or TLS admin).
  health_addr: "${HEALTH_ADDR}"

db:
  path: "${VAR_WGSERVER}/db.sqlite"

# The single WireGuard interface. Clients peer here; per-client
# peers are applied by the syncer via 'wg set wg0 peer ...'.
# wg0.conf MUST contain zero [Peer] sections. All client traffic
# is transparently redirected to a local xray-core (VLESS Reality)
# via iptables (PREROUTING -i wg0 + OUTPUT --uid-owner
# wgserver → ${XRAY_LISTEN_IP}:${XRAY_INBOUND_PORT}). xray runs as the xray system user
# and reads /etc/xray/config.json (operator-managed).
clients:
  interface: "wg0"
  listen_port: 51820
  address: "10.0.1.1/24"
  cidr: "10.0.1.0/24"
  dns_servers:
    - "1.1.1.1"
    - "9.9.9.9"
  # Endpoint goes into the [Peer] line of every client .conf the
  # Telegram bot hands out. Resolved from WGSERVER_PUBLIC_ENDPOINT
  # (with a loud warning if the operator didn't set it and we fell
  # back to $(hostname), which is almost always wrong). See
  # section "input" above.
  endpoint: "${WGSERVER_PUBLIC_ENDPOINT}"
  public_key: "${WG0_PUB}"

telegram:
  bot_token: "${TG_BOT_TOKEN}"
  group_chat_id: ${TG_CHAT_ID}
  per_user_quota: ${TG_QUOTA}

update:
  enabled: false
  github_repo: "erazumov/wgserver"
  check_interval_minutes: 60
EOF
  chmod 0640 "$CONF_FILE"
else
  log "wgserver.yaml already exists; preserving user edits"
fi
chown wgserver:wgserver "$CONF_FILE"

# -----------------------------------------------------------------------------
# 13. systemd units
# -----------------------------------------------------------------------------
log "writing /etc/wgserver/tproxy-routes.sh"
cat > /etc/wgserver/tproxy-routes.sh <<'TPROXY_EOF'
#!/bin/sh
# Apply the ip rule + ip route that TPROXY needs to deliver
# intercepted packets to the xray listener. iptables-save does NOT
# capture these (they are ip-rule state, not netfilter state), so
# they are loaded by this helper script from the systemd unit
# instead of being part of /etc/wgserver/iptables.rules.
#
# Self-correcting: removes any pre-existing matching rule/ route
# first, so re-runs (or partial-failure recoveries) don't pile up
# duplicates. The earlier `set -e` was a trap — `ip ... || true`
# would have masked any real error silently, and the `set -e`
# couldn't catch it because the failing command was followed by
# `|| true`.
#
# TPROXY_MARK and TPROXY_TABLE are hardcoded here, not passed
# via the systemd unit's Environment=. The unit does not export
# variables to ExecStart= scripts, and relying on a variable
# being set by an external caller made the script silently fail
# (the `ip` commands received empty arguments and errored out
# with the result masked by `2>/dev/null || true`).
set -e
TPROXY_MARK=0x1
TPROXY_TABLE=100

# Remove ALL pre-existing rules (handles duplicates from earlier
# runs where the script errored out partway, or from operators
# who ran the script manually multiple times). A single `del` only
# removes one rule — to converge to exactly one rule after every
# run we need a `while` loop that runs until `del` returns non-zero
# (no more matching rules).
while ip -4 rule  del fwmark ${TPROXY_MARK}/${TPROXY_MARK} lookup ${TPROXY_TABLE} 2>/dev/null; do :; done
# Add exactly one.
ip -4 rule  add fwmark ${TPROXY_MARK}/${TPROXY_MARK} lookup ${TPROXY_TABLE} 2>/dev/null || true
while ip -4 route del local 0.0.0.0/0 dev lo table ${TPROXY_TABLE}      2>/dev/null; do :; done
ip -4 route add local 0.0.0.0/0 dev lo table ${TPROXY_TABLE}      2>/dev/null || true
TPROXY_EOF
chmod 0755 /etc/wgserver/tproxy-routes.sh

log "writing $IPTABLES_UNIT"
cat > "$IPTABLES_UNIT" <<'UNIT_EOF'
[Unit]
Description=wgserver: load persistent iptables rules + TPROXY routing
Documentation=https://github.com/erazumov/wgserver
# Must run before wgserver and before wg-quick@wg0 (the latter
# installs the PREROUTING REDIRECT / TPROXY in its PostUp; the
# OUTPUT rules in $IPTABLES_RULES are independent but loading
# them early keeps things consistent).
Before=wgserver.service wg-quick@wg0.service
After=systemd-tmpfiles-setup.service

[Service]
Type=oneshot
RemainAfterExit=yes
# Two startup steps. systemd runs multiple ExecStart= lines in
# order; the first one exits before the second starts.
#
# 1) ip rule + ip route for TPROXY (the kernel needs these in
#    place before any TPROXY iptables rule can actually deliver
#    packets to a local socket).
# 2) iptables-restore from the file. StandardInput= is the
#    systemd-native way to feed a file on stdin — bash-style
#    "iptables-restore < file" in ExecStart would be passed as a
#    literal argument and fail with exit 1 (systemd does NOT run
#    ExecStart through a shell).
ExecStart=/etc/wgserver/tproxy-routes.sh
ExecStart=/sbin/iptables-restore
StandardInput=file:/etc/wgserver/iptables.rules

[Install]
WantedBy=multi-user.target
UNIT_EOF
chmod 0644 "$IPTABLES_UNIT"

log "writing xray.service"
cat > "$XRAY_UNIT" <<EOF
[Unit]
Description=xray-core (wgserver transparent exit)
Documentation=https://github.com/XTLS/Xray-core
# xray must be up before the wgserver syncer starts handling
# traffic (it doesn't, but wg0 PostUp's REDIRECT points to xray's
# port — so xray must accept before wg0 PostUp runs).
After=network-online.target
Wants=network-online.target
Before=wg-quick@wg0.service wgserver.service

[Service]
Type=simple
# Run as a dedicated user that does NOT match the wgserver
# iptables OUTPUT rule. If xray ran as the wgserver uid, its own
# VLESS outbound to the remote would itself get REDIRECTed →
# infinite loop. See AGENTS.md invariant.
User=xray
Group=xray
# CAP_NET_ADMIN is required so xray can do setsockopt(IP_TRANSPARENT)
# on its listening socket — which lets getsockopt(SO_ORIGINAL_DST)
# return the original destination of a connection that iptables NAT
# REDIRECT'd to this listener. Without it, dokodemo-door inbound sees
# every connection with destination = ${XRAY_LISTEN_IP}:${XRAY_INBOUND_PORT} and cannot
# figure out where the client actually wanted to go, so it just
# closes the socket (and the client gets RST). NoNewPrivileges is
# off because it would block AmbientCapabilities.
AmbientCapabilities=CAP_NET_ADMIN
LimitNPROC=10000
LimitNOFILE=1000000
ExecStart=$XRAY_PREFIX/xray run -config $XRAY_CONF
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "$XRAY_UNIT"

log "writing wgserver.service"
cat > "$SYSTEMD_UNIT" <<'EOF'
[Unit]
Description=wgserver: self-hosted WireGuard gateway with xray transparent exit
Documentation=https://github.com/erazumov/wgserver
After=network-online.target wg-quick@wg0.service xray.service wgserver-iptables.service
Wants=network-online.target wg-quick@wg0.service xray.service
Requires=wgserver-iptables.service

[Service]
Type=simple
# Run as a dedicated system user so its outbound (Telegram long-
# poll to api.telegram.org, future GitHub polls) is caught by the
# iptables OUTPUT REDIRECT rule (uid match) and tunnelled through
# xray. CAP_NET_ADMIN is needed by the syncer to call
# `wg set wg0 peer ...`.
User=wgserver
Group=wgserver
AmbientCapabilities=CAP_NET_ADMIN
NoNewPrivileges=true
EnvironmentFile=/etc/wgserver/wgserver.env
ExecStart=/usr/local/bin/wgserver serve -config /etc/wgserver/wgserver.yaml
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "$SYSTEMD_UNIT"

# -----------------------------------------------------------------------------
# 14. start services
# -----------------------------------------------------------------------------
log "systemctl daemon-reload + enable wgserver / xray / wgserver-iptables"
systemctl daemon-reload
systemctl enable wgserver-iptables.service
systemctl enable --now xray.service
systemctl enable --now wgserver.service

# Restart the long-running services so the new binary (and any
# updated unit-file settings) actually take effect. `enable --now`
# is a no-op when the service is already running, so on upgrade
# the old binary would stay loaded in memory without this.
#
# We deliberately do NOT restart wgserver-iptables.service: its
# ExecStart does `iptables-restore` which would wipe any
# operator-added iptables rules (e.g. the SSH-to-self exemption
# documented in README) by overwriting the kernel state with the
# saved file. The rules are already correct in the kernel from
# section 8; the systemd unit is just for boot persistence.
log "restarting wgserver and xray to load new binary"
systemctl restart wgserver.service xray.service

# Post-restart health check. Catches both:
#   - systemctl restart itself returned non-zero
#   - restart succeeded but the service immediately crashed
#     (e.g. new binary segfaults on startup, bad DB migration,
#     xray config rejected by newer xray version)
# We fail loudly instead of leaving the host in a half-broken
# state where the new binary is on disk but the old one is
# still in memory.
sleep 2
for svc in wgserver xray; do
  if ! systemctl is-active --quiet "$svc.service"; then
    die "$svc.service is not active after restart. Check: journalctl -u $svc -n 50"
  fi
done
log "post-restart health check passed (wgserver, xray both active)"

# -----------------------------------------------------------------------------
# 15. summary
# -----------------------------------------------------------------------------
cat <<EOF

${WG0_PUB}

== wgserver installed ==

  binary:        $BIN_PATH
  config:        $CONF_FILE
  env:           $ENV_FILE
  service:       $SYSTEMD_UNIT
  xray binary:   $XRAY_PREFIX/xray
  xray config:   $XRAY_CONF  (operator-managed, NOT touched by wgserver)
  xray service:  $XRAY_UNIT  (runs as user 'xray')
  iptables unit: $IPTABLES_UNIT
  wg0 pubkey:    ${WG0_PUB}

  client endpoint: ${WGSERVER_PUBLIC_ENDPOINT}
                   ^ every Telegram .conf will have THIS in [Peer] Endpoint =
                   If this is a placeholder (TEST-NET, $(hostname), empty),
                   the operator set WGSERVER_PUBLIC_ENDPOINT wrong in deploy.env.
                   Edit $CONF_FILE, set clients.endpoint: "...:51820",
                   then 'sudo systemctl restart wgserver' for the bot to pick it up.

  Next steps:
    1) tail -f journalctl -u wgserver -u xray
    2) sudo ${BIN_PATH} create-admin -username <name>  (then enter password)
    3) open http://${LISTEN_ADDR}/admin/login
    4) add a peer; download its .conf; import into a WireGuard client
    5) verify traffic:
         curl https://ifconfig.io   # from a peered client
       should show the remote VLESS server's public IP, NOT this host's

  The wg0 server pubkey above is what you put into every client .conf.

  Verify the wgserver daemon's own outbound goes through xray:
       sudo -u wgserver curl -sS https://ifconfig.io
    must differ from:
       curl -sS https://ifconfig.io   # run as root from the host

  If they match, the OUTPUT REDIRECT rule is missing — see $IPTABLES_RULES.
EOF
