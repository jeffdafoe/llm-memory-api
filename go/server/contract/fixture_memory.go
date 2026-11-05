//go:build contract

package contract

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Search scores decay with NOW() and the access boost reads last_accessed,
// so a note that decays scores differently on the two servers, which query
// a few milliseconds apart. Every fixture note therefore has a kind or a
// cognitive type whose half-life is 0 in seed.sql (reference, instruction,
// semantic, procedural), or no documents row at all, and last_accessed
// NULL. The decay and access-boost SQL is checked against Node in the
// memory package's golden tests instead.
//
// A search stamps last_accessed on the notes it returns, which turns the
// access boost on for the next search, so a scenario searches those notes
// once.

// fixtureCreated is every fixture note's created_at and updated_at. The
// microseconds exercise the millisecond truncation of the JSON dates.
const fixtureCreated = "2026-01-02 03:04:05.123456+00"

// fixtureChunk is one memory_chunks row. embedText is the text the stored
// vector is EmbeddingFor; "" means the chunk's own text. A chunk embedded
// with a query's cleaned text scores about 1 for that query.
type fixtureChunk struct {
	heading   string
	text      string
	embedText string
}

type fixtureNote struct {
	namespace string
	slug      string
	kind      string // "" means no documents row (a raw ingest)
	metadata  string // JSON, "" for NULL
	deleted   bool
	chunks    []fixtureChunk
}

// The queries the cases send, as PreprocessQuery leaves them.
const (
	queryAuth  = "auth middleware work" // from "how does the auth middleware work"
	queryDream = "dream lock"
)

var fixtureNotes = []fixtureNote{
	{namespace: AgentAName, slug: "notes/codebase/auth-middleware", kind: "reference", chunks: []fixtureChunk{
		{heading: "## Auth middleware", text: "## Auth middleware\nThe auth middleware checks the bearer token on every request.", embedText: queryAuth},
		{heading: "## Sessions", text: "## Sessions\nSessions refresh when they are within twelve hours of expiry."},
	}},
	{namespace: AgentAName, slug: "instructions/deploy", kind: "instruction", chunks: []fixtureChunk{
		{text: "Deploy steps: pull main, build, restart the service. The auth middleware is unchanged by a deploy."},
	}},
	// A cognitive type wins over the kind: "note" decays, "procedural" does
	// not. The value is padded and capitalized for the LOWER(TRIM()) match.
	{namespace: AgentAName, slug: "Notes/Mixed-Case", kind: "note", metadata: `{"cognitive_type": " Procedural "}`, chunks: []fixtureChunk{
		{heading: "# Mixed case", text: "# Mixed case\nA procedural note whose slug has capitals, about middleware ordering."},
	}},
	{namespace: AgentAName, slug: "notes/codebase/deleted-doc", kind: "reference", deleted: true, chunks: []fixtureChunk{
		{text: "A deleted note about the auth middleware.", embedText: queryAuth},
	}},
	{namespace: AgentAName, slug: "memory-only/file-1", chunks: []fixtureChunk{
		{text: "Raw ingest one about the dream lock."},
		{text: "Raw ingest one, second chunk."},
	}},
	{namespace: AgentAName, slug: "memory-only/file-2", chunks: []fixtureChunk{
		{text: "Raw ingest two."},
	}},
	{namespace: "shared", slug: "notes/codebase/dream-lock", kind: "reference", metadata: `{"cognitive_type": "Semantic"}`, chunks: []fixtureChunk{
		{heading: "## Dream lock", text: "## Dream lock\nThe dream lock keeps one dream run at a time.", embedText: queryDream},
	}},
	{namespace: "shared", slug: "imports/raw-file", chunks: []fixtureChunk{
		{text: "Raw ingested text about the deploy and the dream lock."},
	}},
	// "_" in a slug prefix must match only "_", not any character.
	{namespace: "shared", slug: "anne_walker/memory/day-1", kind: "reference", chunks: []fixtureChunk{
		{text: "Anne walked to the mill and talked about the dream lock."},
	}},
	{namespace: "shared", slug: "annexwalker/memory/day-2", kind: "reference", chunks: []fixtureChunk{
		{text: "Annex walker notes about the dream lock."},
	}},
	{namespace: "private-ns", slug: "notes/codebase/shared-with-b", kind: "reference", chunks: []fixtureChunk{
		{text: "A private note shared with agent-b about the dream lock.", embedText: queryDream},
	}},
	{namespace: "private-ns", slug: "notes/codebase/not-shared", kind: "reference", chunks: []fixtureChunk{
		{text: "A private note nobody else reads, about the dream lock.", embedText: queryDream},
	}},
}

// applyMemoryFixture writes the notes and chunks, and shares one private-ns
// note with agent-b through note_permissions.
func applyMemoryFixture(ctx context.Context, tx pgx.Tx, adminID int, agents map[string]int) error {
	for _, n := range fixtureNotes {
		if n.kind != "" {
			var meta any
			if n.metadata != "" {
				meta = n.metadata
			}
			var deleted any
			if n.deleted {
				deleted = fixtureCreated
			}
			content := ""
			for i, c := range n.chunks {
				if i > 0 {
					content += "\n\n"
				}
				content += c.text
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO documents (namespace, slug, title, content, created_at, updated_at, deleted_at, kind, metadata)
				 VALUES ($1, $2, $2, $3, $4, $4, $5, $6, $7::jsonb)`,
				n.namespace, n.slug, content, fixtureCreated, deleted, n.kind, meta); err != nil {
				return fmt.Errorf("document %s/%s: %w", n.namespace, n.slug, err)
			}
		}
		for _, c := range n.chunks {
			var heading any
			if c.heading != "" {
				heading = c.heading
			}
			embedText := c.embedText
			if embedText == "" {
				embedText = c.text
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO memory_chunks (namespace, source_file, heading, chunk_text, embedding, ingested_at)
				 VALUES ($1, $2, $3, $4, $5::vector, $6)`,
				n.namespace, n.slug, heading, c.text, VectorLiteral(EmbeddingFor(embedText)), fixtureCreated); err != nil {
				return fmt.Errorf("chunk of %s/%s: %w", n.namespace, n.slug, err)
			}
		}
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO note_permissions (owner_namespace, slug_pattern, grantee_actor_id, can_read, granted_by)
		 VALUES ('private-ns', 'notes/codebase/shared-with-b', $1, true, $2)`,
		agents[AgentBName], adminID)
	return err
}
