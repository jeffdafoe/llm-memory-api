package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Authorization codes live in memory for 60 seconds and are used once, as in
// Node. They are lost on a restart, which only makes a client in the middle
// of a connection start again.
const (
	authCodeTTL   = 60 * time.Second
	codeSweepEach = 30 * time.Second
)

type authCode struct {
	clientID            string
	redirectURI         string
	codeChallenge       string
	codeChallengeMethod string
	// agent is set only for a public client: the agent who logged in. The
	// token endpoint issues that agent's token and asks for no secret.
	agent     string
	expiresAt time.Time
}

type codeStore struct {
	mu        sync.Mutex
	codes     map[string]authCode
	now       func() time.Time
	lastSweep time.Time
}

func newCodeStore(now func() time.Time) *codeStore {
	if now == nil {
		now = time.Now
	}
	return &codeStore{codes: map[string]authCode{}, now: now, lastSweep: now()}
}

// mint stores c under a new random code and returns the code. Expired codes
// are swept at most every 30 seconds (Node swept on a 30-second timer).
func (s *codeStore) mint(c authCode) string {
	b := make([]byte, 32)
	rand.Read(b)
	code := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if now.Sub(s.lastSweep) >= codeSweepEach {
		s.lastSweep = now
		for k, v := range s.codes {
			if now.After(v.expiresAt) {
				delete(s.codes, k)
			}
		}
	}
	c.expiresAt = now.Add(authCodeTTL)
	s.codes[code] = c
	return code
}

// take removes the code and returns it if it was there and has not expired.
// A presented code is spent either way.
func (s *codeStore) take(code string) (authCode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.codes[code]
	delete(s.codes, code)
	if !ok || s.now().After(c.expiresAt) {
		return authCode{}, false
	}
	return c, true
}

func (s *codeStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.codes)
}

func (s *codeStore) peek(code string) (authCode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.codes[code]
	return c, ok
}
