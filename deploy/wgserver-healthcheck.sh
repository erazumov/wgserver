#!/usr/bin/env bash
# wgserver-healthcheck.sh — comprehensive diagnostic for wgserver.
#
# Checks ~25 conditions across systemd services, wg0 interface,
# /etc/xray/config.json (schema + perms + DNS), iptables rules
# (PREROUTING REDIRECT/TPROXY + OUTPUT for daemon uid), ip rule/route
# for TPROXY delivery, /etc/wgserver/iptables.rules freshness,
# /etc/wgserver/wgserver.yaml sanity, outbound connectivity
# (host vs daemon — must differ), xray recent activity, bot
# self-test, peer handshakes, and the xray binary itself.
#
# Read-only by design: NEVER writes to any operator-controlled
# file. Prints a colored summary + exit code. Pass --json for
# machine-readable output.
#
# Usage:
#   sudo bash wgserver-healthcheck.sh        # human
#   sudo bash wgserver-healthcheck.sh --json # machine
#
# Exit codes:
#   0 — all OK
#   1 — at least one FAIL (something is broken)
#   2 — only WARN (degraded but functional)
#
# Config (env vars, all optional):
#   EXPECTED_PUBLIC_IP  — host's public IP (for sanity-check on
#                          the host-only curl ifconfig.io check)
#   EXPECTED_VLESS_IP   — VLESS server's public IP (for sanity-check
#                          on the daemon curl ifconfig.io check)
#   WGSERVER_UID        — uid that wgserver runs as (default: from
#                          `id -u wgserver`, falls back to 999)
#   XRAY_INBOUND_PORT   — port xray dokodemo-door listens on
#                          (default: 10808)
#   TPROXY_MARK         — fwmark used for TPROXY rules (default: 0x1)
#   TPROXY_TABLE        — routing table for TPROXY-marked packets
#                          (default: 100)
#   WARN_AGE_HOURS      — iptables.rules older than this is a WARN
#                          (default: 24)
#   NO_NET=1            — skip outbound curl ifconfig.io checks
#                          (useful in air-gapped environments)

set -uo pipefail

# -----------------------------------------------------------------------------
# config
# -----------------------------------------------------------------------------
EXPECTED_PUBLIC_IP=${EXPECTED_PUBLIC_IP:-}
EXPECTED_VLESS_IP=${EXPECTED_VLESS_IP:-}
WGSERVER_UID=${WGSERVER_UID:-$(id -u wgserver 2>/dev/null || echo 999)}
XRAY_INBOUND_PORT=${XRAY_INBOUND_PORT:-10808}
TPROXY_MARK=${TPROXY_MARK:-0x1}
TPROXY_TABLE=${TPROXY_TABLE:-100}
WARN_AGE_HOURS=${WARN_AGE_HOURS:-24}
NO_NET=${NO_NET:-0}
JSON_MODE=0
[ "${1:-}" = "--json" ] && JSON_MODE=1

XRAY_CONF=/etc/xray/config.json
IPTABLES_RULES=/etc/wgserver/iptables.rules
WG0_CONF=/etc/wireguard/wg0.conf
WG_YAML=/etc/wgserver/wgserver.yaml

# -----------------------------------------------------------------------------
# output helpers
# -----------------------------------------------------------------------------
if [ -t 1 ] && [ "$JSON_MODE" = "0" ]; then
  C_GREEN=$'\033[32m'; C_RED=$'\033[31m'; C_YEL=$'\033[33m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_GREEN=''; C_RED=''; C_YEL=''; C_DIM=''; C_OFF=''
fi

PASS=0; WARN=0; FAIL=0
JSON_CHECKS=""

record() {
  # record NAME LEVEL [DETAIL] [FIX]
  local name="$1" level="$2" detail="${3:-}" fix="${4:-}"
  case "$level" in
    ok)   PASS=$((PASS+1))
         if [ "$JSON_MODE" = "0" ]; then
           printf "  %s✓%s %s%s\n" "$C_GREEN" "$C_OFF" "$name" \
             "$( [ -n "$detail" ] && printf " %s—%s %s" "$C_DIM" "$C_OFF" "$detail" )"
         fi ;;
    warn) WARN=$((WARN+1))
         if [ "$JSON_MODE" = "0" ]; then
           printf "  %s⚠%s %s%s\n" "$C_YEL" "$C_OFF" "$name" \
             "$( [ -n "$detail" ] && printf " %s—%s %s" "$C_DIM" "$C_OFF" "$detail" )"
         fi ;;
    fail) FAIL=$((FAIL+1))
         if [ "$JSON_MODE" = "0" ]; then
           printf "  %s✗%s %s%s\n" "$C_RED" "$C_OFF" "$name" \
             "$( [ -n "$detail" ] && printf " %s—%s %s" "$C_DIM" "$C_OFF" "$detail" )"
           [ -n "$fix" ] && printf "      %sfix:%s %s\n" "$C_DIM" "$C_OFF" "$fix"
         fi ;;
  esac
  if [ -n "$JSON_CHECKS" ]; then JSON_CHECKS+=","; fi
  JSON_CHECKS+=$(jq -nc --arg n "$name" --arg l "$level" \
                       --arg d "$detail" --arg f "$fix" \
                       '{name:$n, level:$l, detail:$d, fix:$f}')
}

header() {
  [ "$JSON_MODE" = "1" ] && return
  echo
  printf "%s== %s ==%s\n" "$C_DIM" "$1" "$C_OFF"
}

# -----------------------------------------------------------------------------
# preflight
# -----------------------------------------------------------------------------
if [ "$(id -u)" -ne 0 ]; then
  if [ "$JSON_MODE" = "1" ]; then
    echo '{"ok":false,"error":"must run as root"}'
  else
    echo "error: must run as root (try: sudo $0)" >&2
  fi
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  if [ "$JSON_MODE" = "1" ]; then
    echo '{"ok":false,"error":"jq not installed (apt install jq)"}'
  else
    echo "error: jq not installed" >&2
  fi
  exit 1
fi

header "preflight"
for tool in wg iptables ss ip systemctl curl; do
  if command -v "$tool" >/dev/null 2>&1; then
    record "tool $tool" ok
  else
    record "tool $tool" fail "not in PATH" "apt install $tool"
  fi
done

# -----------------------------------------------------------------------------
# systemd services
# -----------------------------------------------------------------------------
header "systemd"
for svc in xray wgserver wg-quick@wg0 wgserver-iptables; do
  if systemctl is-active --quiet "$svc"; then
    record "$svc active" ok
  else
    state=$(systemctl is-active "$svc" 2>/dev/null || echo unknown)
    record "$svc active" fail "state=$state" "journalctl -u $svc -n 50; systemctl restart $svc"
  fi
done

# -----------------------------------------------------------------------------
# wg0 interface
# -----------------------------------------------------------------------------
header "wg0"
if ip link show wg0 >/dev/null 2>&1; then
  record "wg0 interface present" ok
  # Check the IFF_UP flag in the link flags (works for wg0 where
  # operstate is always UNKNOWN, unlike physical interfaces).
  # Output of `ip -o link show wg0` is:
  #   12: wg0: <POINTOPOINT,NOARP,UP,LOWER_UP> mtu 1420 ... state UNKNOWN ...
  # The UP flag is between < and > — UNKNOWN is the operstate which
  # would be a false negative.
  if ip -o link show wg0 2>/dev/null | grep -qE '<[^>]*UP[,>]'; then
    record "wg0 state UP" ok
  else
    record "wg0 state UP" fail "IFF_UP not set in <flags>" "systemctl restart wg-quick@wg0"
  fi
  listen_port=$(wg show wg0 listen-port 2>/dev/null || true)
  if [ "$listen_port" = "51820" ]; then
    record "wg0 listen-port = 51820" ok
  else
    record "wg0 listen-port = 51820" fail "got '$listen_port'" "check $WG0_CONF"
  fi
  actual_pubkey=$(wg show wg0 public-key 2>/dev/null || true)
  if [ -n "$actual_pubkey" ] && [ -f "$WG_YAML" ]; then
    expected_pubkey=$(awk '/public_key:/ {print $2; exit}' "$WG_YAML" | tr -d '"' || true)
    if [ "$actual_pubkey" = "$expected_pubkey" ]; then
      record "wg0 public-key matches wgserver.yaml" ok
    else
      record "wg0 public-key matches wgserver.yaml" fail \
        "kernel=$actual_pubkey yaml=$expected_pubkey" \
        "diff /etc/wireguard/wg0.conf vs $WG_YAML public_key"
    fi
  fi
else
  record "wg0 interface present" fail "no wg0" "systemctl start wg-quick@wg0"
fi

# -----------------------------------------------------------------------------
# /etc/xray/config.json
# -----------------------------------------------------------------------------
header "xray config"
if [ ! -f "$XRAY_CONF" ]; then
  record "$XRAY_CONF exists" fail "missing" "create $XRAY_CONF (dokodemo-door inbound, vless+reality outbound)"
elif [ ! -r "$XRAY_CONF" ]; then
  record "$XRAY_CONF readable" fail "unreadable"
else
  record "$XRAY_CONF exists and readable" ok
  perms=$(stat -c '%a %U %G' "$XRAY_CONF")
  if [ "$perms" = "640 root xray" ]; then
    record "$XRAY_CONF perms 0640 root:xray" ok
  else
    record "$XRAY_CONF perms 0640 root:xray" warn \
      "actual=$perms" \
      "chown root:xray $XRAY_CONF && chmod 0640 $XRAY_CONF"
  fi

  proto=$(jq -r '.inbounds[0].protocol // ""' "$XRAY_CONF")
  if [ "$proto" = "dokodemo-door" ]; then
    record "inbound[0] protocol = dokodemo-door" ok
  else
    record "inbound[0] protocol = dokodemo-door" fail "got '$proto'" \
      "fix-it: jq '.inbounds[0] |= (.protocol=\"dokodemo-door\" | .settings={\"network\":\"tcp,udp\",\"followRedirect\":true})' $XRAY_CONF > /tmp/xc && mv /tmp/xc $XRAY_CONF && chown root:xray $XRAY_CONF && chmod 0640 $XRAY_CONF && systemctl restart xray"
  fi

  listen=$(jq -r '.inbounds[0].listen // ""' "$XRAY_CONF")
  port=$(jq -r '.inbounds[0].port // ""' "$XRAY_CONF")
  if [ "$listen" = "127.0.0.1" ] && [ "$port" = "$XRAY_INBOUND_PORT" ]; then
    record "inbound[0] listen 127.0.0.1:$XRAY_INBOUND_PORT" ok
  else
    record "inbound[0] listen 127.0.0.1:$XRAY_INBOUND_PORT" fail "got $listen:$port"
  fi

  follow=$(jq -r '.inbounds[0].settings.followRedirect // false' "$XRAY_CONF")
  if [ "$follow" = "true" ]; then
    record "inbound[0] settings.followRedirect = true" ok
  else
    record "inbound[0] settings.followRedirect = true" fail "got '$follow'" \
      "without it, xray can't read SO_ORIGINAL_DST and tunnels to itself"
  fi

  network=$(jq -r '.inbounds[0].settings.network // ""' "$XRAY_CONF")
  if [ "$network" = "tcp,udp" ]; then
    record "inbound[0] settings.network = tcp,udp" ok
  else
    record "inbound[0] settings.network = tcp,udp" warn \
      "got '$network'" "TCP-only works, UDP-from-clients will hit port 10808 unproxied"
  fi

  vless_addr=$(jq -r '.outbounds[]? | select(.protocol=="vless") | .settings.vnext[]?.address' "$XRAY_CONF" 2>/dev/null | head -1)
  if [ -z "$vless_addr" ]; then
    record "vless outbound present" fail "no vless outbound in config"
  else
    record "vless outbound present" ok "address=$vless_addr"
    if echo "$vless_addr" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
      record "vless address is literal IP (no DNS dependency)" ok
    elif getent hosts "$vless_addr" >/dev/null 2>&1; then
      record "vless address $vless_addr resolves" ok
    else
      record "vless address $vless_addr resolves" fail "DNS NXDOMAIN" \
        "replace with literal IP in $XRAY_CONF (xray runs as user 'xray' with system DNS)"
    fi
  fi
fi

# -----------------------------------------------------------------------------
# iptables
# -----------------------------------------------------------------------------
header "iptables"
if iptables -t nat -C PREROUTING -i wg0 -p tcp -j REDIRECT --to-ports "$XRAY_INBOUND_PORT" 2>/dev/null; then
  record "PREROUTING -i wg0 -p tcp REDIRECT :$XRAY_INBOUND_PORT" ok
else
  record "PREROUTING -i wg0 -p tcp REDIRECT :$XRAY_INBOUND_PORT" fail "rule missing" \
    "check wg-quick@wg0 PostUp; systemctl restart wg-quick@wg0"
fi

if iptables -t mangle -C PREROUTING -i wg0 -p udp -j TPROXY \
      --tproxy-mark "$TPROXY_MARK/$TPROXY_MARK" --on-port "$XRAY_INBOUND_PORT" 2>/dev/null; then
  record "PREROUTING -i wg0 -p udp TPROXY mark=$TPROXY_MARK :$XRAY_INBOUND_PORT" ok
else
  record "PREROUTING -i wg0 -p udp TPROXY mark=$TPROXY_MARK :$XRAY_INBOUND_PORT" fail "rule missing" \
    "check wg-quick@wg0 PostUp; systemctl restart wg-quick@wg0"
fi

if iptables -t nat -C OUTPUT -m owner --uid-owner "$WGSERVER_UID" -p tcp -j REDIRECT --to-ports "$XRAY_INBOUND_PORT" 2>/dev/null; then
  record "OUTPUT uid=$WGSERVER_UID -p tcp REDIRECT :$XRAY_INBOUND_PORT" ok
else
  record "OUTPUT uid=$WGSERVER_UID -p tcp REDIRECT :$XRAY_INBOUND_PORT" fail "rule missing" \
    "run install.sh (section 8) or systemctl restart wgserver-iptables"
fi

out_udp_ok=0
if iptables -t nat -C OUTPUT -m owner --uid-owner "$WGSERVER_UID" -p udp -j REDIRECT --to-ports "$XRAY_INBOUND_PORT" 2>/dev/null; then
  out_udp_ok=1
  record "OUTPUT uid=$WGSERVER_UID -p udp REDIRECT :$XRAY_INBOUND_PORT" ok
fi
if iptables -t mangle -C OUTPUT -m owner --uid-owner "$WGSERVER_UID" -p udp -j TPROXY \
      --tproxy-mark "$TPROXY_MARK/$TPROXY_MARK" --on-port "$XRAY_INBOUND_PORT" 2>/dev/null; then
  out_udp_ok=1
  record "OUTPUT uid=$WGSERVER_UID -p udp TPROXY mark=$TPROXY_MARK :$XRAY_INBOUND_PORT" ok
fi
if [ "$out_udp_ok" = "0" ]; then
  record "OUTPUT uid=$WGSERVER_UID -p udp (REDIRECT or TPROXY) :$XRAY_INBOUND_PORT" fail \
    "neither REDIRECT nor TPROXY present" \
    "daemon UDP (e.g. DNS via glibc) will not be proxied"
fi

# -----------------------------------------------------------------------------
# ip rule + route (TPROXY delivery)
# -----------------------------------------------------------------------------
header "TPROXY routing"
if ip rule show 2>/dev/null | grep -qE "fwmark ${TPROXY_MARK}/${TPROXY_MARK}[[:space:]]+lookup[[:space:]]+${TPROXY_TABLE}\b"; then
  record "ip rule: fwmark $TPROXY_MARK/$TPROXY_MARK lookup $TPROXY_TABLE" ok
else
  record "ip rule: fwmark $TPROXY_MARK/$TPROXY_MARK lookup $TPROXY_TABLE" fail "missing" \
    "systemctl restart wgserver-iptables (runs tproxy-routes.sh)"
fi
if ip route show table "$TPROXY_TABLE" 2>/dev/null | grep -qE "^local 0\.0\.0\.0/0 dev lo"; then
  record "ip route table $TPROXY_TABLE: local 0.0.0.0/0 dev lo" ok
else
  record "ip route table $TPROXY_TABLE: local 0.0.0.0/0 dev lo" fail "missing"
fi

# -----------------------------------------------------------------------------
# iptables.rules file (reboot persistence)
# -----------------------------------------------------------------------------
header "iptables persistence"
if [ -f "$IPTABLES_RULES" ]; then
  record "$IPTABLES_RULES exists" ok
  if [ -s "$IPTABLES_RULES" ]; then
    record "$IPTABLES_RULES non-empty" ok
  else
    record "$IPTABLES_RULES non-empty" fail "empty file" "iptables-save -c > $IPTABLES_RULES"
  fi
  age_h=$(( ( $(date +%s) - $(stat -c %Y "$IPTABLES_RULES") ) / 3600 ))
  if [ "$age_h" -gt "$WARN_AGE_HOURS" ]; then
    record "$IPTABLES_RULES age <${WARN_AGE_HOURS}h" warn \
      "actual=${age_h}h" "iptables-save -c > $IPTABLES_RULES"
  else
    record "$IPTABLES_RULES age <${WARN_AGE_HOURS}h" ok "${age_h}h"
  fi
else
  record "$IPTABLES_RULES exists" fail "missing" "systemctl restart wgserver-iptables"
fi

# -----------------------------------------------------------------------------
# wgserver.yaml
# -----------------------------------------------------------------------------
header "wgserver.yaml"
if [ -f "$WG_YAML" ]; then
  record "$WG_YAML exists" ok
  if grep -qE '^[[:space:]]*public_key:' "$WG_YAML" && grep -qE '^[[:space:]]*endpoint:' "$WG_YAML"; then
    record "$WG_YAML has public_key and endpoint" ok
  else
    record "$WG_YAML has public_key and endpoint" fail
  fi
  # awk -F'"' splits on double quotes — for `endpoint: "1.2.3.4:51820"`
  # the second field is exactly the value. Robust against leading
  # spaces, comments, and unquoted values.
  endpoint=$(grep -E '^[[:space:]]*endpoint:' "$WG_YAML" | head -1 | awk -F'"' '{print $2}')
  short_host=$(hostname -s 2>/dev/null || hostname)
  if [ -n "$endpoint" ] && [ "$endpoint" != "${short_host}:51820" ]; then
    record "endpoint not default (\$hostname)" ok "endpoint=$endpoint"
  else
    record "endpoint not default (\$hostname)" warn \
      "endpoint=$endpoint looks like short hostname" \
      "WGSERVER_PUBLIC_ENDPOINT should be a public IP/FQDN before next deploy"
  fi
else
  record "$WG_YAML exists" fail "missing" "run install.sh (generates $WG_YAML on first run if missing)"
fi

# -----------------------------------------------------------------------------
# outbound connectivity
# -----------------------------------------------------------------------------
header "outbound connectivity"
if [ "$NO_NET" = "1" ]; then
  record "outbound checks" ok "skipped (NO_NET=1)"
else
  HOST_IP=$(curl -sS --max-time 5 https://ifconfig.io 2>/dev/null || true)
  if [ -n "$HOST_IP" ]; then
    if [ -n "$EXPECTED_PUBLIC_IP" ] && [ "$HOST_IP" != "$EXPECTED_PUBLIC_IP" ]; then
      record "host outbound" warn \
        "ifconfig.io says $HOST_IP, expected $EXPECTED_PUBLIC_IP" \
        "operator: confirm with EXPECTED_PUBLIC_IP=... rerun"
    else
      record "host outbound" ok "$HOST_IP"
    fi
  else
    record "host outbound" fail "curl ifconfig.io failed" "check network/firewall/DNS"
  fi

  DAEMON_IP=$(sudo -u wgserver curl -sS --max-time 5 https://ifconfig.io 2>/dev/null || true)
  if [ -n "$DAEMON_IP" ]; then
    if [ -n "$EXPECTED_VLESS_IP" ] && [ "$DAEMON_IP" != "$EXPECTED_VLESS_IP" ]; then
      record "daemon outbound" warn \
        "ifconfig.io says $DAEMON_IP, expected $EXPECTED_VLESS_IP" \
        "operator: confirm with EXPECTED_VLESS_IP=... rerun"
    elif [ -n "$HOST_IP" ] && [ "$DAEMON_IP" = "$HOST_IP" ]; then
      record "daemon outbound" fail \
        "$DAEMON_IP same as host — proxy NOT working" \
        "OUTPUT REDIRECT rule missing; systemctl restart wgserver-iptables"
    else
      record "daemon outbound" ok "$DAEMON_IP (differs from host, proxy working)"
    fi
  else
    record "daemon outbound" fail "sudo -u wgserver curl ifconfig.io failed" \
      "check OUTPUT REDIRECT rule and xray.service status"
  fi
fi

# -----------------------------------------------------------------------------
# xray recent activity
# -----------------------------------------------------------------------------
header "xray activity"
if [ "$NO_NET" = "1" ]; then
  record "xray activity" ok "skipped (NO_NET=1)"
else
  if journalctl -u xray --since "1 minute ago" --no-pager 2>/dev/null | grep -q "tunneling request"; then
    record "xray outbound activity in last 1m" ok
  else
    record "xray outbound activity in last 1m" warn \
      "no 'tunneling request' in journal" \
      "no client traffic in last 1m, OR xray can't reach VLESS server (check vless address DNS)"
  fi
fi

# -----------------------------------------------------------------------------
# bot startup self-test (last 24h)
# -----------------------------------------------------------------------------
header "telegram bot"
if journalctl -u wgserver --since "24 hours ago" --no-pager 2>/dev/null | grep -q "telegram: startup: getMe OK" \
   && journalctl -u wgserver --since "24 hours ago" --no-pager 2>/dev/null | grep -q "telegram: startup: getChat OK"; then
  record "bot self-test (last 24h)" ok "getMe OK + getChat OK"
else
  if journalctl -u wgserver --since "24 hours ago" --no-pager 2>/dev/null | grep -q "telegram: startup: getMe OK"; then
    record "bot self-test (last 24h)" warn "getChat failed" "check journalctl -u wgserver -n 50 | grep 'telegram: startup'"
  else
    record "bot self-test (last 24h)" warn "no startup logs" \
      "wgserver hasn't restarted in last 24h, OR bot is disabled (group_chat_id=0)"
  fi
fi

# -----------------------------------------------------------------------------
# peer handshakes
# -----------------------------------------------------------------------------
header "client activity"
handshakes=$(wg show wg0 latest-handshakes 2>/dev/null | wc -l)
if [ "$handshakes" -gt 0 ]; then
  record "$handshakes peer handshake(s) recorded" ok
else
  record "peer handshakes recorded" warn "0 handshakes" \
    "no clients connected in this wg0's lifetime — expected if no peer has ever dialled in"
fi

# -----------------------------------------------------------------------------
# xray binary
# -----------------------------------------------------------------------------
header "xray binary"
if command -v xray >/dev/null 2>&1; then
  xray_version_out=$(xray version 2>&1 | head -1 || true)
  if [ -n "$xray_version_out" ]; then
    record "xray binary works" ok "$xray_version_out"
  else
    record "xray binary works" fail "xray version errored"
  fi
else
  record "xray binary in PATH" fail "xray not in PATH" \
    "WGSERVER_XRAY_VERSION=... and reinstall, or copy xray to /usr/local/xray/xray"
fi

# -----------------------------------------------------------------------------
# summary
# -----------------------------------------------------------------------------
TOTAL=$((PASS+WARN+FAIL))
if [ "$JSON_MODE" = "1" ]; then
  printf '[%s]' "$JSON_CHECKS" | jq --argjson p "$PASS" --argjson w "$WARN" --argjson f "$FAIL" --argjson t "$TOTAL" \
    '{ok: ($f == 0), summary: {pass: $p, warn: $w, fail: $f, total: $t}, checks: .}'
else
  echo
  if [ "$FAIL" -gt 0 ]; then
    printf "%s%d FAIL%s · %s%d WARN%s · %s%d OK%s (%d total)\n" \
      "$C_RED" "$FAIL" "$C_OFF" "$C_YEL" "$WARN" "$C_OFF" "$C_GREEN" "$PASS" "$C_OFF" "$TOTAL"
  elif [ "$WARN" -gt 0 ]; then
    printf "%s%d WARN%s · %s%d OK%s (%d total)\n" \
      "$C_YEL" "$WARN" "$C_OFF" "$C_GREEN" "$PASS" "$C_OFF" "$TOTAL"
  else
    printf "%s%d OK%s (%d total)\n" "$C_GREEN" "$PASS" "$C_OFF" "$TOTAL"
  fi
fi

if [ "$FAIL" -gt 0 ]; then
  exit 1
elif [ "$WARN" -gt 0 ]; then
  exit 2
fi
exit 0
