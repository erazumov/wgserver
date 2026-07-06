package wg

import (
	"errors"
	"fmt"
	"os"
)

var errEmptyKey = errors.New("wg returned empty key")

type KeyPair struct {
	PrivateKey string
	PublicKey  string
}

// GenerateKeyPair generates a fresh WireGuard key pair by shelling out
// to `wg genkey` and `wg pubkey`. The private key is persisted to a
// temp file and `wg pubkey` is invoked with the file path so the
// private key never appears on a command line. Caller must persist
// KeyPair.PrivateKey before any failure path so the peer can be
// reconstructed deterministically (see AGENTS.md invariant).
func GenerateKeyPair(r Runner) (KeyPair, error) {
	priv, err := r.Output("wg", "genkey")
	if err != nil {
		return KeyPair{}, fmt.Errorf("wg genkey: %w", err)
	}
	if priv == "" {
		return KeyPair{}, errEmptyKey
	}

	f, err := os.CreateTemp("", "wgserver-priv-")
	if err != nil {
		return KeyPair{}, fmt.Errorf("temp privkey: %w", err)
	}
	privPath := f.Name()
	defer func() { _ = os.Remove(privPath) }()

	if _, err := f.WriteString(priv); err != nil {
		_ = f.Close()
		return KeyPair{}, fmt.Errorf("write privkey: %w", err)
	}
	if err := f.Close(); err != nil {
		return KeyPair{}, fmt.Errorf("close privkey: %w", err)
	}

	pub, err := r.Output("wg", "pubkey", privPath)
	if err != nil {
		return KeyPair{}, fmt.Errorf("wg pubkey: %w", err)
	}
	if pub == "" {
		return KeyPair{}, errEmptyKey
	}
	return KeyPair{PrivateKey: priv, PublicKey: pub}, nil
}
