#!/usr/bin/env bash
# install.sh — end-to-end installer for wgserver.
#
# Brings up a Debian 12 host as a WireGuard gateway with:
#   * wg0  — outbound WireGuard peer (the "exit WG", given by the user)
#   * wg1  — clients peer here, traffic forwarded to wg0 (MASQUERADE)
#   * wgserver  — admin UI + sync-loop, talking to wg1 via `wg set`
#
# Idempotent: re-running on a working host regenerates only the binary
# and the systemd unit. Server keys (wg0, wg1) are written once and
# preserved — overwriting them would invalidate every client .conf.
#
# Usage:
#   WGSERVER_EXIT_WG_ENDPOINT=host:51820 \
#   WGSERVER_EXIT_WG_PUBKEY=base64... \
#   ./install.sh /path/to/wgserver-linux-amd64
#
# Optional env vars: WGSERVER_LISTEN_ADDR, WGSERVER_TG_BOT_TOKEN,
#                    WGSERVER_TG_CHAT_ID, WGSERVER_TG_QUOTA.
# Anything not in the environment is prompted for (if stdin is a TTY).

set -euo pipefail

# -----------------------------------------------------------------------------
# paths
# -----------------------------------------------------------------------------
ETC_WG=/etc/wireguard
ETC_WGSERVER=/etc/wgserver
VAR_WGSERVER=/var/lib/wgserver
BIN_PATH=/usr/local/bin/wgserver
SYSTEMD_UNIT=/etc/systemd/system/wgserver.service
ENV_FILE="${ETC_WGSERVER}/wgserver.env"
CONF_FILE="${ETC_WGSERVER}/wgserver.yaml"
SYSCTL_FILE=/etc/sysctl.d/99-wgserver.conf

WG0_CONF="${ETC_WG}/wg0.conf"
WG1_CONF="${ETC_WG}/wg1.conf"

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
  # prompt VAR_NAME "label" [default]
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
EXIT_WG_ENDPOINT=${WGSERVER_EXIT_WG_ENDPOINT:-}
EXIT_WG_PUBKEY=${WGSERVER_EXIT_WG_PUBKEY:-}
TG_BOT_TOKEN=${WGSERVER_TG_BOT_TOKEN:-}
TG_CHAT_ID=${WGSERVER_TG_CHAT_ID:-0}
TG_QUOTA=${WGSERVER_TG_QUOTA:-2}

prompt EXIT_WG_ENDPOINT "exit_wg endpoint (host:port)" ""
prompt EXIT_WG_PUBKEY   "exit_wg pubkey (base64)"        ""

if [ -z "${1:-}" ] && [ -z "${WGSERVER_BINARY:-}" ]; then
  die "pass the wgserver binary as \$1 or set WGSERVER_BINARY"
fi
WG_BINARY=${1:-${WGSERVER_BINARY}}
[ -f "$WG_BINARY" ] || die "binary not found: $WG_BINARY"

# -----------------------------------------------------------------------------
# 1. apt deps
# -----------------------------------------------------------------------------
log "installing apt deps (wireguard-tools, iptables, curl)"
# curl is required by deploy.sh's /healthz check (a fresh Debian
# minimal install does not include it; without it, deploy.sh fails
# with a misleading "healthz never came up" even though wgserver is
# healthy).
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq wireguard-tools iptables curl

command -v wg >/dev/null      || die "wg not in PATH after install"
command -v wg-quick >/dev/null || die "wg-quick not in PATH after install"

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
# 2.5 wgserver system user
#
# Running the daemon as a dedicated system user lets us policy-route
# its outbound traffic (Telegram bot → api.telegram.org, future
# GitHub polls for the updater, etc.) through the exit WG via a
# `uidrange` ip rule. SSH/ping/apt on the host itself keep using the
# public NIC because they run as root or other system uids that don't
# match the uidrange rule.
#
# Created BEFORE any chown or wg0.conf heredoc reference; the uid is
# baked into the PostUp rule below.
# -----------------------------------------------------------------------------
if ! id -u wgserver >/dev/null 2>&1; then
  log "creating wgserver system user"
  useradd --system --no-create-home --shell /usr/sbin/nologin --user-group wgserver
fi
WGSERVER_UID=$(id -u wgserver)
log "wgserver uid = ${WGSERVER_UID}"

# -----------------------------------------------------------------------------
# 3. server keys (idempotent)
# -----------------------------------------------------------------------------
mkdir -p "$ETC_WG"
chmod 0700 "$ETC_WG"

write_keypair() {
  # write_keypair OUT_PRIV
  local _out=$1
  local _priv _pub
  _priv=$(wg genkey)
  _pub=$(printf '%s' "$_priv" | wg pubkey)
  printf '%s' "$_priv" > "$_out"
  chmod 0600 "$_out"
  printf '%s' "$_pub"
}

# wg0 — outbound to exit_wg server
if [ -f "$WG0_CONF" ]; then
  log "wg0.conf already exists; preserving server keys"
  WG0_PRIV=$(awk '/^PrivateKey/ {print $3; exit}' "$WG0_CONF")
  WG0_PUB=$(printf '%s' "$WG0_PRIV" | wg pubkey)
else
  log "generating wg0 server keypair"
  WG0_PUB=$(write_keypair /etc/wireguard/wg0.key)
  WG0_PRIV=$(cat /etc/wireguard/wg0.key)
fi

# wg1 — clients peer here
if [ -f "$WG1_CONF" ]; then
  log "wg1.conf already exists; preserving server keys"
  WG1_PRIV=$(awk '/^PrivateKey/ {print $3; exit}' "$WG1_CONF")
  WG1_PUB=$(printf '%s' "$WG1_PRIV" | wg pubkey)
else
  log "generating wg1 server keypair"
  WG1_PUB=$(write_keypair /etc/wireguard/wg1.key)
  WG1_PRIV=$(cat /etc/wireguard/wg1.key)
fi

# -----------------------------------------------------------------------------
# 4. wg0.conf — outbound to exit_wg
#
# Routing model: this server is a transparent gateway, not a WireGuard
# client. The host's own traffic (SSH, ping, apt, /healthz polling)
# MUST keep using the system's main routing table and the public NIC.
# Only forwarded client traffic (from the wg1 subnet) AND the
# wgserver daemon's own traffic (e.g. Telegram long-poll to
# api.telegram.org) is sent via wg0 to the exit_wg.
#
# Implementation:
#   * AllowedIPs = 0.0.0.0/0 — kept wide so the wg crypto table
#     accepts MASQUERADEd return packets (src = any internet IP,
#     dst = this host's wg0 IP). This is a kernel-level acceptance
#     rule, not a route.
#   * Table = off — tells wg-quick to NOT install AllowedIPs as
#     kernel routes. Without this, AllowedIPs=0.0.0.0/0 would
#     replace the host's default route with `default dev wg0`,
#     silently breaking SSH/ping/apt (the kernel would route every
#     response out via the tunnel, with the wrong source IP).
#   * PostUp / PreDown — install/remove a dedicated routing table
#     (51820) holding `default dev wg0`, plus two ip rules:
#       (1) source-based — traffic from the wg1 subnet (forwarded
#           client traffic), priority 100.
#       (2) uidrange — traffic from the wgserver uid (the daemon's
#           own outbound, e.g. Telegram API calls), priority 50
#           (checked before the source-based rule, so it wins
#           regardless of source IP).
#     Everything else (root, system uids, plain user shells = SSH/
#     ping/apt) keeps falling through to the main table and the
#     public default route.
#
# The conf is rewritten on every install.sh run, but the private key
# is read from the existing conf first (and /etc/wireguard/wg0.key
# on first install) so client .confs stay valid.
# -----------------------------------------------------------------------------
WG_ROUTING_TABLE=51820
CLIENTS_SUBNET=10.0.1.0/24   # MUST match Address = 10.0.1.1/24 in wg1.conf below
WGSERVER_UIDRULE_PRIO=50
CLIENTS_SUBNET_RULE_PRIO=100

# Read existing private key (or generate a new one on first install)
if [ -f "$WG0_CONF" ]; then
  log "refreshing $WG0_CONF (preserving server keys)"
  WG0_PRIV=$(awk '/^PrivateKey/ {print $3; exit}' "$WG0_CONF")
  WG0_PUB=$(printf '%s' "$WG0_PRIV" | wg pubkey)
else
  log "generating wg0 server keypair"
  WG0_PUB=$(write_keypair /etc/wireguard/wg0.key)
  WG0_PRIV=$(cat /etc/wireguard/wg0.key)
fi

cat > "$WG0_CONF" <<EOF
# managed by wgserver install.sh
# Do NOT add per-client peers here — they belong on wg1.
# See the install.sh comment block above for the routing model.
#
# Table, PostUp and PreDown are wg-quick extensions and MUST live in
# [Interface]. wg-quick only strips wg-quick-specific keys from
# [Interface]; if you put them in [Peer] they are passed verbatim to
# wg setconf, which does not understand them and fails with
# "Line unrecognized: 'Table=off'".
[Interface]
PrivateKey = ${WG0_PRIV}
ListenPort = 51820
Address = 10.0.0.1/24
Table = off
# NB: we use "del ... || true; add ..." rather than "replace" because
# iproute2 <6.4 (Debian 12 ships 6.1.0) does not implement
# "ip rule replace". Same end-state, works on every iproute2.
PostUp = ip -4 route replace default dev %i table ${WG_ROUTING_TABLE}; ip -4 rule del from ${CLIENTS_SUBNET} table ${WG_ROUTING_TABLE} priority ${CLIENTS_SUBNET_RULE_PRIO} 2>/dev/null || true; ip -4 rule add from ${CLIENTS_SUBNET} table ${WG_ROUTING_TABLE} priority ${CLIENTS_SUBNET_RULE_PRIO}; ip -4 rule del uidrange ${WGSERVER_UID}-${WGSERVER_UID} table ${WG_ROUTING_TABLE} priority ${WGSERVER_UIDRULE_PRIO} 2>/dev/null || true; ip -4 rule add uidrange ${WGSERVER_UID}-${WGSERVER_UID} table ${WG_ROUTING_TABLE} priority ${WGSERVER_UIDRULE_PRIO}
PreDown = ip -4 rule del uidrange ${WGSERVER_UID}-${WGSERVER_UID} table ${WG_ROUTING_TABLE} priority ${WGSERVER_UIDRULE_PRIO} || true; ip -4 rule del from ${CLIENTS_SUBNET} table ${WG_ROUTING_TABLE} priority ${CLIENTS_SUBNET_RULE_PRIO} || true; ip -4 route del default dev %i table ${WG_ROUTING_TABLE} || true

[Peer]
# exit_wg (upstream WireGuard). Forwarded client traffic and the
# wgserver daemon's own traffic are routed through it via table
# ${WG_ROUTING_TABLE} (see [Interface] PostUp above). The host's
# own traffic (SSH, ping, apt) is NOT affected.
PublicKey = ${EXIT_WG_PUBKEY}
Endpoint = ${EXIT_WG_ENDPOINT}
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 25
EOF
chmod 0600 "$WG0_CONF"

# -----------------------------------------------------------------------------
# 5. wg1.conf — clients peer here. NO [Peer] sections; per-client peers
#    are added by the syncer via `wg set wg1 peer ...`.
# -----------------------------------------------------------------------------
if [ ! -f "$WG1_CONF" ]; then
  log "writing $WG1_CONF"
  cat > "$WG1_CONF" <<EOF
# managed by wgserver install.sh
# Per-client peers are NOT listed here. The wgserver sync-loop calls
# 'wg set wg1 peer <pubkey> allowed-ips <ip>' after each admin action.
# See AGENTS.md invariant: "Single upstream WG peer."
[Interface]
PrivateKey = ${WG1_PRIV}
ListenPort = 51821
Address = 10.0.1.1/24

PostUp = iptables -A FORWARD -i %i -j ACCEPT; iptables -A FORWARD -o %i -j ACCEPT; iptables -t nat -A POSTROUTING -o wg0 -j MASQUERADE
PreDown = iptables -D FORWARD -i %i -j ACCEPT; iptables -D FORWARD -o %i -j ACCEPT; iptables -t nat -D POSTROUTING -o wg0 -j MASQUERADE
EOF
  chmod 0600 "$WG1_CONF"
fi

# -----------------------------------------------------------------------------
# 6. bring up interfaces
#
# With the PBR setup above, bringing up wg-quick@wg0 no longer affects
# the host's own traffic — SSH/ping/apt keep using the main routing
# table. The handshake check below is therefore a "client experience"
# check, not a "server reachability" check: if the exit-WG is down,
# only forwarded client traffic is affected. We still tear wg0 down in
# that case so the operator notices and the syncer doesn't try to push
# peers that won't work.
#
#   1. Pre-flight: TCP-connect to the exit-WG endpoint. Skip wg0 if
#      unreachable — operator fixes the endpoint, then enables wg0.
#   2. After wg-quick@wg0, wait up to 10s for a WireGuard handshake.
#      If none, `wg-quick down wg0` (PreDown cleans up the routing
#      table and rule).
# -----------------------------------------------------------------------------
log "enabling wg-quick@wg0 + wg-quick@wg1"
ep_host=${EXIT_WG_ENDPOINT%%:*}
ep_port=${EXIT_WG_ENDPOINT##*:}
SKIP_WG0=0
if ! timeout 5 bash -c "exec 3<>/dev/tcp/$ep_host/$ep_port" 2>/dev/null; then
  warn "exit_wg endpoint $EXIT_WG_ENDPOINT is unreachable (TCP connect failed)"
  warn "skipping wg-quick@wg0 — fix the endpoint, then run manually:"
  warn "    systemctl enable --now wg-quick@wg0"
  SKIP_WG0=1
fi

if [ "$SKIP_WG0" = "0" ]; then
  if ! systemctl enable --now wg-quick@wg0.service; then
    warn "wg-quick@wg0 failed to start; rolling on without it"
    SKIP_WG0=1
  else
    # wg-quick@wg0 may have already been up from a previous install;
    # `enable --now` does NOT restart it, so the freshly-written
    # PostUp/PreDown (e.g. the new uidrange rule) is not applied.
    # Force a restart to apply the new routing rules.
    systemctl restart wg-quick@wg0.service || {
      warn "wg-quick@wg0 restart failed; rolling on without it"
      SKIP_WG0=1
    }
    # Wait for handshake. `wg show <iface>` prints a "latest handshake:"
    # line once any peer has completed one; absent that, the tunnel
    # is half-up and only forwarded client traffic is broken (the
    # host's own traffic is unaffected by the PBR setup, but we still
    # tear wg0 down so the operator notices).
    hs_ok=0
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      if wg show wg0 2>/dev/null | grep -q "latest handshake"; then
        hs_ok=1
        break
      fi
      sleep 1
    done
    if [ "$hs_ok" = "0" ]; then
      warn "wg0 handshake did not establish within 10s — rolling back to keep server reachable"
      wg-quick down wg0 || true
      warn "fix the exit_wg endpoint, then bring wg0 up manually:"
      warn "    systemctl enable --now wg-quick@wg0"
      SKIP_WG0=1
    else
      log "wg0 handshake established"
    fi
  fi
fi

# wg1 is local-only (no [Peer] sections). Bringing it up only adds a
# local interface and FORWARD+MASQUERADE rules; it does NOT change the
# default route. Safe to enable unconditionally.
systemctl enable --now wg-quick@wg1.service

# -----------------------------------------------------------------------------
# 7. wgserver dirs
# -----------------------------------------------------------------------------
mkdir -p "$ETC_WGSERVER" "$VAR_WGSERVER"
chmod 0700 "$VAR_WGSERVER"
chown -R wgserver:wgserver "$VAR_WGSERVER"
# psk/ holds per-peer preshared-key files. The syncer writes a PSK
# to psk/<safe-pubkey> and passes that path to `wg set`; wireguard-tools
# 1.0.20210914 (Debian 12) only accepts a file path for preshared-key.
# Must be chowned: mkdir happens after the chown -R above and would
# otherwise leave it root:root.
mkdir -p "$VAR_WGSERVER/psk"
chown wgserver:wgserver "$VAR_WGSERVER/psk"
chmod 0700 "$VAR_WGSERVER/psk"
# /etc/wgserver stays root-owned (config is operator-edited), but
# the wgserver daemon must be able to read it. chgrp + 0750 lets the
# wgserver user traverse the directory; individual file perms
# (set below) control read access.
chown root:wgserver "$ETC_WGSERVER"
chmod 0750 "$ETC_WGSERVER"

# -----------------------------------------------------------------------------
# 8. binary
# -----------------------------------------------------------------------------
log "installing binary to $BIN_PATH"
install -m 0755 "$WG_BINARY" "$BIN_PATH"

# -----------------------------------------------------------------------------
# 9. wgserver.env
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
# 10. wgserver.yaml
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

# outbound WireGuard peer (the "exit WG"). All client traffic is routed
# through it via wg0. Per-client peers must NOT be added to wg0 — they
# belong on wg1 and are applied by the sync-loop.
exit_wg:
  interface: "wg0"
  listen_port: 51820
  address: "10.0.0.1/24"
  peer:
    endpoint: "${EXIT_WG_ENDPOINT}"
    public_key: "${EXIT_WG_PUBKEY}"
    allowed_ips: "0.0.0.0/0"
    persistent_keepalive: 25

# Per-client WireGuard interface. The server's public key here is what
# the .conf handed to each client puts in the [Peer] section.
clients:
  interface: "wg1"
  listen_port: 51821
  address: "10.0.1.1/24"
  cidr: "10.0.1.0/24"
  dns_servers:
    - "1.1.1.1"
    - "9.9.9.9"
  endpoint: "$(hostname):51821"
  public_key: "${WG1_PUB}"

telegram:
  bot_token: "${TG_BOT_TOKEN}"
  group_chat_id: ${TG_CHAT_ID}
  per_user_quota: ${TG_QUOTA}

update:
  enabled: false
  github_repo: "erazumov/wgserver"
  check_interval_minutes: 60
EOF
  chmod 0600 "$CONF_FILE"
else
  log "wgserver.yaml already exists; preserving user edits"
fi
# wgserver.service runs as the wgserver user (see section 11): the
# daemon reads this file at startup. chown is idempotent.
chown wgserver:wgserver "$CONF_FILE"

# -----------------------------------------------------------------------------
# 11. systemd unit
#
# Always rewritten (unlike wgserver.yaml, which we preserve). The
# user / capabilities / runtime path may have changed in install.sh.
# -----------------------------------------------------------------------------
log "writing $SYSTEMD_UNIT"
cat > "$SYSTEMD_UNIT" <<'EOF'
[Unit]
Description=wgserver: self-hosted WireGuard gateway
Documentation=https://github.com/erazumov/wgserver
After=network-online.target wg-quick@wg0.service wg-quick@wg1.service
Wants=network-online.target wg-quick@wg0.service
Requires=wg-quick@wg1.service

[Service]
Type=simple
# Run as a dedicated system user so its outbound traffic (e.g. the
# Telegram bot's long-poll to api.telegram.org) can be policy-routed
# via the exit WG through a uidrange ip rule (see wg0.conf PostUp).
# CAP_NET_ADMIN is needed for the syncer to call `wg set` on wg1.
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
# 12. start the service
# -----------------------------------------------------------------------------
log "systemctl daemon-reload + enable wgserver"
systemctl daemon-reload
systemctl enable wgserver.service
systemctl restart wgserver.service

# -----------------------------------------------------------------------------
# 13. summary
# -----------------------------------------------------------------------------
cat <<EOF

${WG1_PUB}

== wgserver installed ==

  binary:        $BIN_PATH
  config:        $CONF_FILE
  env:           $ENV_FILE
  service:       $SYSTEMD_UNIT
  exit_wg:       $EXIT_WG_ENDPOINT  (pubkey: ${EXIT_WG_PUBKEY:0:16}...)
  wg1 pubkey:    ${WG1_PUB}

  Next steps:
    1) tail -f journalctl -u wgserver
    2) sudo ${BIN_PATH} create-admin -username <name>  (then enter password)
    3) open http://${LISTEN_ADDR}/admin/login
    4) add a peer; download its .conf; import into a WireGuard client
    5) verify traffic:
         curl https://ifconfig.io  # from a peered client
       should show the exit_wg server's public IP

  The wg1 server pubkey above is what you put into every client .conf.
EOF
