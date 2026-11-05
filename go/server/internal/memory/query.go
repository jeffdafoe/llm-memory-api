package memory

import (
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// MaxEmbedQueryChars bounds a search query before it is embedded, in UTF-16
// code units. OpenAI's text-embedding-3-small takes at most 8192 tokens per
// input, and callers pass raw inbound text (a review diff mailed to a virtual
// agent runs past 25 KB). Over the cap the embeddings call fails and the
// caller gets no recall at all. There is no tokenizer here; at about 3
// characters per token for dense diffs this stays near 6000 tokens.
const MaxEmbedQueryChars = 18000

// fillerPhrases are stripped from the front of a query, in this order; the
// first one that matches wins, as in the Node regex alternation. Matching
// ignores ASCII case only.
var fillerPhrases = [][]string{
	{"can you ", "could you ", "please ", "tell me ", "show me ", "explain ", "describe ", "help me ",
		"i want to know ", "i need to know ", "what is ", "what are ", "what's ", "how do i ", "how does ",
		"how do ", "how to ", "where is ", "where are ", "where's ", "who is ", "who are ", "find me ",
		"search for ", "look for ", "give me ", "let me know "},
	{"about ", "regarding ", "related to ", "concerning "},
}

var fillerWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the is are was were be been being
		do does did have has had will would could should can may might shall must
		i me my we our you your it its this that these those
		of in to for with on at by from and or but not so if then
		just also very really actually basically please thanks thank`) {
		fillerWords[w] = true
	}
}

// PreprocessQuery strips filler before embedding: "can you tell me about how
// the auth middleware works" becomes "auth middleware works". The query is
// first cut to MaxEmbedQueryChars, so every return path stays in the
// embedding budget, including the two that fall back to the raw query when
// stripping leaves too little.
func PreprocessQuery(query string) string {
	query = jsutil.TruncateUTF16(query, MaxEmbedQueryChars)
	cleaned := jsutil.Trim(query)
	for {
		prev := cleaned
		for _, phrases := range fillerPhrases {
			for _, p := range phrases {
				if jsutil.HasPrefixFold(cleaned, p) {
					cleaned = cleaned[len(p):]
					break
				}
			}
		}
		cleaned = jsutil.Trim(cleaned)
		if cleaned == prev || cleaned == "" {
			break
		}
	}

	if jsutil.UTF16Len(cleaned) < 3 {
		return jsutil.Trim(query)
	}
	var words []string
	for _, w := range jsutil.SplitSpace(cleaned) {
		if !fillerWords[jsutil.ToLower(w)] {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return jsutil.Trim(query)
	}
	return strings.Join(words, " ")
}

// EscapeLikePattern escapes the three LIKE metacharacters, so a
// caller-supplied prefix matches literally. Backslash is the ESCAPE
// character, so a backslash in the input is doubled.
func EscapeLikePattern(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r == '\\' || r == '%' || r == '_' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
