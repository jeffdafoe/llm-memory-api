package contract

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Placeholders the normalizer writes.
const (
	TimestampMark = "<ts>"
	OriginMark    = "<origin>"
	maskedRandom  = "<rand>"
)

var (
	// An ISO 8601 date-time as Date.toISOString writes it, or as Postgres
	// renders timestamp / timestamptz through to_jsonb or ::text. A date with
	// no time part is left alone: it is usually data, not a clock reading.
	timestampRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d{1,9})?)?(?:Z|[+-]\d{2}(?::?\d{2})?)?`)
	// A Date as String(date) writes it (Date.prototype.toString), which the
	// MCP tools print in their texts: "Fri Nov 06 2026 18:21:15 GMT+0000
	// (Coordinated Universal Time)".
	dateStringRe = regexp.MustCompile(`(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun) (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) \d{2} -?\d{4} \d{2}:\d{2}:\d{2} GMT[+-]\d{4} \([^()]*\)`)
	uuidRe       = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	// A run of token characters. Leftmost-greedy matching of a character
	// class finds maximal runs, so a match never starts or ends inside a
	// longer run.
	tokenRunRe = regexp.MustCompile(`[A-Za-z0-9_-]{32,}`)
	hexRe      = regexp.MustCompile(`^[0-9A-Fa-f]{32,}$`)
)

// Normalizer rewrites the values that differ between two correct runs:
// timestamps become <ts>, each server's own origin becomes <origin>, and
// random values (UUIDs, hex of 32 or more characters, mixed-case base64url
// runs of 32 or more) become <rand-1>, <rand-2>, ... in the order they are
// first seen. One Normalizer is used per server per scenario, and both see
// the results in the same order, so the same token coming back in a later
// response gets the same name on both sides and the link is checked too.
type Normalizer struct {
	origins []string
	names   map[string]string
	// mask, when set, is the Normalizer whose names this one only reads:
	// a value it already named keeps that name, any other random value
	// becomes <rand>.
	mask *Normalizer
	// literals are raw values a step declared random (Step.Random).
	literals []string
}

// AddRandom declares raw a random value: a JSON string or number equal to
// it, as a whole value, is renamed like any other random value. Text that
// merely contains it is left alone, so a short value cannot mask an
// unrelated difference.
func (n *Normalizer) AddRandom(raw string) {
	if raw == "" || n.isLiteral(raw) {
		return
	}
	n.literals = append(n.literals, raw)
}

func (n *Normalizer) isLiteral(s string) bool {
	for _, l := range n.literals {
		if l == s {
			return true
		}
	}
	return false
}

// NewNormalizer returns a Normalizer for one server. origins are that
// server's own base URLs ("http://127.0.0.1:41234"); each is replaced, and so
// is its host:port alone.
func NewNormalizer(origins ...string) *Normalizer {
	n := &Normalizer{names: map[string]string{}}
	for _, o := range origins {
		if o == "" {
			continue
		}
		n.origins = append(n.origins, o)
		if i := strings.Index(o, "://"); i >= 0 {
			n.origins = append(n.origins, o[i+3:])
		}
	}
	return n
}

// masking returns a Normalizer for the sort key of a multiset, where
// first-seen numbering would depend on the order being decided. A value an
// earlier response already named keeps its <rand-N>, so two rows that differ
// only in which known token they carry still sort apart; a value not yet
// named is <rand>.
func (n *Normalizer) masking() *Normalizer {
	return &Normalizer{origins: n.origins, mask: n, literals: n.literals}
}

// Names returns the placeholder each raw random value got, for debugging a
// failed case.
func (n *Normalizer) Names() map[string]string {
	out := make(map[string]string, len(n.names))
	for k, v := range n.names {
		out[k] = v
	}
	return out
}

func (n *Normalizer) random(raw string) string {
	if n.mask != nil {
		if name, ok := n.mask.names[raw]; ok {
			return name
		}
		return maskedRandom
	}
	if name, ok := n.names[raw]; ok {
		return name
	}
	name := "<rand-" + strconv.Itoa(len(n.names)+1) + ">"
	n.names[raw] = name
	return name
}

// String normalizes one string.
func (n *Normalizer) String(s string) string {
	for _, o := range n.origins {
		s = strings.ReplaceAll(s, o, OriginMark)
	}
	s = dateStringRe.ReplaceAllString(s, TimestampMark)
	s = timestampRe.ReplaceAllString(s, TimestampMark)
	s = uuidRe.ReplaceAllStringFunc(s, n.random)
	s = tokenRunRe.ReplaceAllStringFunc(s, func(run string) string {
		if hexRe.MatchString(run) {
			return n.random(run)
		}
		// A long identifier such as "key_<64 hex>": rename only the hex
		// parts, so the readable prefix stays in the diff.
		parts := splitKeepSeparators(run)
		renamed := false
		for i, p := range parts {
			if hexRe.MatchString(p) {
				parts[i] = n.random(p)
				renamed = true
			}
		}
		if renamed {
			return strings.Join(parts, "")
		}
		if isMixedToken(run) {
			return n.random(run)
		}
		return run
	})
	return s
}

// isMixedToken reports whether a token run looks random: an upper-case
// letter, a lower-case letter and a digit, or, from 40 characters, any two
// of the three. Identifiers and words rarely qualify. The longer rule covers
// the session tokens (48 random bytes as 64 base64url characters), about one
// in 50,000 of which has no digit or no capital.
func isMixedToken(s string) bool {
	var upper, lower, digit bool
	for _, c := range s {
		switch {
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= '0' && c <= '9':
			digit = true
		}
	}
	classes := 0
	for _, has := range []bool{upper, lower, digit} {
		if has {
			classes++
		}
	}
	return classes == 3 || (classes == 2 && len(s) >= 40)
}

// splitKeepSeparators splits on '_' and '-' and keeps each separator as its
// own element, so joining the parts gives the input back.
func splitKeepSeparators(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '_' || s[i] == '-' {
			if i > start {
				parts = append(parts, s[start:i])
			}
			parts = append(parts, s[i:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		parts = append(parts, s[start:])
	}
	return parts
}

// Value normalizes every string in v, object keys included. Numbers are kept
// as written.
func (n *Normalizer) Value(v Value) Value {
	switch v.Kind {
	case Number:
		// The placeholder stays a number, so a number on one side and a
		// string on the other still differ.
		if n.isLiteral(v.Num) {
			return Value{Kind: Number, Num: n.random(v.Num)}
		}
	case String:
		if n.isLiteral(v.Str) {
			return Str(n.random(v.Str))
		}
		return Str(n.String(v.Str))
	case Array:
		out := Value{Kind: Array, Items: make([]Value, len(v.Items))}
		for i, item := range v.Items {
			out.Items[i] = n.Value(item)
		}
		return out
	case Object:
		out := Value{Kind: Object, Members: make([]Member, len(v.Members))}
		for i, m := range v.Members {
			out.Members[i] = Member{Key: n.String(m.Key), Value: n.Value(m.Value)}
		}
		return out
	}
	return v
}

// SortMultiset orders values whose order does not matter (rows written by
// background goroutines, parallel provider calls) by their normalized form,
// with values not yet named masked. Sorting before numbering makes the
// <rand-N> names come out the same on both sides whatever order the server
// wrote in. Ties keep their input order; values that tie differ at most in
// random values first seen in this multiset, and such a tie can still number
// them differently on the two sides.
func (n *Normalizer) SortMultiset(values []Value) []Value {
	m := n.masking()
	keys := make([]string, len(values))
	for i, v := range values {
		keys[i] = m.Value(v).Compact()
	}
	idx := make([]int, len(values))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return keys[idx[a]] < keys[idx[b]] })
	out := make([]Value, len(values))
	for i, j := range idx {
		out[i] = values[j]
	}
	return out
}
