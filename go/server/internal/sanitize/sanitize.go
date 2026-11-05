// Package sanitize is Node's sanitize.js: the input cleaning applied to
// request fields before they are used.
//
//   - AgentName: identity fields. Trimmed, lowercased, ASCII letters, digits,
//     "-" and "_" only.
//   - Identifier: structural fields (slugs, file names, channels). Trimmed;
//     ASCII letters, digits, "_", "-", ".", "/" and whitespace only. Case is
//     kept: slugs are case-sensitive.
//   - Content: free text. All of Unicode except the control, zero-width and
//     bidi-override characters that break display, logs or comparison.
package sanitize

import (
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// AgentName normalizes an agent name.
func AgentName(s string) string {
	s = jsutil.ToLower(jsutil.Trim(s))
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return -1
	}, s)
}

// Identifier cleans a slug, file name or other structural value. Whitespace
// is JavaScript's \s, so non-ASCII spaces stay.
func Identifier(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '_' || r == '-' || r == '.' || r == '/' || jsutil.IsSpace(r) {
			return r
		}
		return -1
	}, jsutil.Trim(s))
}

// Content strips C0 controls other than tab, newline and carriage return,
// DEL, C1 controls, the zero-width characters U+200B–U+200D and U+FEFF, and
// the bidi controls U+202A–U+202E and U+2066–U+2069.
func Content(s string) string {
	return strings.Map(func(r rune) rune {
		if dangerous(r) {
			return -1
		}
		return r
	}, s)
}

func dangerous(r rune) bool {
	switch {
	case r <= 0x08, r == 0x0b, r == 0x0c, r >= 0x0e && r <= 0x1f:
		return true
	case r >= 0x7f && r <= 0x9f:
		return true
	case r >= 0x200b && r <= 0x200d, r == 0xfeff:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}
