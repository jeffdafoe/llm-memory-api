// Package frontmatter reads the "key: value" header of a template
// (Node's parseTemplateFrontmatter in routes/admin.js).
package frontmatter

import (
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Header is a template's "key: value" header lines.
type Header map[string]string

// Get is `frontmatter[key] || fallback`.
func (f Header) Get(key, fallback string) string {
	if v := f[key]; v != "" {
		return v
	}
	return fallback
}

// Parse splits a template into its header and body, as Node's
// parseTemplateFrontmatter: the content must match
// /^---\n([\s\S]*?)\n---\n\n?([\s\S]*)$/, else there is no header and the
// whole content is the body. Each header line with a colon after its first
// character is a key and a value, both trimmed; a later key wins.
func Parse(content string) (Header, string) {
	f := Header{}
	if !strings.HasPrefix(content, "---\n") {
		return f, content
	}
	end := strings.Index(content[3:], "\n---\n")
	if end < 0 {
		return f, content
	}
	end += 3
	if end < 4 {
		// The lazy group needs the closing line after the opening one.
		next := strings.Index(content[4:], "\n---\n")
		if next < 0 {
			return f, content
		}
		end = next + 4
	}
	header := content[4:end]
	body := strings.TrimPrefix(content[end+5:], "\n")
	for _, line := range strings.Split(header, "\n") {
		colon := strings.Index(line, ":")
		if colon > 0 {
			key := jsutil.Trim(line[:colon])
			if key == "__proto__" {
				continue
			}
			f[key] = jsutil.Trim(line[colon+1:])
		}
	}
	return f, body
}
