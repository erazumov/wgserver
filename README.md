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
`deploy/wgserver-healthcheck.sh`. It checks ~25 conditions across:

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

### How to run it on the server

The script is in the repo, not on the host. After install, get it
onto the server once and put it somewhere stable:

```bash
# from the mac (or wherever you build):
scp deploy/wgserver-healthcheck.sh root@vpn.example.com:/usr/local/bin/wgserver-healthcheck

# on the server:
chmod +x /usr/local/bin/wgserver-healthcheck
```

Then any time you suspect something is off:

```bash
# from the server (or via ssh):
sudo /usr/local/bin/wgserver-healthcheck
# or for a script-friendly machine-readable output:
sudo /usr/local/bin/wgserver-healthcheck --json | jq
```

The script can also be run via `deploy.sh` automatically on every
deploy by adding a one-liner at the end of your deploy script:

```bash
ssh "$DEPLOY_HOST" "/usr/local/bin/wgserver-healthcheck" || exit 1
```

(That makes a failed healthcheck fail the deploy, so a broken
upgrade never silently leaves a half-working host.)

## Client-side diagnostics (after activating the .conf)

Most "the VPN doesn't work" complaints fall into a small number of
buckets. Walk through these in order — each step tests one layer
of the stack, and a failure at step N makes step N+1 meaningless.

Throughout, replace `10.0.1.X` with the IP your .conf assigned
(it's in the `[Interface] Address = ...` line) and `10.0.1.1` with
the wgserver's wg IP (in the server's `wgserver.yaml`,
`clients.address`).

### Step 0: can the client even reach the server?

Before debugging the tunnel, verify the underlying UDP/51820 path
is open. From the client (Mac/Linux):

```bash
# can the client TCP-ping the endpoint? (TCP; some firewalls let
# ICMP through but block UDP)
nc -zuv vpn.example.com 51820
# should print "Connection to vpn.example.com 51820 port [udp/*] succeeded!"

# alternative: just try the WireGuard app. If it can't even reach
# the endpoint, the issue is firewall / NAT / wrong endpoint, NOT
# the proxy.
```

If this fails:
- **Endpoint is wrong** — check the `[Peer] Endpoint = ...` line in
  the .conf. `WGSERVER_PUBLIC_ENDPOINT` must be set to a public IP
  or FQDN that resolves from the client's network.
- **UDP 51820 is blocked** — by the client's ISP, by a corporate
  firewall, by a hotel/airport WiFi. Try a different network
  (phone hotspot is a good test), or add `ListenPort = 53` to
  the server's wg0.conf (sacrifices DNS, but works behind most
  captive portals).
- **Server firewall** — on the server, `ss -ulnp | grep 51820`
  must show wg-quick listening, and the cloud security group
  must allow UDP 51820 from the public internet.

### Step 1: is the tunnel actually up?

On the **client** (Mac: WireGuard app → log; Linux: `sudo wg show`):

```bash
# Linux:
sudo wg show
# look for the peer block. You're good if:
#   latest handshake: 1 minute, 23 seconds ago   (< 2 min = fresh)
#   transfer: 1.23 KiB received, 4.56 KiB sent  (non-zero)
```

On the **server**:

```bash
# per-peer view (replace PUBKEY with the peer's PublicKey from the .conf)
sudo wg show wg0
sudo wg show wg0 latest-handshakes
sudo wg show wg0 transfer
```

If `latest handshake` is empty or older than 2 minutes:
- **PubKey mismatch** — the .conf's `[Peer] PublicKey` does not
  match the wgserver's wg0 public key (`wg show wg0 public-key`
  on the server, `public_key: ...` in `wgserver.yaml`).
- **PSK mismatch** — the .conf's `PresharedKey` does not match
  the wgserver's per-peer PSK (in the DB; visible in admin UI).
  Re-claim the .conf and try again.
- **AllowedIPs doesn't cover 0.0.0.0/0** — your .conf should
  have `AllowedIPs = 0.0.0.0/0, ::/0` for full-tunnel. If it's
  `10.0.1.2/32` only, the tunnel is up but only sees wgserver
  itself.

If `transfer: 0 B received, 0 B sent`:
- the handshake completed but no traffic is flowing. Usually a
  routing issue on the client (next step).

### Step 2: does the client have a tunnel IP?

On the **client**:

```bash
# Linux:
ip addr show | grep -A1 'wg\|tun\|utun'
# or
ip route get 10.0.1.1
# should report a route via the wg interface (e.g. "dev wg0" or
# "dev utun3")

# Mac: System Settings → Network → WireGuard → "Connected"
# shows the assigned IP. Or:
ifconfig | grep -A1 utun
```

If no IP is assigned:
- the .conf failed to import cleanly. Re-import it and watch
  the WireGuard app's log.
- the `[Interface] Address` line in the .conf is missing or
  malformed. Must be a CIDR like `10.0.1.2/32`.

### Step 3: can the client reach the wgserver's wg IP?

This is the first "is the tunnel actually moving packets" test.
If this fails, nothing else matters.

```bash
# from the client:
ping -c 3 -W 2 10.0.1.1
# or on macOS:
ping -c 3 10.0.1.1
```

If this fails:
- the wg server isn't answering. On the **server**:
  ```bash
  sudo wg show wg0
  # the peer's transfer counter should be increasing as you ping
  # if it isn't, packets are not arriving on the server at all
  ```
- AllowedIPs on the server side doesn't include the client's
  IP. The wgserver syncer should have set `allowed-ips 10.0.1.X/32`
  automatically; check `sudo wg show wg0 allowed-ips`.
- icmp is blocked on the server. Debian default is ACCEPT for
  the FORWARD chain, so this shouldn't happen, but if you have
  a custom INPUT chain that drops icmp, allow it.

### Step 4: does DNS work through the tunnel?

This is where the UDP TPROXY path is exercised.

```bash
# from the client (force DNS over the tunnel — this skips the
# system's default resolver and tests that 1.1.1.1 is reachable
# THROUGH the tunnel):
dig +time=5 +tries=1 @1.1.1.1 example.com
nslookup example.com 1.1.1.1
```

If DNS works → the full UDP path is alive (iptables TPROXY +
ip rule + ip route + xray → VLESS).

If DNS hangs:
- on the **server**, check:
  ```bash
  sudo iptables -t mangle -S | grep TPROXY
  # must show:
  #   -A PREROUTING -i wg0 -p udp -j TPROXY --tproxy-mark 0x1/0x1 --on-port 10808

  ip rule show | grep fwmark
  # must show: 100: from all fwmark 0x1/0x1 lookup 100

  ip route show table 100
  # must show: local 0.0.0.0/0 dev lo
  ```
  If any of these is missing, run `sudo /usr/local/bin/wgserver-healthcheck`
  for the full diagnostic.
- The client may have a stale DNS cache. Disconnect the VPN,
  `sudo dscacheutil -flushcache` (macOS) or `sudo resolvectl
  flush-caches` (Linux), reconnect.

### Step 5: does traffic actually go through the VLESS proxy?

```bash
# from the client:
curl -sS --max-time 10 https://ifconfig.io
# or for a faster check:
curl -sS --max-time 10 https://api.ipify.org
```

If the IP returned equals the **server's** public IP → the proxy
is not working; traffic is leaking out via the host's main NIC
or your local network. The most common cause is that
`wgserver-iptables.service` wasn't reloaded after a wgserver
restart — the daemon's traffic (and client traffic) is going
direct instead of through xray.

If the IP returned is a **different IP** (the VLESS server's) →
the proxy is working. You can verify by:
```bash
# run the same check 3 times in a row; if the IP changes each
# time, the VLESS server has a rotating IP (CDN). If it's stable,
# it's the VLESS server's real IP.
for i in 1 2 3; do curl -sS --max-time 5 https://ifconfig.io; done
```

### Step 6: specific protocol/port issues

If everything above works but a specific app/protocol doesn't:

| Symptom | Likely cause | Fix |
|---|---|---|
| `curl https://example.com` works, but `ssh user@host` hangs | MTU too high (default 1500, but with VLESS encapsulation needs ~1400) | Add `MTU = 1380` to the client's `[Interface]` and re-activate the .conf |
| Some sites load, others don't (esp. sites with large cookies, websockets) | MTU / MSS clamping | Same as above. `MTU = 1280` is a safer universal value |
| DNS works but `https://` sites give "connection reset" | ECN / DSCP bits being mangled along the path | `Table = off` was the old way; for transparent proxy just set `MTU = 1380` |
| Voice/video calls drop every ~2 min | NAT timeout on the WG path is too short | Add `PersistentKeepalive = 25` (already in our generated .conf) |
| Telegram works but WhatsApp doesn't | Some services block known VPN/proxy IP ranges | Test with the host's own IP (turn VPN off) — if it works without VPN, the IP is blocked, not the proxy |
| Random high latency spikes | VLESS server is overloaded or far | Try a different VLESS server, or split-tunnel: in the .conf change `AllowedIPs = 10.0.1.0/24, <some-specific-subnet>` to route only specific subnets through the tunnel |

### On-the-server live debugging

When a client reports problems, the most useful real-time view on
the server is:

```bash
# watch wg0 handshakes and transfer in real time
watch -n 1 'sudo wg show wg0'

# watch xray activity (which destinations clients are trying to reach,
# and whether VLESS handshake succeeded for each)
sudo journalctl -u xray -f | grep -E 'accepted|tunneling|reject'

# watch wgserver syncer (peer creation / removal from admin UI / bot)
sudo journalctl -u wgserver -f

# check that the daemon's own outbound goes through xray (sanity)
sudo -u wgserver curl -sS --max-time 5 https://ifconfig.io
# should return the VLESS server's IP, not the host's

# full diagnostic (the healthcheck script)
  sudo /usr/local/bin/wgserver-healthcheck
```

If a client is connected but traffic isn't flowing:
```bash
# on the server, check the peer's rx counter — is it increasing?
sudo wg show wg0 transfer
# the peer's "received" counter should grow when the client pings
# or sends traffic. If it stays at 0, packets are not arriving
# (firewall? client AllowedIPs?).
```

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
