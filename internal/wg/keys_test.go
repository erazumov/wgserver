package wg

import (
	"errors"
	"testing"
)

func TestGenerateKeyPair_Success(t *testing.T) {
	priv, pub := "PRIVKEY_BASE64", "PUBKEY_BASE64"
	r := &fakeRunner{
		outputs: map[string]string{
			"wg genkey": priv,
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
	if len(r.outputCalls) != 2 {
		t.Fatalf("output calls = %d, want 2", len(r.outputCalls))
	}
	if r.outputCalls[0].Name != "wg" || r.outputCalls[0].Args[0] != "genkey" {
		t.Errorf("first call = %+v", r.outputCalls[0])
	}
	if r.outputCalls[1].Name != "wg" || r.outputCalls[1].Args[0] != "pubkey" {
		t.Errorf("second call = %+v", r.outputCalls[1])
	}
	// wg pubkey takes a key file path as its only arg after the subcommand.
	if len(r.outputCalls[1].Args) != 2 || r.outputCalls[1].Args[0] != "pubkey" {
		t.Errorf("wg pubkey args = %v, want [pubkey <privPath>]", r.outputCalls[1].Args)
	}
}

func TestGenerateKeyPair_GenkeyError(t *testing.T) {
	r := &fakeRunner{
		outputs: map[string]string{},
		runErr:  nil,
	}
	// Force the second call (Output) to fail by not registering "wg genkey" output.
	// fakeRunner.Output returns an error if the key isn't in the map.
	_, err := GenerateKeyPair(r)
	if err == nil {
		t.Fatal("GenerateKeyPair: want error when wg genkey not configured, got nil")
	}
}

func TestGenerateKeyPair_EmptyKeysRejected(t *testing.T) {
	r := &fakeRunner{
		outputs: map[string]string{
			"wg genkey": "",
			"wg pubkey": "",
		},
	}
	_, err := GenerateKeyPair(r)
	if err == nil {
		t.Fatal("GenerateKeyPair: want error on empty keys, got nil")
	}
	if !errors.Is(err, errEmptyKey) {
		t.Errorf("err = %v, want errEmptyKey", err)
	}
}
