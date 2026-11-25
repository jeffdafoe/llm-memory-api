package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

func conversationConfig() *config.Store {
	cfg := config.NewStore()
	cfg.Set("conversation_chunk_window", "5")
	cfg.Set("conversation_chunk_overlap", "2")
	cfg.Set("conversation_chunk_max_chars", "2000")
	return cfg
}

func storedChunks(t *testing.T, s *Service, namespace, sourceFile string) []string {
	t.Helper()
	rows, err := s.DB.Query(context.Background(),
		"SELECT chunk_text FROM memory_chunks WHERE namespace = $1 AND source_file = $2 ORDER BY id", namespace, sourceFile)
	if err != nil {
		t.Fatal(err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return texts
}

// The two shapes that failed with the embedding model's 8192-token limit:
// a note with one huge section (wendy's notes/active-work) and a salem
// sim-day under an agent folder. Both must ingest into chunks under the cap.
func TestIngestKeepsEveryChunkUnderTheCap(t *testing.T) {
	pool := testutil.ScratchDB(t, chunksSchema)
	e := &countingEmbedder{}
	s := &Service{DB: pool, Config: conversationConfig(), Embedder: e}

	var note strings.Builder
	note.WriteString("# Active Work\n\nintro\n\n## In Progress\n\n")
	for range 300 {
		note.WriteString("- **Client** — " + strings.Repeat("status update; ", 120) + "\n")
	}
	var day strings.Builder
	day.WriteString("Day: 2026-11-24 — Constance Scott\n\n")
	for i := range 200 {
		day.WriteString("[Tuesday 12:0" + string(rune('0'+i%10)) + " Constance Scott] \"" + strings.Repeat("talk ", 30) + "\"\n")
	}

	cases := []struct{ sourceFile, content string }{
		{"notes/active-work", note.String()},
		{"constance-scott/conversations/2026-11-24-sim-day", day.String()},
	}
	for _, c := range cases {
		if _, err := s.Ingest(context.Background(), "ns", c.sourceFile, c.content); err != nil {
			t.Fatalf("%s: %v", c.sourceFile, err)
		}
		texts := storedChunks(t, s, "ns", c.sourceFile)
		if len(texts) < 2 {
			t.Errorf("%s: %d chunks, want it split", c.sourceFile, len(texts))
		}
		for i, text := range texts {
			if n := jsutil.UTF16Len(text); n > MaxChunkChars {
				t.Errorf("%s: chunk %d is %d code units, over %d", c.sourceFile, i, n, MaxChunkChars)
			}
		}
	}
	// The sim-day is chunked by message window, so its chunks carry the
	// day's header line, which heading chunking would not repeat.
	for i, text := range storedChunks(t, s, "ns", "constance-scott/conversations/2026-11-24-sim-day") {
		if !strings.HasPrefix(text, "Day: 2026-11-24") {
			t.Fatalf("sim-day chunk %d is not a message window: %.60q", i, text)
		}
	}
	// The 540K-character note needed more than one embeddings request.
	if len(e.calls) < 3 {
		t.Errorf("%d embeddings requests for both documents, want the big note batched", len(e.calls))
	}
}
