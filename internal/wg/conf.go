package wg

import (
	"fmt"
	"strings"
)

type ClientConfig struct {
	ClientPrivateKey string
	ClientAddress    string
	DNSServers       []string

	ServerPublicKey string
	ServerEndpoint  string
	PresharedKey    string
	AllowedIPs      string
	Keepalive       int
}

// GenerateClientConfig produces a wg-quick .conf that the user can save
// to a file and import into the WireGuard client. Deterministic: same
// inputs always yield the same bytes, so it can be re-derived from
// stored data (see AGENTS.md invariant: peer private key is generated
// once and persisted, .conf is re-derivable).
func GenerateClientConfig(c ClientConfig) string {
	var b strings.Builder
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", c.ClientPrivateKey)
	fmt.Fprintf(&b, "Address = %s\n", c.ClientAddress)
	if len(c.DNSServers) > 0 {
		fmt.Fprintf(&b, "DNS = %s\n", strings.Join(c.DNSServers, ", "))
	}
	b.WriteString("\n[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", c.ServerPublicKey)
	if c.PresharedKey != "" {
		fmt.Fprintf(&b, "PresharedKey = %s\n", c.PresharedKey)
	}
	fmt.Fprintf(&b, "Endpoint = %s\n", c.ServerEndpoint)
	fmt.Fprintf(&b, "AllowedIPs = %s\n", c.AllowedIPs)
	if c.Keepalive > 0 {
		fmt.Fprintf(&b, "PersistentKeepalive = %d\n", c.Keepalive)
	}
	return b.String()
}
