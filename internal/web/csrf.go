package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// SignCSRF returns a base64 HMAC-SHA256(secret, sessionID). The token
// is safe to embed in a hidden form field on pages served to the
// authenticated admin. An attacker without the secret cannot produce
// a valid token; the session ID is not guessable either, so binding
// the token to it prevents reuse across sessions.
func SignCSRF(secret []byte, sessionID string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyCSRF re-derives the token from the session ID and compares in
// constant time. Always returns false on any error rather than
// distinguishing failure modes to the caller.
func VerifyCSRF(secret []byte, sessionID, token string) bool {
	if token == "" {
		return false
	}
	want, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(sessionID))
	return hmac.Equal(mac.Sum(nil), want)
}
