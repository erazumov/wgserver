# wgserver

Self-hosted WireGuard server whose client traffic is transparently
proxied through a local [xray-core](https://github.com/XTLS/Xray-core)
VLESS Reality client. One binary, one bootstrap script, one Telegram
bot. See [AGENTS.md](AGENTS.md) for invariants and design rationale.

## TL;DR

1. Provision a Debian 12 host with a public IP.
2. Put your xray Reality config at `/etc/xray/config.json` (see below).
3. Set `WGSERVER_PUBLIC_ENDPOINT` to the host's public IP or FQDN.
4. `sudo ./deploy/install.sh /path/to/wgserver-linux-amd64`.
5. Create an admin, log in, hand out peers.

The rest of this README is what you need to do **before** step 4
to make step 4 actually work.

## Required manual setup BEFORE `install.sh`

The bootstrap script is mostly idempotent but a few things must
exist on the target host first. None of these are shipped by
`deploy.sh` — they're operator-supplied, on purpose.

### 1. `/etc/xray/config.json` (operator-managed, NOT touched by `install.sh`)

The installer validates the file but does not generate it. The
first inbound **must** be `dokodemo-door` on `127.0.0.1:10808`
(plain `socks`/`http` won't work — the iptables REDIRECT hands
raw TCP/UDP to that port and the xray listener needs to read
`SO_ORIGINAL_DST`).

Minimal template — fill in your VLESS Reality `vnext` and
`realitySettings` from your VLESS server operator:

```json
{
  "log": { "loglevel": "warning" },
  "inbounds": [{
    "listen": "127.0.0.1",
    "port": 10808,
    "protocol": "dokodemo-door",
    "settings": { "network": "tcp,udp", "followRedirect": true },
    "tag": "transparent"
  }],
  "outbounds": [{
    "protocol": "vless",
    "settings": {
      "vnext": [{
        "address": "vless.example.com",
        "port": 443,
        "users": [{
          "id": "00000000-0000-0000-0000-000000000000",
          "encryption": "none",
          "flow": "xtls-rprx-vision"
        }]
      }]
    },
    "streamSettings": {
      "network": "tcp",
      "security": "reality",
      "realitySettings": {
        "serverName": "www.googletagmanager.com",
        "fingerprint": "chrome",
        "shortId": "",
        "publicKey": ""
      }
    }
  }]
}
```

Filesystem perms (run before starting xray):

```bash
chown root:xray /etc/xray
chmod 0750 /etc/xray
chown root:xray /etc/xray/config.json
chmod 0640 /etc/xray/config.json
```

**The `address` field must resolve from the host** (system DNS, not
through xray). If your VLESS server is at a non-public hostname
(e.g. one that exists only on the VLESS-provider's internal DNS),
put the literal IP instead. The installer warns but does not
rewrite this — get it right before starting.

### 2. A Telegram bot (only if you want the bot)

Skip this section if you don't want the bot; set
`WGSERVER_TG_BOT_TOKEN=""` in `deploy.env` and the daemon won't
start the bot.

1. Talk to [@BotFather](https://t.me/BotFather), `/newbot`, copy
   the token to `WGSERVER_TG_BOT_TOKEN`.
2. **Disable privacy mode** before the bot sees any messages in
   groups: `/setprivacy` → `Disable`. Default is `Enabled`, which
   means the bot only sees `/commands` and `@mentions` — `getChat`
   will succeed but `/start` in the group will be silently
   ignored.
3. Add the bot to your target group.
4. Get the group's chat id. Easiest: forward any message from the
   group to [@RawDataBot](https://t.me/RawDataBot), look for
   `"chat": {"id": -100…}`. The negative `-100` prefix for
   supergroups is part of the id. Put it in `WGSERVER_TG_CHAT_ID`.
5. Set `WGSERVER_TG_QUOTA` (default 2) — max number of configs a
   single Telegram user can claim.

When `install.sh` runs it now logs a startup self-test:

```
telegram: startup: getMe OK — bot id=... @username ("name")
telegram: startup: getChat OK — id=... title="..." type=supergroup
```

If `getChat` fails, the log includes the three most common causes
(not a member, wrong chat_id, kicked/blocked).

### 3. Host firewall / cloud security group

The installer does not touch `iptables`/`nftables` rules on the
**public** NIC (it only sets up REDIRECT/TPROXY on `wg0` for the
tunnel). You need:

| Port | Proto | From | Purpose |
|------|-------|------|---------|
| 51820 | UDP | public internet | WireGuard handshake + data |
| 8080 | TCP | localhost only (or your reverse proxy) | admin UI |
| 9090 | TCP | localhost only | `/healthz` for the updater |
| 22 | TCP | your IP | SSH (if you want it) |

Don't open 8080/9090 to the public internet — admin auth is a
session cookie, not TLS, and `wgserver` is not designed to be
exposed. Use a reverse proxy with TLS, a UNIX socket, or SSH port
forwarding.

Outbound from the host must reach:

- The VLESS server at the address in `/etc/xray/config.json` (port 443).
- `api.telegram.org` (443, for the bot) and
  `api.github.com` (443, for the updater).
- `github.com` (443) for `wgserver-updater` release polling.

### 4. SSH-to-self exemption (must be done after first install)

`install.sh` installs `iptables -t nat -A PREROUTING -i wg0 -p tcp -j REDIRECT`
which catches **all** TCP from the wg subnet, including a client
trying to SSH to the wgserver itself (`10.0.1.1:22`). That SSH
gets REDIRECTed to xray, xray tries to dial `10.0.1.1:22` via
the VLESS server, the VLESS server can't route the private IP,
SSH fails.

After `install.sh` finishes, add the exemption:

```bash
iptables -t nat -I PREROUTING -i wg0 -d 10.0.1.0/24 -j RETURN
iptables-save -c | tee /etc/wgserver/iptables.rules >/dev/null
```

Or, if you also want clients to be able to reach the wgserver on
its public IP from inside the tunnel (without going through
VLESS), add a similar exemption for the public IP too.

## Environment variables for `deploy.sh` / `install.sh`

Set in `deploy.env` (or pass as shell env). All have safe defaults
except the ones marked **REQUIRED**.

| Var | Default | Notes |
|-----|---------|-------|
| `DEPLOY_HOST` | — | **REQUIRED** if you use `deploy.sh`, e.g. `root@vpn.example.com` |
| `WGSERVER_PUBLIC_ENDPOINT` | `$(hostname):51820` | **REQUIRED** for any real deploy. Public IP or FQDN that client `.conf` files will have in their `[Peer] Endpoint = ...` line. Short hostnames are almost never a valid public endpoint. **The installer warns loudly if this is unset, but does not refuse to install — every bot-issued `.conf` will be useless until you fix this.** |
| `WGSERVER_TG_BOT_TOKEN` | `""` (bot disabled) | From `@BotFather` |
| `WGSERVER_TG_CHAT_ID` | `0` (bot disabled) | Must match the group the bot is in |
| `WGSERVER_TG_QUOTA` | `2` | Max configs per Telegram user |
| `WGSERVER_LISTEN_ADDR` | `127.0.0.1:8080` | Admin UI |
| `WGSERVER_HEALTH_ADDR` | `127.0.0.1:9090` | `/healthz` (required if admin is on UNIX socket or TLS, so the updater can poll it) |
| `WGSERVER_XRAY_VERSION` | (latest) | Pin xray release tag, e.g. `v1.8.20` |
| `ENABLE_UPDATER` | `1` | Install `wgserver-updater` systemd unit + timer |
| `WGSERVER_BINARY` | `$1` | Path to the wgserver binary (alternative to passing as arg) |
| `REMOTE_TMP` | `/tmp` | Remote staging dir for the deploy |
| `VERSION` | `git describe` | Override the embedded version string |

## Post-install smoke test

After `install.sh` and a `systemctl restart wgserver`:

```bash
# 1. All services up
systemctl is-active xray wg-quick@wg0 wgserver-iptables wgserver

# 2. Daemon's own outbound really goes through VLESS
sudo -u wgserver curl -sS --max-time 10 https://ifconfig.io
# → must return the VLESS server's public IP, not the host's

# 3. Host's own traffic does NOT go through VLESS
curl -sS --max-time 5 https://ifconfig.io
# → returns the host's public IP (e.g. 203.0.113.10)

# 4. Create an admin
wgserver create-admin -username admin

# 5. In Telegram: /start in the group → bot DMs a .conf

# 6. Import the .conf into a WireGuard client, activate it:
#    - DNS resolves through VLESS
#    - HTTP through VLESS (curl https://ifconfig.io shows VLESS server IP)
#    - SSH works to external hosts
```

If `sudo -u wgserver curl` returns the host's IP, the OUTPUT
REDIRECT rule is missing — re-apply `wgserver-iptables.service`
(`systemctl restart wgserver-iptables`) and check
`iptables -t nat -S | grep uid-owner`.

## Health check

A comprehensive diagnostic script is provided at
`deploy/wgserver-healthcheck.sh`. Run as root after install
(or any time something looks off):

```bash
sudo bash deploy/wgserver-healthcheck.sh
# or with machine-readable JSON:
sudo bash deploy/wgserver-healthcheck.sh --json
```

The script checks ~25 conditions across:

- **Preflight** — `wg`, `iptables`, `jq`, `ss`, `ip`, `systemctl`, `curl`
- **systemd** — `xray`, `wgserver`, `wg-quick@wg0`, `wgserver-iptables`
- **wg0** — interface present, state UP, listen-port, public-key vs `wgserver.yaml`
- **xray config** — `/etc/xray/config.json` exists, readable, perms `0640 root:xray`; first inbound is `dokodemo-door` on `127.0.0.1:10808` with `followRedirect: true` and `network: tcp,udp`; VLESS outbound address resolvable
- **iptables** — `PREROUTING -i wg0 -p tcp REDIRECT :10808`, `PREROUTING -i wg0 -p udp TPROXY mark=0x1 :10808`, `OUTPUT uid=999 -p tcp REDIRECT :10808`, `OUTPUT uid=999 -p udp` (REDIRECT or TPROXY)
- **TPROXY delivery** — `ip rule` with `fwmark 0x1/0x1 lookup 100`, `ip route table 100` with `local 0.0.0.0/0 dev lo`
- **persistence** — `/etc/wgserver/iptables.rules` exists, non-empty, age < 24h
- **wgserver.yaml** — has `public_key` and `endpoint`; `endpoint` is not the default `$(hostname)`
- **outbound** — host's `curl ifconfig.io` returns the public IP, daemon's (`sudo -u wgserver`) returns a **different** IP (the VLESS server's). If they match, the proxy is not working.
- **xray activity** — `tunneling request` in the last 1m
- **bot self-test** — `getMe OK` + `getChat OK` in the last 24h
- **peer activity** — at least one wg handshake recorded (warns if zero)
- **xray binary** — present and runs

Output: per-check `✓` / `✗` / `⚠` with a one-line `fix:` when available,
plus a summary line. Exit codes: `0` all OK, `1` at least one FAIL,
`2` only WARN. The script is **strictly read-only** — it never writes
to any operator-controlled file. Set `NO_NET=1` to skip the two
`ifconfig.io` checks in air-gapped environments.

## Common issues

### xray exits with `status=23/n.a` and "permission denied" on `/etc/xray/config.json`

Wrong ownership/perms. The xray process runs as user `xray` and
needs to read the config:

```bash
chown root:xray /etc/xray
chmod 0750 /etc/xray
chown root:xray /etc/xray/config.json
chmod 0640 /etc/xray/config.json
```

### `wgserver` is `active` but the bot doesn't react to `/start` in the group

Run `journalctl -u wgserver -n 30 --no-pager | grep "telegram: startup"`
— the new self-test logs the three most common causes if
`getChat` fails. Most often: the bot's **privacy mode is enabled**
(see step 2 above).

### Client connects, tunnel is up, but every `curl` times out

Almost always: the wgserver's `wg0.conf` PostUp didn't run, or
iptables TPROXY rules are missing. On the host:

```bash
iptables -t mangle -S | grep TPROXY
# must show:
#   -A PREROUTING -i wg0 -p udp -j TPROXY --tproxy-mark 0x1/0x1 --on-port 10808
#   -A OUTPUT -p udp -m owner --uid-owner <uid> -j TPROXY --tproxy-mark 0x1/0x1 --on-port 10808

ip rule show
ip route show table 100
# rule: fwmark 0x1/0x1 lookup 100
# table 100: local 0.0.0.0/0 dev lo
```

If the TPROXY OUTPUT rule fails with "Invalid argument", the
kernel/nf_tables combo doesn't support TPROXY in OUTPUT — TCP
still works via REDIRECT, only UDP-DNS from the daemon is
unproxied. Functionally fine for most use cases; only matters if
you want the daemon's DNS queries to be anonymous.

### `sudo -u wgserver curl https://ifconfig.io` returns the host's IP instead of the VLESS server's IP

The OUTPUT REDIRECT rule didn't install. Re-apply:

```bash
iptables -t nat -A OUTPUT -m owner --uid-owner $(id -u wgserver) -p tcp -j REDIRECT --to-ports 10808
iptables -t nat -A OUTPUT -m owner --uid-owner $(id -u wgserver) -p udp -j REDIRECT --to-ports 10808
iptables-save -c | tee /etc/wgserver/iptables.rules >/dev/null
```

Then verify with `sudo -u wgserver curl -sS https://ifconfig.io`.

### Bot says "internal error: cannot generate keypair"

The `wg pubkey` failure is almost always a missing `OUTPUT PATH`
in the systemd unit (or running an old binary). Confirm the binary
mtime matches the latest build, then `systemctl restart wgserver`.

## Architecture in one diagram

```
[wg client] ──(WireGuard)──> wgserver host (eth0=<public-ip>)
                              │
                              ├─ wg0 (10.0.1.1/24) — single iface, peers in store
                              │
                              ├─ xray :127.0.0.1:10808 (dokodemo-door)
                              │       └─ outbound: vless+reality → <vless-server>:443
                              │
                              └─ iptables (this host, not eth0):
                                    PREROUTING -i wg0 -p tcp -j REDIRECT --to-ports 10808
                                    PREROUTING -i wg0 -p udp -j TPROXY    --tproxy-mark 0x1/0x1 --on-port 10808
                                    OUTPUT     -m owner --uid-owner 999 -p tcp -j REDIRECT --to-ports 10808
                                    OUTPUT     -m owner --uid-owner 999 -p udp -j REDIRECT --to-ports 10808
```

For design rationale (why single wg interface, why xray doesn't
run as wgserver, why TPROXY for UDP, etc.) see
[AGENTS.md](AGENTS.md).
