package memory

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

func TestIsMessageLine(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"[14:05 home] hello", true},
		{"[Tuesday 01:00 Hannah Boggs] \"That's the shop shut.\"", true},
		{"[Tuesday 11:00 Hannah Boggs] (had water)", true},
		{"[Tuesday 01:00]", false},
		{"[Tuesday01:00 Hannah] hi", false},
		{"[Tuesday  01:00 Hannah] hi", false},
		{"[ 01:00 Hannah] hi", false},
		{"[Tuesday ??:?? Hannah] hi", false},
		{"[Mon 1:00 x] hi", false},
		{"Day: 2026-11-24 — Hannah Boggs", false},
		{"[", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isMessageLine(c.line); got != c.want {
			t.Errorf("isMessageLine(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

// A salem sim-day ("[Weekday HH:MM speaker]" lines) chunks by message
// window. Before, no line matched, the whole day became one header-only
// chunk, and a 30 KB day was over the embedding model's input limit.
func TestChunkConversationSimDay(t *testing.T) {
	var b strings.Builder
	b.WriteString("Day: 2026-11-24 — Hannah Boggs\n\n")
	for i := range 12 {
		b.WriteString("[Tuesday 12:0")
		b.WriteByte(byte('0' + i%10))
		b.WriteString(" Hannah Boggs] \"Line ")
		b.WriteString(strings.Repeat("x", i))
		b.WriteString("\"\n")
	}
	chunks := ChunkConversation(b.String(), 5, 2, 2000)
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks, want a window per few messages", len(chunks))
	}
	for i, c := range chunks {
		if !strings.HasPrefix(c.ChunkText, "Day: 2026-11-24 — Hannah Boggs") {
			t.Errorf("chunk %d does not start with the header: %q", i, c.ChunkText)
		}
		if strings.Count(c.ChunkText, "[Tuesday ") > 5 {
			t.Errorf("chunk %d holds more than a window: %q", i, c.ChunkText)
		}
	}
	if chunks[0].Heading == nil || *chunks[0].Heading != "Conversation at Tuesday 12:00" {
		t.Errorf("heading = %v, want \"Conversation at Tuesday 12:00\"", chunks[0].Heading)
	}
}

func TestIsConversationSource(t *testing.T) {
	cases := map[string]bool{
		"conversations/2026-11-25-abc":                     true,
		"constance-scott/conversations/2026-11-24-sim-day": true,
		"notes/active-work":                                false,
		"notes/conversations-with-jeff":                    false,
		"context/soul":                                     false,
	}
	for slug, want := range cases {
		if got := isConversationSource(slug); got != want {
			t.Errorf("isConversationSource(%q) = %v, want %v", slug, got, want)
		}
	}
}

// words is the text's words in order, the content a split must keep.
func words(text string) []string {
	return strings.Fields(text)
}

func checkPieces(t *testing.T, name, text string, pieces []string, max int) {
	t.Helper()
	var joined []string
	for i, p := range pieces {
		if n := jsutil.UTF16Len(p); n > max {
			t.Errorf("%s: piece %d is %d code units, over %d", name, i, n, max)
		}
		if p == "" || p != jsutil.Trim(p) {
			t.Errorf("%s: piece %d is empty or untrimmed: %q", name, i, p)
		}
		if !utf8.ValidString(p) {
			t.Errorf("%s: piece %d is not valid UTF-8", name, i)
		}
		joined = append(joined, words(p)...)
	}
	// Whitespace is dropped on both sides: a cut inside a word splits it
	// across two pieces, and trimming drops the separators.
	if strings.Join(joined, "") != strings.Join(words(text), "") {
		t.Errorf("%s: the pieces do not hold the text's characters in order", name)
	}
}

func TestSplitTextKeepsEveryWordUnderTheCap(t *testing.T) {
	const max = 100
	paragraph := strings.Repeat("alpha beta gamma ", 4) // 68 chars
	cases := map[string]string{
		"paragraphs":         strings.Repeat(paragraph+"\n\n", 10),
		"lines":              strings.Repeat(paragraph+"\n", 10),
		"one long line":      strings.Repeat("delta epsilon ", 40),
		"no separator":       strings.Repeat("z", 350),
		"emoji":              strings.Repeat("⭐⚠️🙂", 60),
		"long word in lines": "short line\n" + strings.Repeat("q", 250) + "\nanother short line",
	}
	for name, text := range cases {
		pieces := splitText(text, max)
		if len(pieces) < 2 {
			t.Errorf("%s: %d pieces, want a split", name, len(pieces))
		}
		checkPieces(t, name, text, pieces, max)
	}
}

// Paragraphs are packed whole while they fit, so a split does not cut
// between lines of one paragraph when it has a choice.
func TestSplitTextPacksWholeParagraphs(t *testing.T) {
	a := strings.Repeat("a", 40)
	b := strings.Repeat("b", 40)
	c := strings.Repeat("c", 40)
	got := splitText(a+"\n\n"+b+"\n\n"+c, 90)
	want := []string{a + "\n\n" + b, c}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSplitOversizedKeepsHeadingsAndSmallChunks(t *testing.T) {
	h1, h2 := "# Small", "## In Progress"
	small := Chunk{Heading: &h1, ChunkText: "# Small\n\nshort"}
	big := Chunk{Heading: &h2, ChunkText: "## In Progress\n\n" + strings.Repeat("- **Client** — status line\n", 400)}
	out := SplitOversized([]Chunk{small, big}, MaxChunkChars)
	if out[0] != small {
		t.Errorf("a chunk under the cap changed: %+v", out[0])
	}
	if len(out) < 3 {
		t.Fatalf("got %d chunks, want the big one split", len(out))
	}
	var pieces []string
	for _, c := range out[1:] {
		if c.Heading != &h2 {
			t.Errorf("a piece lost its heading: %v", c.Heading)
		}
		pieces = append(pieces, c.ChunkText)
	}
	checkPieces(t, "big", big.ChunkText, pieces, MaxChunkChars)
}

// countingEmbedder records the size of every request and answers with one
// vector per text, the vector being the text's position in all calls.
type countingEmbedder struct {
	calls [][]string
	short bool
}

func (e *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	e.calls = append(e.calls, texts)
	out := make([][]float64, len(texts))
	for i, text := range texts {
		out[i] = []float64{float64(len(text))}
	}
	if e.short {
		return out[:len(out)-1], nil
	}
	return out, nil
}

func TestEmbedBatchedSplitsLargeRequestsInOrder(t *testing.T) {
	var texts []string
	for i := range 100 {
		texts = append(texts, strings.Repeat("w", 5000+i))
	}
	e := &countingEmbedder{}
	s := &Service{Embedder: e}
	vectors, err := s.embedBatched(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.calls) < 3 {
		t.Errorf("%d requests for 500K characters, want them split at %d", len(e.calls), embedBatchChars)
	}
	for i, call := range e.calls {
		size := 0
		for _, text := range call {
			size += len(text)
		}
		if size > embedBatchChars {
			t.Errorf("request %d holds %d characters, over %d", i, size, embedBatchChars)
		}
	}
	if len(vectors) != len(texts) {
		t.Fatalf("got %d vectors for %d texts", len(vectors), len(texts))
	}
	for i, v := range vectors {
		if int(v[0]) != len(texts[i]) {
			t.Fatalf("vector %d belongs to a text of length %v, want %d", i, v[0], len(texts[i]))
		}
	}
}

func TestEmbedBatchedSmallNoteIsOneRequest(t *testing.T) {
	e := &countingEmbedder{}
	s := &Service{Embedder: e}
	if _, err := s.embedBatched(context.Background(), []string{"a", "b", "c"}); err != nil {
		t.Fatal(err)
	}
	if len(e.calls) != 1 {
		t.Errorf("%d requests, want 1", len(e.calls))
	}
}

func TestEmbedBatchedShortAnswerFails(t *testing.T) {
	s := &Service{Embedder: &countingEmbedder{short: true}}
	_, err := s.embedBatched(context.Background(), []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "expected array") {
		t.Errorf("err = %v, want the short-answer error", err)
	}
}
