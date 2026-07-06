package web

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

type Session struct {
	ID        string
	AdminID   int64
	Username  string
	CreatedAt time.Time
	ExpiresAt time.Time

	csrfToken string
}

func (s *Session) CSRFToken() string { return s.csrfToken }

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	ttl      time.Duration
}

func NewSessionStore(ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &SessionStore{
		sessions: map[string]*Session{},
		ttl:      ttl,
	}
}

func (s *SessionStore) Create(adminID int64, username string) (*Session, error) {
	id, err := randomToken(32)
	if err != nil {
		return nil, fmt.Errorf("session id: %w", err)
	}
	csrf, err := randomToken(32)
	if err != nil {
		return nil, fmt.Errorf("csrf token: %w", err)
	}
	now := time.Now()
	sess := &Session{
		ID:        id,
		AdminID:   adminID,
		Username:  username,
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
		csrfToken: csrf,
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	return sess, nil
}

func (s *SessionStore) Get(id string) (*Session, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(sess.ExpiresAt) {
		s.Destroy(id)
		return nil, false
	}
	return sess, true
}

func (s *SessionStore) Destroy(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *SessionStore) GC() {
	s.mu.Lock()
	now := time.Now()
	for id, sess := range s.sessions {
		if now.After(sess.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
}

func randomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
