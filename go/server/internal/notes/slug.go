package notes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

func badRequest(message string) error {
	return apierr.New(http.StatusBadRequest, message)
}

// ValidateSlug refuses slugs that break the notes tree in the admin UI.
// allowTrailingSlash is for folder prefixes. A slug of another JSON type
// fails as "Slug is required", as in Node's typeof check; callers pass
// notString true for that.
func ValidateSlug(slug string, notString, allowTrailingSlash bool) error {
	if notString || slug == "" {
		return badRequest("Slug is required")
	}
	if jsutil.UTF16Len(slug) > 500 {
		return badRequest("Slug too long (max 500 characters)")
	}
	for _, r := range slug {
		if r <= 0x1f || r == 0x7f || r == '\\' {
			return badRequest("Slug contains invalid characters")
		}
	}
	if strings.HasPrefix(slug, "/") {
		return badRequest("Slug must not start with /")
	}
	if !allowTrailingSlash && strings.HasSuffix(slug, "/") {
		return badRequest("Slug must not end with /")
	}
	if strings.Contains(slug, "//") {
		return badRequest("Slug must not contain //")
	}
	for _, seg := range strings.Split(strings.TrimSuffix(slug, "/"), "/") {
		if seg == "" {
			return badRequest("Slug must not contain empty path segments")
		}
	}
	return nil
}

// SlugToKind is the document kind a slug's folder implies. Search decay and
// weights are keyed on it.
func SlugToKind(slug string) string {
	switch {
	case strings.HasPrefix(slug, "instructions/"):
		return "instruction"
	case strings.HasPrefix(slug, "notes/codebase/"):
		return "reference"
	case strings.HasPrefix(slug, "conversations/"):
		return "conversation"
	case strings.HasPrefix(slug, "context/"):
		return "context"
	case strings.HasPrefix(slug, "dreams/"):
		return "dream"
	case strings.HasPrefix(slug, "tasks/"):
		return "task"
	case strings.HasPrefix(slug, "learnings/"):
		return "learning"
	}
	return "note"
}

// TitleToSlug derives a slug from a title: lowercased, only a-z, 0-9,
// whitespace and "-" kept, each whitespace run a "-", repeated "-" folded,
// one "-" trimmed from each end, at most 200 characters.
func TitleToSlug(title string) string {
	lower := jsutil.ToLower(title)
	var kept strings.Builder
	for _, r := range lower {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || jsutil.IsSpace(r) {
			kept.WriteRune(r)
		}
	}
	var b strings.Builder
	inSpace := false
	for _, r := range kept.String() {
		if jsutil.IsSpace(r) {
			if !inSpace {
				b.WriteByte('-')
			}
			inSpace = true
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	s := b.String()
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.TrimPrefix(s, "-")
	s = strings.TrimSuffix(s, "-")
	// Only ASCII is left, so bytes are UTF-16 code units.
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// ReadMaxLimit caps the lines one paginated read returns.
const ReadMaxLimit = 10000

// Page is a paginated read: Text starts with "[lines START-END of TOTAL]"
// and a blank line.
type Page struct {
	Text       string
	TotalLines int
}

// jsInteger reads a request value as Number.isInteger does: a JSON number
// with an integral, finite value. ok is false for any other value.
func jsInteger(raw json.RawMessage) (float64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !(raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9') {
		return 0, false
	}
	f := jsutil.ToNumber(raw)
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, false
	}
	return f, true
}

func present(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && string(raw) != "null"
}

// PaginateContent applies line pagination (1-indexed) to a note's content.
// With neither offset nor limit it returns nil: the caller sends the content
// as is. An offset alone reads 2000 lines; a limit alone starts at line 1.
// An offset past the end gives the header with an open range and no body.
// Out-of-range values are 400s, never clamped.
func PaginateContent(content string, offset, limit json.RawMessage) (*Page, error) {
	hasOffset, hasLimit := present(offset), present(limit)
	if !hasOffset && !hasLimit {
		return nil, nil
	}
	start, startOK := 1.0, true
	if hasOffset {
		start, startOK = jsInteger(offset)
	}
	lim, limOK := 2000.0, true
	if hasLimit {
		lim, limOK = jsInteger(limit)
	}
	if !startOK || start < 1 {
		return nil, badRequest("offset must be a positive integer (1-indexed)")
	}
	if !limOK || lim < 1 {
		return nil, badRequest("limit must be a positive integer")
	}
	if lim > ReadMaxLimit {
		return nil, badRequest(fmt.Sprintf("limit must not exceed %d", ReadMaxLimit))
	}

	lines := strings.Split(content, "\n")
	total := len(lines)
	if start > float64(total) {
		return &Page{Text: "[lines " + jsutil.NumberString(start) + "- of " + fmt.Sprint(total) + "]\n\n", TotalLines: total}, nil
	}
	startIdx := int(start) - 1
	endIdx := min(startIdx+int(lim), total)
	header := fmt.Sprintf("[lines %d-%d of %d]\n\n", int(start), endIdx, total)
	return &Page{Text: header + strings.Join(lines[startIdx:endIdx], "\n"), TotalLines: total}, nil
}
