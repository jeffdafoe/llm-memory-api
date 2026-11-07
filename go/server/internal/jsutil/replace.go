package jsutil

import "strings"

// ReplaceAll is s.replace(/<pattern, as literal text>/g, replacement). The
// replacement's $ patterns expand as GetSubstitution expands them for a
// regular expression with no capture groups: $$ is $, $& the match, $` the
// text before the match and $' the text after it. Any other $ is kept.
func ReplaceAll(s, pattern, replacement string) string {
	if pattern == "" || !strings.Contains(replacement, "$") {
		return strings.ReplaceAll(s, pattern, replacement)
	}
	var b strings.Builder
	rest := 0
	for {
		i := strings.Index(s[rest:], pattern)
		if i < 0 {
			break
		}
		start := rest + i
		end := start + len(pattern)
		b.WriteString(s[rest:start])
		b.WriteString(substitution(replacement, s, start, end))
		rest = end
	}
	b.WriteString(s[rest:])
	return b.String()
}

func substitution(replacement, s string, start, end int) string {
	var b strings.Builder
	for i := 0; i < len(replacement); i++ {
		c := replacement[i]
		if c != '$' || i+1 == len(replacement) {
			b.WriteByte(c)
			continue
		}
		switch replacement[i+1] {
		case '$':
			b.WriteByte('$')
		case '&':
			b.WriteString(s[start:end])
		case '`':
			b.WriteString(s[:start])
		case '\'':
			b.WriteString(s[end:])
		default:
			b.WriteByte(c)
			continue
		}
		i++
	}
	return b.String()
}
