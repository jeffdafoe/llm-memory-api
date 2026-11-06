// Package peopleslug is Node's people-slug.js: the slug of a
// context/people/<slug> note, built from a display name.
package peopleslug

import (
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Slug is personContextSlug: the name trimmed, NFKD-decomposed,
// lower-cased, whitespace runs turned into "-", then only [a-z0-9-] kept,
// "-" runs collapsed and one edge hyphen dropped at each end. "" (Node's
// null) when nothing is left, so a name like "../x" cannot build a path
// outside context/people/. Node strips the combining marks U+0300-U+036F
// before lower-casing; the [a-z0-9-] filter drops them all the same.
func Slug(name string) string {
	if name == "" {
		return ""
	}
	lowered := jsutil.ToLower(norm.NFKD.String(jsutil.Trim(name)))
	var b strings.Builder
	space := false
	for _, r := range lowered {
		if jsutil.IsSpace(r) {
			if !space {
				b.WriteByte('-')
			}
			space = true
			continue
		}
		space = false
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	var out strings.Builder
	prev := byte(0)
	for i := 0; i < b.Len(); i++ {
		c := b.String()[i]
		if c == '-' && prev == '-' {
			continue
		}
		out.WriteByte(c)
		prev = c
	}
	// replace(/^-|-$/g, ''): one hyphen at each end; the collapse above
	// leaves no more than one there.
	return strings.TrimSuffix(strings.TrimPrefix(out.String(), "-"), "-")
}
