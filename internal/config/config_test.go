package config

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleYAML = `
http:
  addr: "127.0.0.1:8080"
  unix_socket: ""
  tls_cert_file: ""
  tls_key_file: ""
  health_addr: "127.0.0.1:9090"

db:
  path: "/var/lib/wgserver/db.sqlite"

exit_wg:
  interface: "wg0"
  listen_port: 51820
  address: "10.0.0.1/24"
  peer:
    endpoint: "vpn.example.com:51820"
    public_key: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
    allowed_ips: "0.0.0.0/0"
    persistent_keepalive: 25

clients:
  interface: "wg1"
  listen_port: 51821
  address: "10.0.1.1/24"
  cidr: "10.0.1.0/24"
  dns_servers:
    - "1.1.1.1"
    - "9.9.9.9"
  endpoint: "vpn.example.com:51821"
  public_key: "PUBKEY_WG1_BASE64="

telegram:
  bot_token: "REPLACE_ME"
  group_chat_id: -1001234567890
  per_user_quota: 2

update:
  enabled: true
  github_repo: "erazumov/wgserver"
  check_interval_minutes: 60
`

func TestLoad_ParsesAllSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wgserver.yaml")
	if err := os.WriteFile(path, []byte(sampleYAML), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got, want := cfg.HTTP.Addr, "127.0.0.1:8080"; got != want {
		t.Errorf("HTTP.Addr = %q, want %q", got, want)
	}
	if got, want := cfg.HTTP.HealthAddr, "127.0.0.1:9090"; got != want {
		t.Errorf("HTTP.HealthAddr = %q, want %q", got, want)
	}
	if got, want := cfg.DB.Path, "/var/lib/wgserver/db.sqlite"; got != want {
		t.Errorf("DB.Path = %q, want %q", got, want)
	}

	if got, want := cfg.ExitWG.Interface, "wg0"; got != want {
		t.Errorf("ExitWG.Interface = %q, want %q", got, want)
	}
	if got, want := cfg.ExitWG.ListenPort, 51820; got != want {
		t.Errorf("ExitWG.ListenPort = %d, want %d", got, want)
	}
	if got, want := cfg.ExitWG.Peer.Endpoint, "vpn.example.com:51820"; got != want {
		t.Errorf("ExitWG.Peer.Endpoint = %q, want %q", got, want)
	}
	if got, want := cfg.ExitWG.Peer.PersistentKeepalive, 25; got != want {
		t.Errorf("ExitWG.Peer.PersistentKeepalive = %d, want %d", got, want)
	}

	if got, want := cfg.Clients.Interface, "wg1"; got != want {
		t.Errorf("Clients.Interface = %q, want %q", got, want)
	}
	if got, want := len(cfg.Clients.DNSServers), 2; got != want {
		t.Errorf("Clients.DNSServers len = %d, want %d", got, want)
	}
	if got, want := cfg.Clients.Endpoint, "vpn.example.com:51821"; got != want {
		t.Errorf("Clients.Endpoint = %q, want %q", got, want)
	}
	if got, want := cfg.Clients.PublicKey, "PUBKEY_WG1_BASE64="; got != want {
		t.Errorf("Clients.PublicKey = %q, want %q", got, want)
	}

	if got, want := cfg.Telegram.BotToken, "REPLACE_ME"; got != want {
		t.Errorf("Telegram.BotToken = %q, want %q", got, want)
	}
	if got, want := cfg.Telegram.GroupChatID, int64(-1001234567890); got != want {
		t.Errorf("Telegram.GroupChatID = %d, want %d", got, want)
	}
	if got, want := cfg.Telegram.PerUserQuota, 2; got != want {
		t.Errorf("Telegram.PerUserQuota = %d, want %d", got, want)
	}

	if !cfg.Update.Enabled {
		t.Errorf("Update.Enabled = false, want true")
	}
	if got, want := cfg.Update.GitHubRepo, "erazumov/wgserver"; got != want {
		t.Errorf("Update.GitHubRepo = %q, want %q", got, want)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("Load: want error for missing file, got nil")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("http: : : not valid"), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load: want error for invalid YAML, got nil")
	}
}
