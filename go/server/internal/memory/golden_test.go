package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// goldens is testdata/goldens.json, written by testdata/goldens.js from the
// Node server's own functions. Regenerate it with that script when the Node
// side changes.
type goldens struct {
	Embedding          []float64 `json:"embedding"`
	MaxEmbedQueryChars int       `json:"max_embed_query_chars"`
	Preprocess         []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	} `json:"preprocess"`
	EscapeLike []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	} `json:"escape_like"`
	ChunkByHeading []struct {
		In  string  `json:"in"`
		Out []Chunk `json:"out"`
	} `json:"chunk_by_heading"`
	ChunkConversation []struct {
		Content  string  `json:"content"`
		Window   int     `json:"window"`
		Overlap  int     `json:"overlap"`
		MaxChars int     `json:"maxChars"`
		Out      []Chunk `json:"out"`
	} `json:"chunk_conversation"`
	Search []struct {
		Name string `json:"name"`
		Args struct {
			Query        string          `json:"query"`
			Namespace    *string         `json:"namespace"`
			Limit        json.RawMessage `json:"limit"`
			LimitMissing bool            `json:"limit_missing"`
			Readable     []string        `json:"readable"`
			ActorID      int             `json:"actor_id"`
			SlugPrefix   *string         `json:"slug_prefix"`
		} `json:"args"`
		Config map[string]string `json:"config"`
		SQL    string            `json:"sql"`
		Params []json.RawMessage `json:"params"`
	} `json:"search"`
}

func loadGoldens(t *testing.T) goldens {
	t.Helper()
	data, err := os.ReadFile("testdata/goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldens
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestPreprocessQueryMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	if g.MaxEmbedQueryChars != MaxEmbedQueryChars {
		t.Fatalf("MaxEmbedQueryChars = %d, Node has %d", MaxEmbedQueryChars, g.MaxEmbedQueryChars)
	}
	for _, c := range g.Preprocess {
		if got := PreprocessQuery(c.In); got != c.Out {
			t.Errorf("PreprocessQuery(%.60q) = %.60q, Node gives %.60q", c.In, got, c.Out)
		}
	}
}

func TestEscapeLikePatternMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).EscapeLike {
		if got := EscapeLikePattern(c.In); got != c.Out {
			t.Errorf("EscapeLikePattern(%q) = %q, Node gives %q", c.In, got, c.Out)
		}
	}
}

func chunksText(cs []Chunk) string {
	b, _ := json.Marshal(cs)
	return string(b)
}

func TestChunkByHeadingMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).ChunkByHeading {
		if got, want := chunksText(ChunkByHeading(c.In)), chunksText(c.Out); got != want {
			t.Errorf("ChunkByHeading(%q)\n got %s\nNode %s", c.In, got, want)
		}
	}
}

func TestChunkConversationMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).ChunkConversation {
		got := chunksText(ChunkConversation(c.Content, c.Window, c.Overlap, c.MaxChars))
		if want := chunksText(c.Out); got != want {
			t.Errorf("ChunkConversation(%q, %d, %d, %d)\n got %s\nNode %s", c.Content, c.Window, c.Overlap, c.MaxChars, got, want)
		}
	}
}

// collapse folds every run of whitespace to one space. The SQL has no
// whitespace inside a string literal, so this compares the statements
// without depending on how the Go source is indented.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// paramText is a parameter as node-postgres would send it, for comparison
// with the Node parameter in JSON form.
func paramText(v any) string {
	switch x := v.(type) {
	case jsutil.Number:
		return string(x)
	case int:
		return fmt.Sprint(x)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestSearchSQLMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	vector := VectorLiteral(g.Embedding)
	for _, c := range g.Search {
		t.Run(c.Name, func(t *testing.T) {
			store := config.NewStore()
			for k, v := range c.Config {
				store.Set(k, v)
			}
			cfg, err := loadSearchConfig(store)
			if err != nil {
				t.Fatal(err)
			}
			in := SearchInput{Query: c.Args.Query, ActorID: c.Args.ActorID, Limit: math.NaN()}
			if c.Args.Namespace != nil {
				in.Namespace = *c.Args.Namespace
			}
			if !c.Args.LimitMissing {
				in.Limit = jsutil.ToNumber(c.Args.Limit)
			}
			if c.Args.Readable != nil {
				in.Readable = c.Args.Readable
				in.FilterReadable = true
			}
			if c.Args.SlugPrefix != nil {
				in.SlugPrefix = *c.Args.SlugPrefix
			}

			sql, params := buildSearch(in, cfg, PreprocessQuery(in.Query), vector)
			if got, want := collapse(sql), collapse(c.SQL); got != want {
				t.Errorf("SQL differs\n  Go: %s\nNode: %s", got, want)
			}
			if len(params) != len(c.Params) {
				t.Fatalf("%d params, Node has %d", len(params), len(c.Params))
			}
			for i, p := range params {
				var compact bytes.Buffer
				json.Compact(&compact, c.Params[i])
				want := compact.String()
				if got := paramText(p); got != want {
					t.Errorf("param $%d = %s, Node has %s", i+1, got, want)
				}
			}
		})
	}
}
