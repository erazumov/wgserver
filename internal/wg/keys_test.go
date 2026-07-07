package wg

import (
	"errors"
	"strings"
	"testing"
)

func TestGenerateKeyPair_Success(t *testing.T) {
	priv, pub := "PRIVKEY_BASE64", "PUBKEY_BASE64"
	r := &fakeRunner{
		outputs: map[string]string{
			"wg genkey": priv,
		},
		// wg pubkey is now called via OutputStdin (stdin = the temp
		// file with the private key). The fake's OutputStdin looks
		// up by "name args" key, so we register the expected pubkey
		// there. We also want to assert that the stdin reader was
		// actually wired up and that reading it returns the priv key.
		stdinOutputs: map[string]string{
			"wg pubkey": pub,
		},
	}
	kp, err := GenerateKeyPair(r)
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if kp.PrivateKey != priv {
		t.Errorf("PrivateKey = %q, want %q", kp.PrivateKey, priv)
	}
	if kp.PublicKey != pub {
		t.Errorf("PublicKey = %q, want %q", kp.PublicKey, pub)
	}
	if len(r.outputCalls) != 1 {
		t.Fatalf("Output calls = %d, want 1 (only wg genkey goes through Output)", len(r.outputCalls))
	}
	if r.outputCalls[0].Name != "wg" || r.outputCalls[0].Args[0] != "genkey" {
		t.Errorf("first call = %+v", r.outputCalls[0])
	}
	if len(r.stdinCalls) != 1 {
		t.Fatalf("OutputStdin calls = %d, want 1", len(r.stdinCalls))
	}
	if r.stdinCalls[0].Name != "wg" || r.stdinCalls[0].Args[0] != "pubkey" {
		t.Errorf("OutputStdin call = %+v", r.stdinCalls[0])
	}
	// Confirm wg pubkey was fed SOME non-nil reader as stdin (i.e.
	// the caller-side fix from "wg pubkey <file> as argument" →
	// stdin is wired up correctly). We deliberately do NOT read
	// from the reader here: the production code no longer closes
	// the file (see the comment in keys.go — the runner owns the
	// reader's lifetime), so a read here would race with whatever
	// the runner is doing with it.
	if r.stdinCalls[0].StdinR == nil {
		t.Fatal("OutputStdin: stdin reader is nil; pubkey would read /dev/null")
	}
}

func TestGenerateKeyPair_GenkeyError(t *testing.T) {
	r := &fakeRunner{
		outputs:      map[string]string{},
		runErr:       nil,
		stdinOutputs: map[string]string{},
	}
	// Force the first call (Output for wg genkey) to fail by not
	// registering the output. fakeRunner.Output returns an error
	// if the key isn't in the map.
	_, err := GenerateKeyPair(r)
	if err == nil {
		t.Fatal("GenerateKeyPair: want error when wg genkey not configured, got nil")
	}
}

func TestGenerateKeyPair_PubkeyError(t *testing.T) {
	priv := "PRIVKEY_BASE64"
	r := &fakeRunner{
		outputs: map[string]string{
			"wg genkey": priv,
		},
		// No stdinOutputs entry → fakeRunner.OutputStdin returns
		// "no output configured" error, exercising the wg pubkey
		// failure path.
		stdinOutputs: map[string]string{},
	}
	_, err := GenerateKeyPair(r)
	if err == nil {
		t.Fatal("GenerateKeyPair: want error when wg pubkey fails, got nil")
	}
	if !strings.Contains(err.Error(), "wg pubkey") {
		t.Errorf("err = %v, want it to mention wg pubkey", err)
	}
}

func TestGenerateKeyPair_EmptyKeysRejected(t *testing.T) {
	r := &fakeRunner{
		outputs: map[string]string{
			"wg genkey": "",
		},
		stdinOutputs: map[string]string{
			"wg pubkey": "",
		},
	}
	_, err := GenerateKeyPair(r)
	if err == nil {
		t.Fatal("GenerateKeyPair: want error on empty priv, got nil")
	}
	if !errors.Is(err, errEmptyKey) {
		t.Errorf("err = %v, want errEmptyKey", err)
	}
}
