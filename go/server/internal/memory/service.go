// Package memory is the search pipeline: chunking, embedding, ingest into
// memory_chunks, and search with its scoring (Node's services/memory.js,
// chunker.js and embeddings.js). The REST routes, the MCP tools and the notes
// service all call it.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/syserrors"
)

// Service runs the pipeline against one database.
type Service struct {
	DB       db.DB
	Config   *config.Store
	Embedder Embedder
	// Errors records failures of background work; nil drops them.
	Errors *syserrors.Recorder
}

// ingestLockClass is the first key of the two-key advisory lock that
// serializes the ingests of one source file (the second key is a hash of the
// file's identity).
const ingestLockClass = 919001

// IngestResult is the ingest response. A context/ document is not indexed
// and answers only {"chunks_created":0}.
type IngestResult struct {
	ChunksCreated int    `json:"chunks_created"`
	SourceFile    string `json:"source_file,omitempty"`
	Namespace     string `json:"namespace,omitempty"`
}

// Ingest replaces a document's chunks: it chunks the content, embeds every
// chunk, then deletes the old rows and inserts the new ones in one
// transaction. Conversation logs are chunked by message window, everything
// else by heading.
func (s *Service) Ingest(ctx context.Context, namespace, sourceFile, content string) (IngestResult, error) {
	// Context documents (context/soul) go into sessions whole; they are not
	// searched.
	if strings.HasPrefix(sourceFile, "context/") {
		return IngestResult{}, nil
	}

	var chunks []Chunk
	if strings.HasPrefix(sourceFile, "conversations/") {
		window, err := s.Config.Get("conversation_chunk_window")
		if err != nil {
			return IngestResult{}, err
		}
		overlap, err := s.Config.Get("conversation_chunk_overlap")
		if err != nil {
			return IngestResult{}, err
		}
		maxChars, err := s.Config.Get("conversation_chunk_max_chars")
		if err != nil {
			return IngestResult{}, err
		}
		chunks = ChunkConversation(content, jsutil.IntOr(window, 5), jsutil.IntOr(overlap, 2), jsutil.IntOr(maxChars, 0))
	} else {
		chunks = ChunkByHeading(content)
	}
	if len(chunks) == 0 {
		return IngestResult{}, apierr.New(http.StatusBadRequest, "No chunks extracted from content")
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.ChunkText
	}
	vectors, err := s.Embedder.Embed(ctx, texts)
	if err != nil {
		return IngestResult{}, err
	}
	if len(vectors) < len(chunks) {
		return IngestResult{}, errors.New("expected array or sparse vector")
	}

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return IngestResult{}, err
	}
	defer tx.Rollback(ctx)
	// Two ingests of one file at once would both run their DELETE before
	// either inserts, and both versions' chunks would stay. The lock makes
	// the second wait for the first to commit; its DELETE is a later
	// statement, so under READ COMMITTED it sees and removes the first one's
	// rows. The key matches the DELETE: the namespace as is, the source file
	// lowercased.
	if _, err := tx.Exec(ctx,
		"SELECT pg_advisory_xact_lock($1, hashtext($2::text || '/' || LOWER($3::text)))",
		ingestLockClass, namespace, sourceFile); err != nil {
		return IngestResult{}, err
	}
	if _, err := tx.Exec(ctx,
		"DELETE FROM memory_chunks WHERE namespace = $1 AND LOWER(source_file) = LOWER($2)",
		namespace, sourceFile); err != nil {
		return IngestResult{}, err
	}
	for i, c := range chunks {
		if _, err := tx.Exec(ctx,
			"INSERT INTO memory_chunks (namespace, source_file, heading, chunk_text, embedding) VALUES ($1, $2, $3, $4, $5)",
			namespace, sourceFile, c.Heading, c.ChunkText, VectorLiteral(vectors[i])); err != nil {
			return IngestResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return IngestResult{}, err
	}
	return IngestResult{ChunksCreated: len(chunks), SourceFile: sourceFile, Namespace: namespace}, nil
}

// SearchResult is one note in a search answer: its best chunk, the note's
// number of matching chunks, and its score.
type SearchResult struct {
	SourceFile string   `json:"source_file"`
	Heading    *string  `json:"heading"`
	ChunkText  string   `json:"chunk_text"`
	Namespace  string   `json:"namespace"`
	CreatedAt  *string  `json:"created_at"`
	Similarity *Float   `json:"similarity"`
	ChunkCount BigCount `json:"chunk_count"`
}

// SearchResults is the search response.
type SearchResults struct {
	Results []SearchResult `json:"results"`
}

// Float is a float8 column. node-postgres read NaN and ±Infinity as
// numbers, which JSON.stringify writes as null.
type Float float64

// MarshalJSON writes the number as JSON.stringify does.
func (f Float) MarshalJSON() ([]byte, error) {
	s := jsutil.NumberString(float64(f))
	if s == "NaN" || s == "Infinity" || s == "-Infinity" {
		return []byte("null"), nil
	}
	return []byte(s), nil
}

// BigCount is a bigint column. node-postgres returns bigint as a string, and
// clients read it that way.
type BigCount int64

// MarshalJSON writes the count as a JSON string.
func (c BigCount) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`"%d"`, int64(c))), nil
}

// Search runs the scored search, then stamps last_accessed on the notes it
// returned, in the background.
func (s *Service) Search(ctx context.Context, in SearchInput) (SearchResults, error) {
	if in.LimitErr != nil {
		return SearchResults{}, in.LimitErr
	}
	cleaned := PreprocessQuery(in.Query)
	vectors, err := s.Embedder.Embed(ctx, []string{cleaned})
	if err != nil {
		return SearchResults{}, err
	}
	if len(vectors) == 0 {
		return SearchResults{}, errors.New("expected array or sparse vector")
	}
	cfg, err := loadSearchConfig(s.Config)
	if err != nil {
		return SearchResults{}, err
	}
	sql, params := buildSearch(in, cfg, cleaned, VectorLiteral(vectors[0]))

	rows, err := s.DB.Query(ctx, sql, params...)
	if err != nil {
		return SearchResults{}, err
	}
	defer rows.Close()
	out := SearchResults{Results: []SearchResult{}}
	for rows.Next() {
		var r SearchResult
		var created *time.Time
		var sim *float64
		var count int64
		if err := rows.Scan(&r.SourceFile, &r.Heading, &r.ChunkText, &r.Namespace, &created, &sim, &count); err != nil {
			return SearchResults{}, err
		}
		if created != nil {
			iso := jsutil.DateISO(*created)
			r.CreatedAt = &iso
		}
		if sim != nil {
			f := Float(*sim)
			r.Similarity = &f
		}
		r.ChunkCount = BigCount(count)
		out.Results = append(out.Results, r)
	}
	if err := rows.Err(); err != nil {
		return SearchResults{}, err
	}

	if len(out.Results) > 0 {
		s.stampLastAccessed(out.Results)
	}
	return out, nil
}

// stampLastAccessed resets last_accessed on the documents a search returned.
// A recalled memory is reinforced: the stamp feeds the access boost and
// resets the decay age. It never delays or fails the search; a failure is
// recorded in system_errors so a broken stamp is visible.
func (s *Service) stampLastAccessed(results []SearchResult) {
	namespaces := make([]string, len(results))
	files := make([]string, len(results))
	for i, r := range results {
		namespaces[i] = r.Namespace
		files[i] = r.SourceFile
	}
	db.Background(func(ctx context.Context) {
		_, err := s.DB.Exec(ctx,
			`UPDATE documents d
         SET last_accessed = NOW()
         FROM (SELECT unnest($1::text[]) AS ns, unnest($2::text[]) AS sf) t
         WHERE d.namespace = t.ns AND LOWER(d.slug) = LOWER(t.sf) AND d.deleted_at IS NULL`,
			namespaces, files)
		if err != nil && s.Errors != nil {
			s.Errors.Record(ctx, "", "memory", "LAST_ACCESSED_STAMP_FAILED", map[string]string{"error": err.Error()})
		}
	})
}

// DeleteResult is the delete response.
type DeleteResult struct {
	ChunksDeleted int64 `json:"chunks_deleted"`
}

// Delete removes one source file's chunks. The match is exact, unlike
// ingest's case-insensitive replace.
func (s *Service) Delete(ctx context.Context, namespace, sourceFile string) (DeleteResult, error) {
	tag, err := s.DB.Exec(ctx, "DELETE FROM memory_chunks WHERE namespace = $1 AND source_file = $2", namespace, sourceFile)
	if err != nil {
		return DeleteResult{}, err
	}
	return DeleteResult{ChunksDeleted: tag.RowsAffected()}, nil
}

// CleanupResult is the cleanup response.
type CleanupResult struct {
	OrphansDeleted int64    `json:"orphans_deleted"`
	OrphanFiles    []string `json:"orphan_files"`
}

// Cleanup deletes the chunks of every source file in the namespace that is
// not in valid. Only string members of valid can match, as in a JavaScript
// Set of the decoded values.
func (s *Service) Cleanup(ctx context.Context, namespace string, valid []json.RawMessage) (CleanupResult, error) {
	keep := map[string]bool{}
	for _, raw := range valid {
		var v string
		if json.Unmarshal(raw, &v) == nil && len(raw) > 0 && raw[0] == '"' {
			keep[v] = true
		}
	}

	rows, err := s.DB.Query(ctx, "SELECT DISTINCT source_file FROM memory_chunks WHERE namespace = $1", namespace)
	if err != nil {
		return CleanupResult{}, err
	}
	orphans := []string{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			rows.Close()
			return CleanupResult{}, err
		}
		if !keep[f] {
			orphans = append(orphans, f)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return CleanupResult{}, err
	}
	if len(orphans) == 0 {
		return CleanupResult{OrphanFiles: orphans}, nil
	}
	tag, err := s.DB.Exec(ctx, "DELETE FROM memory_chunks WHERE namespace = $1 AND source_file = ANY($2)", namespace, orphans)
	if err != nil {
		return CleanupResult{}, err
	}
	return CleanupResult{OrphansDeleted: tag.RowsAffected(), OrphanFiles: orphans}, nil
}

// StatusFile is one source file's ingest state.
type StatusFile struct {
	Namespace  string   `json:"namespace"`
	SourceFile string   `json:"source_file"`
	IngestedAt string   `json:"ingested_at"`
	ChunkCount BigCount `json:"chunk_count"`
}

// StatusResult is the ingest-status response.
type StatusResult struct {
	Files []StatusFile `json:"files"`
}

// Status lists each source file with its newest ingest time and chunk count,
// for one namespace or (namespace "") all of them.
func (s *Service) Status(ctx context.Context, namespace string) (StatusResult, error) {
	sql := `
        SELECT namespace, source_file, MAX(ingested_at) as ingested_at, COUNT(*) as chunk_count
        FROM memory_chunks
    `
	var args []any
	if namespace != "" {
		sql += " WHERE namespace = $1"
		args = append(args, namespace)
	}
	sql += " GROUP BY namespace, source_file ORDER BY namespace, source_file"

	rows, err := s.DB.Query(ctx, sql, args...)
	if err != nil {
		return StatusResult{}, err
	}
	defer rows.Close()
	out := StatusResult{Files: []StatusFile{}}
	for rows.Next() {
		var f StatusFile
		var at time.Time
		var count int64
		if err := rows.Scan(&f.Namespace, &f.SourceFile, &at, &count); err != nil {
			return StatusResult{}, err
		}
		f.IngestedAt = jsutil.DateISO(at)
		f.ChunkCount = BigCount(count)
		out.Files = append(out.Files, f)
	}
	return out, rows.Err()
}
