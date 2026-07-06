package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	HTTP     HTTPConfig     `yaml:"http"`
	DB       DBConfig       `yaml:"db"`
	ExitWG   ExitWGConfig   `yaml:"exit_wg"`
	Clients  ClientsConfig  `yaml:"clients"`
	Telegram TelegramConfig `yaml:"telegram"`
	Update   UpdateConfig   `yaml:"update"`
}

type HTTPConfig struct {
	// Addr is the admin UI listen address (host:port) for plain HTTP
	// or HTTPS. Ignored when UnixSocket is set.
	Addr string `yaml:"addr"`

	// UnixSocket, if non-empty, makes the admin UI listen on this
	// path instead of Addr. Recommended over a public TCP port.
	// Must be writable by the wgserver process; install.sh sets up
	// the directory with mode 0700 owned by root.
	UnixSocket string `yaml:"unix_socket"`

	// TLSCertFile and TLSKeyFile enable HTTPS on the admin listener.
	// Both must be set (or both empty). Ignored when UnixSocket is set.
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`

	// HealthAddr, if set, exposes /healthz on a separate TCP listener
	// (always plain HTTP). The wgserver-updater polls this to detect
	// failed updates. Defaults to Addr when empty and Addr is TCP —
	// but for UnixSocket or TLS, HealthAddr MUST be set, otherwise
	// the updater cannot reach /healthz.
	HealthAddr string `yaml:"health_addr"`
}

type DBConfig struct {
	Path string `yaml:"path"`
}

type ExitWGConfig struct {
	Interface  string `yaml:"interface"`
	ListenPort int    `yaml:"listen_port"`
	Address    string `yaml:"address"`
	Peer       WGPeer `yaml:"peer"`
}

type WGPeer struct {
	Endpoint            string `yaml:"endpoint"`
	PublicKey           string `yaml:"public_key"`
	AllowedIPs          string `yaml:"allowed_ips"`
	PersistentKeepalive int    `yaml:"persistent_keepalive"`
}

type ClientsConfig struct {
	Interface  string   `yaml:"interface"`
	ListenPort int      `yaml:"listen_port"`
	Address    string   `yaml:"address"`
	CIDR       string   `yaml:"cidr"`
	DNSServers []string `yaml:"dns_servers"`

	// Endpoint is the externally-reachable host:port that clients dial.
	// May differ from listen_port when the server is behind NAT/port-forward.
	Endpoint string `yaml:"endpoint"`

	// PublicKey is the wg1 interface's public key (base64). It goes into
	// the [Peer] section of every client .conf we hand out. Private key
	// for this interface is not in config — it is generated and persisted
	// at first boot.
	PublicKey string `yaml:"public_key"`
}

type TelegramConfig struct {
	BotToken     string `yaml:"bot_token"`
	GroupChatID  int64  `yaml:"group_chat_id"`
	PerUserQuota int    `yaml:"per_user_quota"`
}

type UpdateConfig struct {
	Enabled              bool   `yaml:"enabled"`
	GitHubRepo           string `yaml:"github_repo"`
	CheckIntervalMinutes int    `yaml:"check_interval_minutes"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &c, nil
}
