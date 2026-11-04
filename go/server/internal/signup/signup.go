// Package signup holds the signup-funnel stamps (LLM-734) that the auth layer
// writes: an account's first MCP connection. The tool-call stamp and the
// admin funnel view move with the MCP endpoint and the admin API.
package signup

import (
	"regexp"
	"strconv"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
)

// ConnectedSQL is the SET-clause fragment the MCP heartbeat folds into its
// UPDATE. $2 is TrackingSince(). A NULL $2 compares as NULL, so the CASE never
// stamps: an account older than the tracking start has an unknown true first
// use, and stamping its next use as "first" would read as fact.
const ConnectedSQL = `first_connected_at = COALESCE(first_connected_at,
                 CASE WHEN created_at >= $2::timestamptz THEN NOW() END)`

// isoDate matches the ISO 8601 forms JavaScript's Date.parse accepts and
// Postgres's timestamptz cast also reads: a date, optionally a T or space and
// hh:mm, optional :ss and fraction, optional Z or offset (+hh, +hhmm,
// +hh:mm). Migration MEM-150 writes "YYYY-MM-DDTHH:MM:SS.ffffffZ".
//
// Values V8 takes but Postgres refuses are refused too, so a bad config row
// stops the stamping instead of failing the heartbeat's UPDATE: offsets past
// 15:59 (V8 goes to 23:59) and year 0000.
//
// V8 also accepts free-form dates ("10/09/2026", "Oct 9 2026"). Those are
// not accepted here and read as "not a date", which stamps nothing.
var isoDate = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2})(?:\.\d+)?)?(?:Z|[+-](\d{2})(?::?(\d{2}))?)?)?$`)

func validISODate(value string) bool {
	m := isoDate.FindStringSubmatch(value)
	if m == nil {
		return false
	}
	if m[1] == "0000" {
		return false
	}
	if _, err := time.Parse("2006-01-02", m[1]+"-"+m[2]+"-"+m[3]); err != nil {
		return false
	}
	if m[4] == "" {
		return true
	}
	hour, _ := strconv.Atoi(m[4])
	minute, _ := strconv.Atoi(m[5])
	second := 0
	if m[6] != "" {
		second, _ = strconv.Atoi(m[6])
	}
	if hour >= 24 || minute >= 60 || second >= 60 {
		return false
	}
	if m[7] != "" {
		offsetHour, _ := strconv.Atoi(m[7])
		offsetMinute := 0
		if m[8] != "" {
			offsetMinute, _ = strconv.Atoi(m[8])
		}
		if offsetHour > 15 || offsetMinute >= 60 {
			return false
		}
	}
	return true
}

// TrackingSince returns config.signup_funnel_since, or nil when it is blank or
// not a date (then no first_* stamp is written: fail closed). A missing key is
// an error, as config.get threw in Node.
func TrackingSince(store *config.Store) (*string, error) {
	value, err := store.Get("signup_funnel_since")
	if err != nil {
		return nil, err
	}
	if !validISODate(value) {
		return nil, nil
	}
	return &value, nil
}
