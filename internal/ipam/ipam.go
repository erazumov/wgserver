// Package ipam allocates free IPs inside a CIDR block, skipping the
// network address, broadcast, the server's own address, and any IP
// already assigned to a peer in the store. Used by both the admin web
// handler and the Telegram bot, which must agree on the IP space.
package ipam

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"

	"github.com/erazumov/wgserver/internal/store"
)

// Allocate returns the lowest free /32 inside cidr. serverAddr is the
// server's own address on the interface (e.g. "10.0.1.1/24"); it is
// reserved and never handed to a peer. The returned value is in CIDR
// form (host/32) so it can be stored verbatim in peers.assigned_ip
// and used directly as `wg set ... allowed-ips`.
//
// Returns an error if the CIDR is too small to allocate from, if it
// does not parse, or if every host is taken.
func Allocate(ctx context.Context, db *sql.DB, cidr, serverAddr string) (string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse cidr %s: %w", cidr, err)
	}
	ones, bits := ipnet.Mask.Size()
	if bits-ones < 5 {
		return "", fmt.Errorf("cidr %s too small to allocate from", cidr)
	}

	used, err := store.ListAssignedIPs(ctx, db)
	if err != nil {
		return "", err
	}
	usedSet := make(map[string]bool, len(used)+1)
	for _, u := range used {
		usedSet[u] = true
	}
	if serverAddr != "" {
		host := strings.SplitN(serverAddr, "/", 2)[0]
		usedSet[host+"/32"] = true
	}

	ip := ipnet.IP.Mask(ipnet.Mask)
	for ; ipnet.Contains(ip); incIP(ip) {
		if usedSet[ip.String()+"/32"] {
			continue
		}
		if isNetworkOrBroadcast(ip, ipnet) {
			continue
		}
		return ip.String() + "/32", nil
	}
	return "", fmt.Errorf("no free IPs in %s", cidr)
}

func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] != 0 {
			break
		}
	}
}

func isNetworkOrBroadcast(ip net.IP, cidr *net.IPNet) bool {
	if ip.Equal(cidr.IP) {
		return true
	}
	broadcast := make(net.IP, len(cidr.IP))
	for i := range cidr.IP {
		broadcast[i] = cidr.IP[i] | ^cidr.Mask[i]
	}
	if ip.Equal(broadcast) {
		return true
	}
	return false
}
