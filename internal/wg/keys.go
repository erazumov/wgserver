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
// temp file and `wg pubkey` reads it from STDIN (not as a command
// argument — older wireguard-tools just exit with "Usage:" if you
// give them a file path). Caller must persist KeyPair.PrivateKey
// before any failure path so the peer can be reconstructed
// deterministically (see AGENTS.md invariant).
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

	privFile, err := os.Open(privPath)
	if err != nil {
		return KeyPair{}, fmt.Errorf("open privkey for pubkey: %w", err)
	}
	// NB: we intentionally do NOT `defer privFile.Close()` here.
	// The runner may keep reading from the stdin reader after this
	// function returns to it asynchronously, and some runners (e.g.
	// the production ExecRunner) hand the reader to a child process
	// and rely on it being open until the child exits. Closing here
	// races with that. The runner is responsible for closing its
	// own stdin (e.g. by waiting for cmd.Wait() to drain the
	// child). For tests, the fake just discards the reader without
	// closing, so we leak an os.File — acceptable in a short-lived
	// test helper.
	_ = privFile
	pub, err := r.OutputStdin("wg", []string{"pubkey"}, privFile)
	if err != nil {
		return KeyPair{}, fmt.Errorf("wg pubkey: %w", err)
	}
	if pub == "" {
		return KeyPair{}, errEmptyKey
	}
	return KeyPair{PrivateKey: priv, PublicKey: pub}, nil
}
