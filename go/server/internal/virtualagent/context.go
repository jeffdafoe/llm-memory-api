package virtualagent

import (
	"context"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
)

// ragMinSimilarity is the score a search hit needs to go into a prompt.
const ragMinSimilarity = 0.3

// loadRAGContext is loadRAGContext: the agent's own notes that match query,
// as "[<source file>] <chunk>" blocks, or "" when none score above
// ragMinSimilarity. A failed search is logged and gives "".
func (s *Service) loadRAGContext(ctx context.Context, agentName, query string) string {
	res, err := s.Memory.Search(ctx, memory.SearchInput{Query: query, Namespace: agentName, Limit: 5})
	if err != nil {
		s.logVA("rag-error", logger.Fields{{Key: "agent", Value: agentName}, {Key: "error", Value: err.Error()}})
		return ""
	}
	var parts []string
	for _, r := range res.Results {
		if r.Similarity != nil && float64(*r.Similarity) > ragMinSimilarity {
			parts = append(parts, "["+r.SourceFile+"] "+r.ChunkText)
		}
	}
	return strings.Join(parts, "\n\n")
}

// loadSoul is loadSoul: the agent's context/soul note, or "" when it has
// none or it cannot be read.
func (s *Service) loadSoul(ctx context.Context, agentName string) string {
	n, err := s.Notes.Read(ctx, agentName, "context/soul")
	if err != nil {
		return ""
	}
	return n.Content
}

// loadPeopleContext is loadPeopleContext: the agent's context/people/<slug>
// note for each counterpart, under a "## Your impressions of <name>"
// heading. A name with no usable slug is logged and skipped; a missing or
// empty note is skipped.
func (s *Service) loadPeopleContext(ctx context.Context, agentName string, counterparts []string) string {
	var sections []string
	for _, person := range counterparts {
		slug := personContextSlug(person)
		if slug == "" {
			s.logVA("people-context-invalid-name", logger.Fields{{Key: "agent", Value: agentName}, {Key: "person", Value: person}})
			continue
		}
		n, err := s.Notes.Read(ctx, agentName, "context/people/"+slug)
		if err == nil && n.Content != "" {
			sections = append(sections, "## Your impressions of "+person+"\n\n"+n.Content)
		}
	}
	return strings.Join(sections, "\n\n")
}

// personContextSlug is people-slug.js's personContextSlug: the name trimmed,
// NFKD-decomposed, lower-cased, whitespace runs turned into "-", then only
// [a-z0-9-] kept, "-" runs collapsed and edge hyphens dropped. "" when
// nothing is left, so a name like "../x" cannot reach outside
// context/people/. Node also strips the combining diacritics after the
// decomposition; the [a-z0-9-] filter drops them all the same.
func personContextSlug(name string) string {
	decomposed := norm.NFKD.String(jsutil.Trim(name))
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(decomposed) {
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
	for _, r := range b.String() {
		if r == '-' && strings.HasSuffix(out.String(), "-") {
			continue
		}
		out.WriteRune(r)
	}
	return strings.TrimSuffix(strings.TrimPrefix(out.String(), "-"), "-")
}
