package wg

import "fmt"

func AddPeer(r Runner, iface, publicKey, allowedIPs, presharedKey string) error {
	args := []string{"set", iface, "peer", publicKey, "allowed-ips", allowedIPs}
	if presharedKey != "" {
		args = append(args, "preshared-key", presharedKey)
	}
	if err := r.Run("wg", args...); err != nil {
		return fmt.Errorf("wg add peer on %s: %w", iface, err)
	}
	return nil
}

func RemovePeer(r Runner, iface, publicKey string) error {
	if err := r.Run("wg", "set", iface, "peer", publicKey, "remove"); err != nil {
		return fmt.Errorf("wg remove peer on %s: %w", iface, err)
	}
	return nil
}
