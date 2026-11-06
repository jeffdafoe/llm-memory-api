// Package sim holds the salem simulation helpers of the Node server's
// sim-conversation-distiller.js and sim-soul.js.
package sim

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// SoulLengthDirective closes every soul-writer prompt: the nightly dream's
// and /sim/soul's. The soul is re-billed on every NPC turn, so both
// writers carry the same condense-don't-grow contract.
const SoulLengthDirective = "Keep the soul document under about 200 words. " +
	"When weaving in new material, condense: fold older detail into fewer, " +
	"stronger sentences and keep only what defines the character. A short, " +
	"sharp soul beats a long memoir."

// maxRawSlugPrefixLength bounds a raw prefix before any work on it. The
// canonical form is at most 100 characters; the raw form can only add
// surrounding whitespace and extra trailing slashes.
const maxRawSlugPrefixLength = 1024

// CollapseTrailingSlashes reduces a run of trailing slashes to exactly one
// (and adds one when there is none). A loop, not a regex: Node's comment
// records the O(n^2) backtracking of the obvious pattern.
func CollapseTrailingSlashes(value string) string {
	return strings.TrimRight(value, "/") + "/"
}

// NormalizeSlugPrefix validates and canonicalizes a shared-VA villager's
// prefix ("constance-scott/"): lowercase-kebab segments with exactly one
// trailing slash, at most 100 characters. Anything else gives "", which
// callers refuse. Non-string values are the caller's to refuse (Node
// returned "" for them).
func NormalizeSlugPrefix(raw string) string {
	if jsutil.UTF16Len(raw) > maxRawSlugPrefixLength {
		return ""
	}
	trimmed := jsutil.Trim(raw)
	if trimmed == "" {
		return ""
	}
	withSlash := CollapseTrailingSlashes(trimmed)
	if !kebabPath(withSlash) || len(withSlash) > 100 {
		return ""
	}
	return withSlash
}

// kebabPath is /^[a-z0-9]+(-[a-z0-9]+)*\/$/.
func kebabPath(s string) bool {
	body, ok := strings.CutSuffix(s, "/")
	if !ok || body == "" {
		return false
	}
	for _, seg := range strings.Split(body, "-") {
		if seg == "" {
			return false
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') {
				return false
			}
		}
	}
	return true
}

var upper = cases.Upper(language.Und)

// SlugToDisplay turns an NPC slug into its display name: the zbbs- prefix
// dropped, then each "-" segment capitalized and joined with spaces
// ("zbbs-john-ellis" is "John Ellis"). A name that is already a display
// name comes back unchanged.
func SlugToDisplay(name string) string {
	if name == "" {
		return ""
	}
	stripped := strings.TrimPrefix(name, "zbbs-")
	var words []string
	for _, w := range strings.Split(stripped, "-") {
		if w == "" {
			continue
		}
		words = append(words, upperFirst(w))
	}
	return strings.Join(words, " ")
}

// upperFirst is w.charAt(0).toUpperCase() + w.slice(1). charAt takes one
// UTF-16 unit, so a character outside the BMP is split into its surrogate
// halves, which toUpperCase leaves alone: the word comes back unchanged.
func upperFirst(w string) string {
	r, size := utf8.DecodeRuneInString(w)
	if r > 0xFFFF {
		return w
	}
	return upper.String(w[:size]) + w[size:]
}
