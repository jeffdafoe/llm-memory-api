// Package config holds the rows of the `config` table in memory. The table is
// read once at startup; the server must not start if that read fails.
package config

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
)

// Store is the in-memory copy of the config table.
type Store struct {
	mu     sync.RWMutex
	values map[string]string
}

func NewStore() *Store {
	return &Store{values: map[string]string{}}
}

// Load reads every config row into the store.
func (s *Store) Load(ctx context.Context, conn db.DB) error {
	rows, err := conn.Query(ctx, "SELECT key, value FROM config")
	if err != nil {
		return err
	}
	defer rows.Close()

	loaded := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		loaded[key] = value
	}
	if err := rows.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	for key, value := range loaded {
		s.values[key] = value
	}
	s.mu.Unlock()
	return nil
}

// Get returns a config value. A missing key is an error, so a misconfigured
// deployment fails loudly instead of running on a silent default.
func (s *Store) Get(key string) (string, error) {
	s.mu.RLock()
	value, ok := s.values[key]
	s.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("Config key \"%s\" not found. Was config.init() called?", key)
	}
	return value, nil
}

// Lookup returns a config value and whether the key exists.
func (s *Store) Lookup(key string) (string, bool) {
	s.mu.RLock()
	value, ok := s.values[key]
	s.mu.RUnlock()
	return value, ok
}

// Set updates a cached value after the caller has written it to the table.
func (s *Store) Set(key, value string) {
	s.mu.Lock()
	s.values[key] = value
	s.mu.Unlock()
}

// ParseNonNegativeFinite reads a config value that must be a non-negative
// number. It returns fallback when the value is absent (present=false), blank,
// unparseable, negative or not finite.
//
// Zero is a real value here: several keys document "0 to disable". Blank is
// checked first because JavaScript's Number("") is 0, and the Node version had
// to stop a blank row from reading as an explicit zero; this keeps the same
// rule.
func ParseNonNegativeFinite(value string, present bool, fallback float64) float64 {
	if !present {
		return fallback
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	n, ok := parseJSNumber(trimmed)
	if !ok || math.IsInf(n, 0) || math.IsNaN(n) || n < 0 {
		return fallback
	}
	return n
}

// parseJSNumber accepts the string forms JavaScript's Number() accepts for
// config values: decimal and exponent notation, and unsigned 0x / 0o / 0b
// integer literals. The caller has already trimmed whitespace.
func parseJSNumber(s string) (float64, bool) {
	// JavaScript never accepts digit separators; strconv does in some forms.
	if strings.Contains(s, "_") {
		return 0, false
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "0x") || strings.HasPrefix(lower, "0o") || strings.HasPrefix(lower, "0b") {
		n, err := strconv.ParseUint(s, 0, 64)
		if err != nil {
			return 0, false
		}
		return float64(n), true
	}
	// strconv accepts "inf", "infinity" and "nan" in any case; JavaScript
	// accepts only "Infinity". Both end up rejected as not finite, so the
	// difference does not matter here.
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
