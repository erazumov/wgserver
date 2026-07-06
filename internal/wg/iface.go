package wg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AddPeer applies `wg set <iface> peer <pub> allowed-ips <ip>` and, if
// a preshared key is given, also writes the PSK to pskDir/<safe-pubkey>
// and passes that file path as the PSK value.
//
// Why a file, not an inline value: wireguard-tools 1.0.20210914 (what
// Debian 12 ships) parses `preshared-key` in `wg set` exclusively as a
// file path through parse_keyfile() (config.c). Inline base64 is not
// supported. Newer wireguard-tools accept both, but we cannot assume
// them. The PSK is a short secret; writing it to a wgserver-owned
// file is no worse than storing it in the DB.
func AddPeer(r Runner, iface, publicKey, allowedIPs, pskDir, presharedKey string) error {
	args := []string{"set", iface, "peer", publicKey, "allowed-ips", allowedIPs}
	if presharedKey != "" {
		path, err := writePSKFile(pskDir, publicKey, presharedKey)
		if err != nil {
			return err
		}
		args = append(args, "preshared-key", path)
	}
	if err := r.Run("wg", args...); err != nil {
		return fmt.Errorf("wg add peer on %s: %w", iface, err)
	}
	return nil
}

// RemovePeer applies `wg set <iface> peer <pub> remove` and removes
// the PSK file written by AddPeer (best-effort).
func RemovePeer(r Runner, iface, pskDir, publicKey string) error {
	if err := r.Run("wg", "set", iface, "peer", publicKey, "remove"); err != nil {
		return fmt.Errorf("wg remove peer on %s: %w", iface, err)
	}
	_ = os.Remove(pskFilePath(pskDir, publicKey))
	return nil
}

// pskFilePath returns a deterministic per-peer path under dir. The
// pubkey is base64 (A-Za-z0-9+/=) and would produce unsafe filenames
// ("/" in base64). The replacer makes it a valid single-component
// filename without lossy collisions: base64's "+" and "/" both
// become unique single characters.
func pskFilePath(dir, publicKey string) string {
	safe := strings.NewReplacer("/", "_", "+", "-").Replace(publicKey)
	return filepath.Join(dir, safe)
}

func writePSKFile(dir, publicKey, psk string) (string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("mkdir psk dir: %w", err)
	}
	path := pskFilePath(dir, publicKey)
	if err := os.WriteFile(path, []byte(psk+"\n"), 0600); err != nil {
		return "", fmt.Errorf("write psk: %w", err)
	}
	return path, nil
}
