package notes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsregex"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// GrepMaxContext caps the context lines per side of a match.
const GrepMaxContext = 50

// grepMatchSteps bounds the regex matching work of one grep request across
// all its lines. Node had no bound; a pattern that passes safe-regex but
// backtracks badly (for example (a|aa)+$) held its event loop until done.
// Past the budget the request is a 500.
const grepMatchSteps = 20_000_000

// grepRegexMaxLength caps a regex pattern, in UTF-16 units. Length alone does
// not prevent catastrophic backtracking, but it bounds parsing and pairs
// with the safe-regex check.
const grepRegexMaxLength = 200

// GrepInput is one grep. The raw values are the caller's JSON (nil when
// absent). Namespace "" or "*" searches across namespaces, filtered by
// Readable unless FilterReadable is false (a wildcard reader).
type GrepInput struct {
	Pattern        string
	Namespace      string
	Limit          json.RawMessage
	Readable       []string
	FilterReadable bool
	ContextBefore  json.RawMessage
	ContextAfter   json.RawMessage
	Context        json.RawMessage
	// Regex is true only for a JSON true.
	Regex bool
}

// GrepLine is one line of a result: a match or a context line. NewBlock
// marks the first line of a block that does not follow the previous line,
// where a formatter prints a "--" separator.
type GrepLine struct {
	LineNumber int    `json:"lineNumber"`
	Line       string `json:"line"`
	IsMatch    bool   `json:"isMatch"`
	NewBlock   bool   `json:"newBlock,omitempty"`
}

// GrepResult is one note that matched (in its content or its title).
type GrepResult struct {
	Namespace  string     `json:"namespace"`
	Slug       string     `json:"slug"`
	Title      *string    `json:"title"`
	MatchCount int        `json:"matchCount"`
	Matches    []GrepLine `json:"matches"`
}

// contextLines resolves one side of the context window: the specific value,
// else the symmetric context, else 2. It must be an integer from 0 to 50.
func contextLines(specific, both json.RawMessage, name string) (int, error) {
	raw := specific
	if !present(raw) {
		raw = both
	}
	if !present(raw) {
		return 2, nil
	}
	n, ok := jsInteger(raw)
	if !ok || n < 0 || n > GrepMaxContext {
		return 0, badRequest(fmt.Sprintf("%s must be an integer between 0 and %d", name, GrepMaxContext))
	}
	return int(n), nil
}

// Grep searches live notes for a pattern: Postgres picks the candidate notes
// (ILIKE '%pattern%', or ~* for a regex; the substring pattern is not
// escaped, as in Node), then each note's lines are matched with JavaScript's
// rules (a lowercase substring test, or the regex with the i flag) and
// returned with their context. A regex is refused when it is over 200
// characters, invalid in JavaScript, or flagged by the safe-regex check.
func (s *Service) Grep(ctx context.Context, in GrepInput) ([]GrepResult, error) {
	if in.Pattern == "" {
		return nil, badRequest("Required field: pattern")
	}
	before, err := contextLines(in.ContextBefore, in.Context, "context_before")
	if err != nil {
		return nil, err
	}
	after, err := contextLines(in.ContextAfter, in.Context, "context_after")
	if err != nil {
		return nil, err
	}
	limit := orDefault(in.Limit, 20)

	var match func(string) (bool, error)
	sqlPattern, op := "%"+in.Pattern+"%", "ILIKE"
	if in.Regex {
		if jsutil.UTF16Len(in.Pattern) > grepRegexMaxLength {
			return nil, badRequest(fmt.Sprintf("regex pattern exceeds %d characters", grepRegexMaxLength))
		}
		parsed, err := jsregex.Parse(in.Pattern)
		if err != nil {
			return nil, badRequest("Invalid regex pattern: " + err.Error())
		}
		if !parsed.Safe() {
			return nil, badRequest("regex pattern rejected by safety check (possible catastrophic backtracking). Simplify the pattern or use substring mode.")
		}
		re, err := jsregex.CompileIgnoreCase(in.Pattern)
		if err != nil {
			return nil, err
		}
		budget := jsregex.NewBudget(ctx, grepMatchSteps)
		match = func(line string) (bool, error) { return re.MatchString(line, budget) }
		sqlPattern, op = in.Pattern, "~*"
	} else {
		needle := jsutil.ToLower(in.Pattern)
		match = func(line string) (bool, error) {
			return strings.Contains(jsutil.ToLower(line), needle), nil
		}
	}

	var sql string
	var params []any
	switch {
	case in.Namespace != "" && in.Namespace != "*":
		sql = `
            SELECT id, namespace, slug, title, content, updated_at
            FROM documents
            WHERE namespace = $1 AND deleted_at IS NULL AND (content ` + op + ` $2 OR title ` + op + ` $2)
            ORDER BY updated_at DESC
            LIMIT $3
        `
		params = []any{in.Namespace, sqlPattern, limit}
	case in.FilterReadable:
		sql = `
            SELECT id, namespace, slug, title, content, updated_at
            FROM documents
            WHERE namespace = ANY($1) AND deleted_at IS NULL AND (content ` + op + ` $2 OR title ` + op + ` $2)
            ORDER BY updated_at DESC
            LIMIT $3
        `
		params = []any{nonNil(in.Readable), sqlPattern, limit}
	default:
		sql = `
            SELECT id, namespace, slug, title, content, updated_at
            FROM documents
            WHERE deleted_at IS NULL AND (content ` + op + ` $1 OR title ` + op + ` $1)
            ORDER BY updated_at DESC
            LIMIT $2
        `
		params = []any{sqlPattern, limit}
	}

	rows, err := s.DB.Query(ctx, sql, params...)
	if err != nil {
		return nil, err
	}
	type doc struct {
		namespace, slug, content string
		title                    *string
	}
	var docs []doc
	for rows.Next() {
		var d doc
		var id int
		var updated any
		if err := rows.Scan(&id, &d.namespace, &d.slug, &d.title, &d.content, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	results := []GrepResult{}
	for _, d := range docs {
		lines := strings.Split(d.content, "\n")
		matched := make([]bool, len(lines))
		count := 0
		for i, line := range lines {
			ok, err := match(line)
			if err != nil {
				return nil, err
			}
			if ok {
				matched[i] = true
				count++
			}
		}
		// Lines within the window of any match, in order.
		shown := make([]bool, len(lines))
		for i := range lines {
			if !matched[i] {
				continue
			}
			for j := max(0, i-before); j <= i+after && j < len(lines); j++ {
				shown[j] = true
			}
		}
		out := GrepResult{Namespace: d.namespace, Slug: d.slug, Title: d.title, MatchCount: count, Matches: []GrepLine{}}
		prev := -1
		for i := range lines {
			if !shown[i] {
				continue
			}
			out.Matches = append(out.Matches, GrepLine{LineNumber: i + 1, Line: lines[i], IsMatch: matched[i], NewBlock: prev != -1 && i > prev+1})
			prev = i
		}
		results = append(results, out)
	}
	return results, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
