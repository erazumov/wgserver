package web

import (
	"crypto/hmac"
	"testing"
)

func TestSignAndVerify(t *testing.T) {
	key := []byte("test-secret-key-32-bytes-long!!!")
	sessionID := "abc123"

	tok := SignCSRF(key, sessionID)
	if tok == "" {
		t.Fatal("SignCSRF returned empty string")
	}
	if !VerifyCSRF(key, sessionID, tok) {
		t.Error("VerifyCSRF: valid token rejected")
	}
}

func TestVerifyCSRF_WrongKey(t *testing.T) {
	tok := SignCSRF([]byte("key-a"), "sess-1")
	if VerifyCSRF([]byte("key-b"), "sess-1", tok) {
		t.Error("VerifyCSRF: token signed with different key accepted")
	}
}

func TestVerifyCSRF_WrongSessionID(t *testing.T) {
	key := []byte("k")
	tok := SignCSRF(key, "session-original")
	if VerifyCSRF(key, "session-attacker", tok) {
		t.Error("VerifyCSRF: token accepted for different session ID")
	}
}

func TestVerifyCSRF_TamperedToken(t *testing.T) {
	key := []byte("k")
	tok := SignCSRF(key, "sess-1")
	tampered := tok[:len(tok)-2] + "AA"
	if VerifyCSRF(key, "sess-1", tampered) {
		t.Error("VerifyCSRF: tampered token accepted")
	}
}

func TestVerifyCSRF_EmptyToken(t *testing.T) {
	if VerifyCSRF([]byte("k"), "sess-1", "") {
		t.Error("VerifyCSRF: empty token accepted")
	}
}

func TestVerifyCSRF_ConstantTimeCompare(t *testing.T) {
	// Sanity: two tokens derived from the same input are equal.
	key := []byte("k")
	a := SignCSRF(key, "sess-1")
	b := SignCSRF(key, "sess-1")
	if !hmac.Equal([]byte(a), []byte(b)) {
		t.Error("equal tokens not equal")
	}
}
