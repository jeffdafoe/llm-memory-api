package dream

import (
	"errors"
	"strconv"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/peopleslug"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sim"
)

// maxDreamDelayMs bounds the operator-set pauses between provider calls
// (dream_interagent_delay, dream_interchunk_delay). In Node it kept a value
// past the 32-bit timer range from turning into no pause at all.
const maxDreamDelayMs = 3600000

// dreamDelayMs reads one of those config rows: parseInt, the fallback
// for an unparseable value, then the cap. 0 stays 0 (the documented
// "disable"), and a negative value is left for the caller's > 0 check. A
// missing key is the error config.get threw.
func (j *Job) dreamDelayMs(key string, fallback float64) (float64, error) {
	raw, err := j.Config.Get(key)
	if err != nil {
		return 0, err
	}
	configured, ok := jsutil.ParseInt(raw)
	if !ok {
		configured = fallback
	}
	return min(configured, maxDreamDelayMs), nil
}

// parseInt10 is parseInt(s, 10) || 0: leading whitespace, a sign, decimal
// digits; NaN and 0 give 0.
func parseInt10(s string) float64 {
	s = jsutil.TrimStart(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	end := 0
	for end < len(s) && isASCIIDigit(s[end]) {
		end++
	}
	if end == 0 {
		return 0
	}
	n, _ := strconv.ParseFloat(s[:end], 64)
	if neg {
		n = -n
	}
	return n
}

// scopePrefix is a dream scope's slug prefix: Absent (nil, undefined, "")
// for a dedicated agent, else the value to validate. Any non-string value
// is refused.
type scopePrefix struct {
	value     string
	notString bool
}

// resolveScopePrefix is the one validated boundary for a scope's prefix:
// "" for an absent prefix (a dedicated agent's namespace root), the
// canonical "<villager>/" for a shared-VA villager, and an error for a
// present prefix that is not already canonical (a non-string, LIKE
// metacharacters, traversal, wrong case, over-length). Coercing a bad
// prefix to "" would merge the villager's notes into the unscoped
// namespace.
func resolveScopePrefix(raw scopePrefix) (string, error) {
	if raw.notString {
		return "", errors.New("invalid slug prefix: not a string")
	}
	if raw.value == "" {
		return "", nil
	}
	canonical := sim.NormalizeSlugPrefix(raw.value)
	if canonical == "" || canonical != raw.value {
		return "", errors.New("invalid slug prefix: " + raw.value)
	}
	return canonical, nil
}

// validatePersonSlug returns the slug when it is already the canonical
// form peopleslug.Slug gives, else "".
func validatePersonSlug(slug string) string {
	canonical := peopleslug.Slug(slug)
	if canonical == "" || canonical != slug {
		return ""
	}
	return canonical
}

// peopleNotePath is the context/people note slug for a person under a
// scope: both the prefix and the person slug are validated, so neither can
// traverse or inject.
func peopleNotePath(prefix scopePrefix, personSlug string) (string, error) {
	safe := validatePersonSlug(personSlug)
	if safe == "" {
		return "", errors.New("invalid person slug: " + personSlug)
	}
	p, err := resolveScopePrefix(prefix)
	if err != nil {
		return "", err
	}
	return p + "context/people/" + safe, nil
}

// validateRosterPrefix is a sim_shared_actor row's prefix at the dream
// boundary: the canonical prefix, or "" for a row to skip (NULL, not
// canonical, or empty: a roster row must name a villager).
func validateRosterPrefix(rowPrefix *string) string {
	if rowPrefix == nil {
		return ""
	}
	p, err := resolveScopePrefix(scopePrefix{value: *rowPrefix})
	if err != nil {
		return ""
	}
	return p
}
