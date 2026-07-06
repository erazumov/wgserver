package wg

import (
	"strings"
	"testing"
)

func TestGenerateClientConfig_BasicShape(t *testing.T) {
	cfg := ClientConfig{
		ClientPrivateKey: "CLIENT_PRIV",
		ClientAddress:    "10.0.1.2/32",
		DNSServers:       []string{"1.1.1.1", "9.9.9.9"},

		ServerPublicKey: "SERVER_PUB",
		ServerEndpoint:  "vpn.example.com:51821",
		PresharedKey:    "PSK_BASE64",
		AllowedIPs:      "0.0.0.0/0, ::/0",
		Keepalive:       25,
	}
	got := GenerateClientConfig(cfg)

	wants := []string{
		"[Interface]",
		"PrivateKey = CLIENT_PRIV",
		"Address = 10.0.1.2/32",
		"DNS = 1.1.1.1, 9.9.9.9",
		"[Peer]",
		"PublicKey = SERVER_PUB",
		"PresharedKey = PSK_BASE64",
		"Endpoint = vpn.example.com:51821",
		"AllowedIPs = 0.0.0.0/0, ::/0",
		"PersistentKeepalive = 25",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q\n---\n%s\n---", w, got)
		}
	}

	// [Interface] must appear before [Peer].
	i := strings.Index(got, "[Interface]")
	if i < 0 {
		t.Fatal("output missing [Interface]")
	}
	if j := strings.Index(got, "[Peer]"); j < 0 {
		t.Fatal("output missing [Peer]")
	} else if j < i {
		t.Errorf("[Peer] (idx %d) appears before [Interface] (idx %d)", j, i)
	}
}

func TestGenerateClientConfig_NoDNS(t *testing.T) {
	cfg := ClientConfig{
		ClientPrivateKey: "PK",
		ClientAddress:    "10.0.1.2/32",
		ServerPublicKey:  "SPK",
		ServerEndpoint:   "host:1",
		AllowedIPs:       "0.0.0.0/0",
	}
	got := GenerateClientConfig(cfg)
	if strings.Contains(got, "DNS =") {
		t.Errorf("output contains DNS line when DNSServers empty:\n%s", got)
	}
}

func TestGenerateClientConfig_NoPresharedKey(t *testing.T) {
	cfg := ClientConfig{
		ClientPrivateKey: "PK",
		ClientAddress:    "10.0.1.2/32",
		ServerPublicKey:  "SPK",
		ServerEndpoint:   "host:1",
		AllowedIPs:       "0.0.0.0/0",
	}
	got := GenerateClientConfig(cfg)
	if strings.Contains(got, "PresharedKey") {
		t.Errorf("output contains PresharedKey when empty:\n%s", got)
	}
}

func TestGenerateClientConfig_Deterministic(t *testing.T) {
	cfg := ClientConfig{
		ClientPrivateKey: "PK",
		ClientAddress:    "10.0.1.2/32",
		ServerPublicKey:  "SPK",
		ServerEndpoint:   "host:1",
		AllowedIPs:       "0.0.0.0/0",
		Keepalive:        25,
	}
	a := GenerateClientConfig(cfg)
	b := GenerateClientConfig(cfg)
	if a != b {
		t.Errorf("not deterministic:\n%s\n---\n%s", a, b)
	}
}
