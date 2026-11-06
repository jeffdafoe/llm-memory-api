// Package notes is the documents service: save, list, read, delete, restore,
// edit and move notes (Node's services/documents.js). Every write keeps the
// note's search chunks (memory package) and the namespace usage counters in
// step, in the background.
package notes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/events"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/syserrors"
)

// Service runs the notes operations against one database.
type Service struct {
	DB       db.DB
	Config   *config.Store
	Actors   *actors.Cache
	Memory   *memory.Service
	Errors   *syserrors.Recorder
	Events   events.Broadcaster
	Enricher Enricher
}

// Time is a timestamptz in a response, as node-postgres put it in JSON.
type Time = *string

func jsTime(t *time.Time) Time {
	if t == nil {
		return nil
	}
	s := jsutil.DateISO(*t)
	return &s
}

// JSON is a jsonb column in a response: node-postgres parsed it and
// JSON.stringify wrote it again, so it is re-encoded the same way. NULL is
// null.
type JSON []byte

// MarshalJSON writes the value as JSON.stringify would.
func (j JSON) MarshalJSON() ([]byte, error) {
	if j == nil {
		return []byte("null"), nil
	}
	s, err := jsutil.Stringify(j)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

func (s *Service) broadcast(namespace, slug, operation string) {
	if s.Events != nil {
		s.Events.Broadcast("note_updated", map[string]string{"namespace": namespace, "slug": slug, "operation": operation})
	}
}

// background runs fn off the request; an error is recorded in
// system_errors under source "documents" with code and the note's
// namespace and slug.
func (s *Service) background(code, namespace, slug string, fn func(ctx context.Context) error) {
	db.Background(func(ctx context.Context) {
		if err := fn(ctx); err != nil && s.Errors != nil {
			s.Errors.Record(ctx, "", "documents", code, map[string]string{"namespace": namespace, "slug": slug, "error": err.Error()})
		}
	})
}

// updateUsage moves the namespace_usage counters in the background. Usage
// tracking never blocks or fails a note operation; a failure is dropped.
func (s *Service) updateUsage(namespace string, countDelta, bytesDelta int) {
	db.Background(func(ctx context.Context) {
		s.DB.Exec(ctx, `
        INSERT INTO namespace_usage (namespace, note_count, total_bytes, updated_at)
        VALUES ($1, $2, $3, NOW())
        ON CONFLICT (namespace) DO UPDATE SET
            note_count = GREATEST(0, namespace_usage.note_count + $2),
            total_bytes = GREATEST(0, namespace_usage.total_bytes + $3),
            updated_at = NOW()
    `, namespace, countDelta, bytesDelta)
	})
}

// checkQuota returns the 413 message when adding bytes to the namespace
// would pass its storage quota, else "". The quota is the agent's
// agent_configuration.storage_quota when set, else default_storage_quota;
// a missing, unparseable or non-positive quota means none.
func (s *Service) checkQuota(ctx context.Context, namespace string, additional int) (string, error) {
	var current int64
	err := s.DB.QueryRow(ctx, "SELECT total_bytes FROM namespace_usage WHERE namespace = $1", namespace).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}

	var quota float64
	var agentQuota *int64
	err = s.DB.QueryRow(ctx, `SELECT agc.storage_quota
         FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE ac.name = $1`, namespace).Scan(&agentQuota)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if agentQuota != nil {
		quota = float64(*agentQuota)
	} else {
		v, ok := s.Config.Lookup("default_storage_quota")
		if !ok {
			return "", nil
		}
		n, parsed := jsutil.ParseInt(v)
		if !parsed {
			return "", nil
		}
		quota = n
	}
	if quota <= 0 {
		return "", nil
	}
	if float64(current)+float64(additional) > quota {
		const mb = 1024 * 1024
		return "Storage quota exceeded (" + jsutil.ToFixed(float64(current)/mb, 1) + " MB / " + jsutil.ToFixed(quota/mb, 1) + " MB limit)", nil
	}
	return "", nil
}

func (s *Service) maxNoteBytes() (int, error) {
	v, err := s.Config.Get("note_maximum_size")
	if err != nil {
		return 0, err
	}
	return jsutil.IntOr(v, 512000), nil
}

func kb(n int) string {
	return jsutil.NumberString(jsutil.Round(float64(n) / 1024))
}

// createdByName resolves created_by_actor_id to the actor's name.
func (s *Service) createdByName(ctx context.Context, id *int) (*string, error) {
	if id == nil || *id == 0 {
		return nil, nil
	}
	a, ok, err := s.Actors.ResolveByID(ctx, *id)
	if err != nil || !ok {
		return nil, err
	}
	return &a.Name, nil
}

// SaveInput is one save. Slug "" derives the slug from the title;
// SlugNotString marks a slug of another JSON type, which Node refused as
// "Slug is required". Metadata is the raw JSON value or nil; Extension ""
// is none.
type SaveInput struct {
	Namespace     string
	Title         string
	Content       string
	Slug          string
	SlugNotString bool
	CreatedBy     string
	Metadata      json.RawMessage
	Extension     string
	// Upsert overwrites the live note at the slug instead of a 409.
	Upsert bool
}

// SavedNote is the save response.
type SavedNote struct {
	ID               int     `json:"id"`
	Namespace        string  `json:"namespace"`
	Slug             string  `json:"slug"`
	Title            *string `json:"title"`
	CreatedByActorID *int    `json:"created_by_actor_id"`
	CreatedAt        Time    `json:"created_at"`
	UpdatedAt        Time    `json:"updated_at"`
	Metadata         JSON    `json:"metadata"`
	Extension        *string `json:"extension"`
	CreatedBy        *string `json:"created_by"`
}

const savedColumns = "id, namespace, slug, title, created_by_actor_id, created_at, updated_at, metadata, extension"

func scanSaved(row pgx.Row) (SavedNote, error) {
	var n SavedNote
	var created, updated *time.Time
	var meta []byte
	err := row.Scan(&n.ID, &n.Namespace, &n.Slug, &n.Title, &n.CreatedByActorID, &created, &updated, &meta, &n.Extension)
	n.CreatedAt, n.UpdatedAt, n.Metadata = jsTime(created), jsTime(updated), meta
	return n, err
}

// Save creates a note, or with Upsert overwrites the live one at its slug.
// Afterwards, in the background: the note is indexed for search, the usage
// counters move, and enrichment runs when it is turned on.
func (s *Service) Save(ctx context.Context, in SaveInput) (SavedNote, error) {
	if in.Title == "" || in.Content == "" {
		return SavedNote{}, badRequest("Required fields: title, content")
	}
	maxBytes, err := s.maxNoteBytes()
	if err != nil {
		return SavedNote{}, err
	}
	// Node gave this a 500 status; the route answers "Failed: save".
	if len(in.Content) > maxBytes {
		return SavedNote{}, apierr.New(http.StatusInternalServerError,
			"Note content exceeds maximum size ("+kb(len(in.Content))+"KB / "+kb(maxBytes)+"KB limit)")
	}

	slug := in.Slug
	if slug == "" && !in.SlugNotString {
		slug = TitleToSlug(in.Title)
		if slug == "" {
			return SavedNote{}, badRequest("Could not generate slug from title")
		}
	}
	if err := ValidateSlug(slug, in.SlugNotString, false); err != nil {
		return SavedNote{}, err
	}
	if strings.HasPrefix(slug, in.Namespace+"/") {
		return SavedNote{}, badRequest(`Slug "` + slug + `" starts with its own namespace "` + in.Namespace + `/". ` +
			`The namespace is already implicit — use "` + slug[len(in.Namespace)+1:] + `" instead.`)
	}

	// The live note an upsert overwrites, by id: matching (namespace,
	// LOWER(slug)) in the UPDATE would also revive a soft-deleted row at the
	// same slug, and two live rows break the partial unique index.
	var existingID, existingLength int
	existing := false
	if in.Upsert {
		var length *int
		err := s.DB.QueryRow(ctx,
			"SELECT id, LENGTH(content) AS content_length FROM documents WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NULL",
			in.Namespace, slug).Scan(&existingID, &length)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return SavedNote{}, err
		}
		if err == nil {
			existing = true
			if length != nil {
				existingLength = *length
			}
		}
	}

	// content.length in Node: UTF-16 code units, compared with Postgres
	// LENGTH (characters). Kept as it was.
	contentLength := jsutil.UTF16Len(in.Content)
	additional := contentLength
	if existing {
		additional = contentLength - existingLength
	}
	if additional > 0 {
		msg, err := s.checkQuota(ctx, in.Namespace, additional)
		if err != nil {
			return SavedNote{}, err
		}
		if msg != "" {
			return SavedNote{}, apierr.New(http.StatusRequestEntityTooLarge, msg)
		}
	}

	var createdByID *int
	if in.CreatedBy != "" {
		a, ok, err := s.Actors.ResolveByName(ctx, in.CreatedBy)
		if err != nil {
			return SavedNote{}, err
		}
		if ok {
			createdByID = &a.ID
		}
	}

	kind := SlugToKind(slug)
	metadata, err := metadataJSON(in.Metadata)
	if err != nil {
		return SavedNote{}, err
	}
	extension := cleanExtension(in.Extension)

	var doc SavedNote
	if existing {
		sets := ""
		params := []any{in.Title, in.Content, existingID, kind}
		// Explicit casts: the optional parameters have no other type
		// context in the assignment.
		if metadata != nil {
			params = append(params, *metadata)
			sets += fmt.Sprintf(", metadata = $%d::jsonb", len(params))
		}
		if extension != nil {
			params = append(params, *extension)
			sets += fmt.Sprintf(", extension = $%d::varchar", len(params))
		}
		doc, err = scanSaved(s.DB.QueryRow(ctx, `
            UPDATE documents
            SET title = $1, content = $2, deleted_at = NULL, updated_at = NOW(), kind = $4`+sets+`
            WHERE id = $3
            RETURNING `+savedColumns, params...))
		if err != nil {
			return SavedNote{}, err
		}
	} else {
		doc, err = scanSaved(s.DB.QueryRow(ctx, `
                INSERT INTO documents (namespace, slug, title, content, created_by_actor_id, kind, metadata, extension)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
                RETURNING `+savedColumns,
			in.Namespace, slug, in.Title, in.Content, createdByID, kind, metadata, extension))
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return SavedNote{}, apierr.WithCode(http.StatusConflict, "DUPLICATE_SLUG",
					`Note already exists at slug "`+slug+`" in namespace "`+in.Namespace+`". Use edit_note to update existing notes, or pass upsert:true to overwrite.`)
			}
			return SavedNote{}, err
		}
	}

	if existing {
		s.updateUsage(in.Namespace, 0, contentLength-existingLength)
	} else {
		s.updateUsage(in.Namespace, 1, contentLength)
	}

	if doc.CreatedBy, err = s.createdByName(ctx, doc.CreatedByActorID); err != nil {
		return SavedNote{}, err
	}

	namespace, content := in.Namespace, in.Content
	s.background("AUTO_INDEX_FAILED", namespace, slug, func(ctx context.Context) error {
		_, err := s.Memory.Ingest(ctx, namespace, slug, content)
		return err
	})
	s.broadcast(namespace, slug, "saved")
	if s.Enricher != nil {
		meta := []byte(doc.Metadata)
		title := in.Title
		s.background("NOTE_ENRICHMENT_FAILED", namespace, slug, func(ctx context.Context) error {
			return s.Enricher.Enrich(ctx, namespace, slug, title, content, meta)
		})
	}
	return doc, nil
}

// metadataJSON is a save's metadata as stored: a plain object of at most
// 10 KB serialized (JSON.stringify of the value, in UTF-16 units). A falsy
// value is no metadata.
func metadataJSON(raw json.RawMessage) (*string, error) {
	if !pgbind.Truthy(raw) {
		return nil, nil
	}
	raw = bytes.TrimSpace(raw)
	if raw[0] != '{' {
		return nil, badRequest("metadata must be a plain object")
	}
	// Node kept a lone surrogate and Postgres refused the jsonb at the
	// write, a 500; Go's decoder would store U+FFFD instead.
	if jsutil.HasLoneSurrogateEscape(raw) {
		return nil, errors.New("metadata holds a lone UTF-16 surrogate, which jsonb cannot store")
	}
	serialized, err := jsutil.Stringify(raw)
	if err != nil {
		return nil, badRequest("metadata must be a plain object")
	}
	if jsutil.UTF16Len(serialized) > 10240 {
		return nil, badRequest("metadata exceeds 10KB limit")
	}
	return &serialized, nil
}

// cleanExtension keeps an extension of the form ".abc" (dot, lowercase
// letters and digits, 2 to 20 characters); anything else is none.
func cleanExtension(ext string) *string {
	if ext == "" {
		return nil
	}
	ext = jsutil.ToLower(ext)
	ext = "." + strings.TrimPrefix(ext, ".")
	var b strings.Builder
	for _, r := range ext {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' {
			b.WriteRune(r)
		}
	}
	clean := b.String()
	if len(clean) < 2 || len(clean) > 20 || strings.Contains(clean[1:], ".") {
		return nil
	}
	return &clean
}

// ListInput is one listing. Limit and Offset are the caller's raw JSON
// values (nil when absent); List applies `limit || 50` and `offset || 0`.
type ListInput struct {
	Namespace      string
	Limit, Offset  json.RawMessage
	Prefix         string
	IncludeDeleted bool
}

// ListedNote is one row of a listing.
type ListedNote struct {
	ID             int     `json:"id"`
	Slug           string  `json:"slug"`
	Title          *string `json:"title"`
	Snippet        string  `json:"snippet"`
	ContentHash    string  `json:"content_hash"`
	CreatedBy      *string `json:"created_by"`
	CreatedAt      Time    `json:"created_at"`
	UpdatedAt      Time    `json:"updated_at"`
	UpdatedAtExact *string `json:"updated_at_exact"`
	LastAccessed   Time    `json:"last_accessed"`
}

// ListedNoteWithDeleted adds deleted_at, which only include_deleted lists.
type ListedNoteWithDeleted struct {
	ListedNote
	DeletedAt Time `json:"deleted_at"`
}

// List returns a namespace's notes, newest update first. A prefix filters
// by slug like a folder; it is a LIKE pattern, not escaped, as in Node.
// updated_at_exact is the microsecond text memory-sync passes back to a
// conditional delete.
func (s *Service) List(ctx context.Context, in ListInput) (any, error) {
	plain, withDeleted, err := s.list(ctx, in)
	if err != nil {
		return nil, err
	}
	if in.IncludeDeleted {
		return struct {
			Notes []ListedNoteWithDeleted `json:"notes"`
		}{withDeleted}, nil
	}
	return struct {
		Notes []ListedNote `json:"notes"`
	}{plain}, nil
}

// ListLive is List without deleted notes, as rows for a caller that uses
// them itself (memory-sync). IncludeDeleted is ignored.
func (s *Service) ListLive(ctx context.Context, in ListInput) ([]ListedNote, error) {
	in.IncludeDeleted = false
	plain, _, err := s.list(ctx, in)
	return plain, err
}

func (s *Service) list(ctx context.Context, in ListInput) ([]ListedNote, []ListedNoteWithDeleted, error) {
	deletedFilter, deletedCol := " AND d.deleted_at IS NULL", ""
	if in.IncludeDeleted {
		deletedFilter, deletedCol = "", ", d.deleted_at"
	}
	cols := `
            SELECT d.id, d.slug, d.title,
                   LEFT(d.content, 200) AS snippet,
                   MD5(d.content) AS content_hash,
                   ac.name AS created_by, d.created_at, d.updated_at,
                   to_char(d.updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') AS updated_at_exact,
                   d.last_accessed` + deletedCol + `
            FROM documents d
            LEFT JOIN actors ac ON ac.id = d.created_by_actor_id`
	var sql string
	params := []any{in.Namespace, pgbind.OrDefault(in.Limit, 50), pgbind.OrDefault(in.Offset, 0)}
	if in.Prefix != "" {
		sql = cols + `
            WHERE d.namespace = $1 AND LOWER(d.slug) LIKE LOWER($4)` + deletedFilter + `
            ORDER BY d.updated_at DESC, d.slug ASC
            LIMIT $2 OFFSET $3`
		params = append(params, in.Prefix+"%")
	} else {
		sql = cols + `
            WHERE d.namespace = $1` + deletedFilter + `
            ORDER BY d.updated_at DESC
            LIMIT $2 OFFSET $3`
	}

	rows, err := s.DB.Query(ctx, sql, params...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	plain := []ListedNote{}
	withDeleted := []ListedNoteWithDeleted{}
	for rows.Next() {
		var n ListedNote
		var created, updated, accessed, deleted *time.Time
		dest := []any{&n.ID, &n.Slug, &n.Title, &n.Snippet, &n.ContentHash, &n.CreatedBy, &created, &updated, &n.UpdatedAtExact, &accessed}
		if in.IncludeDeleted {
			dest = append(dest, &deleted)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, err
		}
		n.CreatedAt, n.UpdatedAt, n.LastAccessed = jsTime(created), jsTime(updated), jsTime(accessed)
		if in.IncludeDeleted {
			withDeleted = append(withDeleted, ListedNoteWithDeleted{ListedNote: n, DeletedAt: jsTime(deleted)})
		} else {
			plain = append(plain, n)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return plain, withDeleted, nil
}

// Note is a read response. TotalLines is set for a paginated read.
type Note struct {
	ID         int     `json:"id"`
	Namespace  string  `json:"namespace"`
	Slug       string  `json:"slug"`
	Title      *string `json:"title"`
	Content    string  `json:"content"`
	CreatedBy  *string `json:"created_by"`
	CreatedAt  Time    `json:"created_at"`
	UpdatedAt  Time    `json:"updated_at"`
	Metadata   JSON    `json:"metadata"`
	Extension  *string `json:"extension"`
	TotalLines *int    `json:"total_lines,omitempty"`
}

func notFound(slug string) error {
	return apierr.New(http.StatusNotFound, "Note not found: "+slug)
}

// Read returns a live note (slug matched case-insensitively) and stamps its
// last_accessed in the background, which feeds the search decay and boost.
func (s *Service) Read(ctx context.Context, namespace, slug string) (Note, error) {
	var n Note
	var created, updated *time.Time
	var meta []byte
	err := s.DB.QueryRow(ctx, `
        SELECT d.id, d.namespace, d.slug, d.title, d.content,
               ac.name AS created_by, d.created_at, d.updated_at, d.metadata, d.extension
        FROM documents d
        LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
        WHERE d.namespace = $1 AND LOWER(d.slug) = LOWER($2) AND d.deleted_at IS NULL
    `, namespace, slug).Scan(&n.ID, &n.Namespace, &n.Slug, &n.Title, &n.Content, &n.CreatedBy, &created, &updated, &meta, &n.Extension)
	if errors.Is(err, pgx.ErrNoRows) {
		return Note{}, notFound(slug)
	}
	if err != nil {
		return Note{}, err
	}
	n.CreatedAt, n.UpdatedAt, n.Metadata = jsTime(created), jsTime(updated), meta

	s.background("LAST_ACCESSED_UPDATE_FAILED", namespace, slug, func(ctx context.Context) error {
		_, err := s.DB.Exec(ctx, "UPDATE documents SET last_accessed = NOW() WHERE namespace = $1 AND LOWER(slug) = LOWER($2)", namespace, slug)
		return err
	})
	return n, nil
}

// Deleted is the delete response.
type Deleted struct {
	Deleted bool   `json:"deleted"`
	Slug    string `json:"slug"`
}

// Delete soft-deletes a live note and hard-deletes its chunks (in the
// background), so stale text stops reaching search. With expectedUpdatedAt
// (updated_at_exact from a listing) the delete happens only if the note is
// still that version: a note rewritten since is a 409, one already gone a
// 404. memory-sync's prune relies on this.
func (s *Service) Delete(ctx context.Context, namespace, slug string, expectedUpdatedAt *string) (Deleted, error) {
	params := []any{namespace, slug}
	versionClause := ""
	if expectedUpdatedAt != nil {
		params = append(params, *expectedUpdatedAt)
		versionClause = " AND updated_at = $3::timestamptz"
	}
	var id int
	var length *int
	err := s.DB.QueryRow(ctx, `
        UPDATE documents
        SET deleted_at = NOW()
        WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NULL`+versionClause+`
        RETURNING id, LENGTH(content) AS content_length
    `, params...).Scan(&id, &length)
	if errors.Is(err, pgx.ErrNoRows) {
		if expectedUpdatedAt != nil {
			var one int
			err := s.DB.QueryRow(ctx, "SELECT 1 FROM documents WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NULL", namespace, slug).Scan(&one)
			if err == nil {
				return Deleted{}, apierr.New(http.StatusConflict, "Note changed since it was read: "+slug)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return Deleted{}, err
			}
		}
		return Deleted{}, notFound(slug)
	}
	if err != nil {
		return Deleted{}, err
	}

	db.Background(func(ctx context.Context) {
		s.DB.Exec(ctx, "DELETE FROM memory_chunks WHERE namespace = $1 AND LOWER(source_file) = LOWER($2)", namespace, slug)
	})
	bytes := 0
	if length != nil {
		bytes = *length
	}
	s.updateUsage(namespace, -1, -bytes)
	s.broadcast(namespace, slug, "deleted")
	return Deleted{Deleted: true, Slug: slug}, nil
}

// Restored is the restore response.
type Restored struct {
	ID        int     `json:"id"`
	Namespace string  `json:"namespace"`
	Slug      string  `json:"slug"`
	Title     *string `json:"title"`
}

// Restore clears deleted_at on a soft-deleted note. Its chunks were removed
// at delete; a later save re-indexes it. A retention tombstone (content
// purged, metadata.purged_at set) is 410: restoring it would bring back an
// empty note.
func (s *Service) Restore(ctx context.Context, namespace, slug string) (Restored, error) {
	var id int
	var length *int
	var purgedAt *string
	err := s.DB.QueryRow(ctx,
		`SELECT id, LENGTH(content) AS content_length, metadata->>'purged_at' AS purged_at
         FROM documents WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NOT NULL`,
		namespace, slug).Scan(&id, &length, &purgedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Restored{}, apierr.New(http.StatusNotFound, "No deleted note found: "+slug)
	}
	if err != nil {
		return Restored{}, err
	}
	if purgedAt != nil && *purgedAt != "" {
		return Restored{}, apierr.New(http.StatusGone,
			"Note content was purged by retention on "+*purgedAt+" and cannot be restored: "+slug)
	}

	bytes := 0
	if length != nil {
		bytes = *length
	}
	msg, err := s.checkQuota(ctx, namespace, bytes)
	if err != nil {
		return Restored{}, err
	}
	if msg != "" {
		return Restored{}, apierr.New(http.StatusRequestEntityTooLarge, msg)
	}

	var r Restored
	err = s.DB.QueryRow(ctx, `
        UPDATE documents d
        SET deleted_at = NULL
        WHERE d.id = $1
        RETURNING d.id, d.namespace, d.slug, d.title
    `, id).Scan(&r.ID, &r.Namespace, &r.Slug, &r.Title)
	if err != nil {
		return Restored{}, err
	}
	s.updateUsage(namespace, 1, bytes)
	s.broadcast(namespace, slug, "restored")
	return r, nil
}

// changedNote is the row an edit or a move returns.
type changedNote struct {
	ID               int     `json:"id"`
	Namespace        string  `json:"namespace"`
	Slug             string  `json:"slug"`
	Title            *string `json:"title"`
	CreatedByActorID *int    `json:"created_by_actor_id"`
	CreatedAt        Time    `json:"created_at"`
	UpdatedAt        Time    `json:"updated_at"`
	CreatedBy        *string `json:"created_by"`
}

// scanChanged reads the first row of an UPDATE ... RETURNING that may touch
// several rows (the WHERE does not exclude soft-deleted rows at the slug, as
// in Node). ok is false when it touched none. created_by is resolved by the
// caller, at the point Node resolved it.
func scanChanged(rows pgx.Rows) (changedNote, bool, error) {
	var n changedNote
	found := false
	for rows.Next() {
		if found {
			continue
		}
		var created, updated *time.Time
		if err := rows.Scan(&n.ID, &n.Namespace, &n.Slug, &n.Title, &n.CreatedByActorID, &created, &updated); err != nil {
			rows.Close()
			return changedNote{}, false, err
		}
		n.CreatedAt, n.UpdatedAt = jsTime(created), jsTime(updated)
		found = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return changedNote{}, false, err
	}
	return n, found, nil
}

// Edited is the edit response: the note's row and how many occurrences
// were replaced.
type Edited struct {
	*changedNote
	Replacements int `json:"replacements"`
}

// MarshalJSON writes the note's fields then replacements, or only
// replacements when the update touched no row (as Node's spread of
// undefined did).
func (e Edited) MarshalJSON() ([]byte, error) {
	if e.changedNote == nil {
		return []byte(fmt.Sprintf(`{"replacements":%d}`, e.Replacements)), nil
	}
	type plain Edited
	return json.Marshal(plain(e))
}

// EditInput is one search-and-replace edit. NewString nil is a missing
// new_string. ReplaceAll is the truthiness of replace_all.
type EditInput struct {
	Namespace, Slug string
	OldString       string
	NewString       *string
	ReplaceAll      bool
}

// Edit replaces OldString in the note's content. It must occur exactly once
// unless ReplaceAll. The replacement is literal. The note is re-indexed in
// the background.
func (s *Service) Edit(ctx context.Context, in EditInput) (Edited, error) {
	if in.OldString == "" || in.NewString == nil {
		return Edited{}, badRequest("Required fields: old_string, new_string")
	}
	doc, err := s.Read(ctx, in.Namespace, in.Slug)
	if err != nil {
		return Edited{}, err
	}
	content := doc.Content
	count := strings.Count(content, in.OldString)
	if count == 0 {
		return Edited{}, badRequest("old_string not found in document")
	}
	if count > 1 && !in.ReplaceAll {
		return Edited{}, badRequest(fmt.Sprintf("old_string appears %d times — use replace_all to replace all occurrences, or provide more context to make it unique", count))
	}
	updated := strings.ReplaceAll(content, in.OldString, *in.NewString)

	maxBytes, err := s.maxNoteBytes()
	if err != nil {
		return Edited{}, err
	}
	if len(updated) > maxBytes {
		return Edited{}, apierr.New(http.StatusInternalServerError,
			"Edit would exceed maximum note size ("+kb(len(updated))+"KB / "+kb(maxBytes)+"KB limit)")
	}
	delta := jsutil.UTF16Len(updated) - jsutil.UTF16Len(content)
	if delta > 0 {
		msg, err := s.checkQuota(ctx, in.Namespace, delta)
		if err != nil {
			return Edited{}, err
		}
		if msg != "" {
			return Edited{}, apierr.New(http.StatusRequestEntityTooLarge, msg)
		}
	}

	rows, err := s.DB.Query(ctx, `
        UPDATE documents d
        SET content = $1, updated_at = NOW()
        WHERE d.namespace = $2 AND LOWER(d.slug) = LOWER($3)
        RETURNING d.id, d.namespace, d.slug, d.title, d.created_by_actor_id, d.created_at, d.updated_at
    `, updated, in.Namespace, in.Slug)
	if err != nil {
		return Edited{}, err
	}
	note, found, err := scanChanged(rows)
	if err != nil {
		return Edited{}, err
	}
	s.updateUsage(in.Namespace, 0, delta)
	if found {
		if note.CreatedBy, err = s.createdByName(ctx, note.CreatedByActorID); err != nil {
			return Edited{}, err
		}
	}

	namespace, slug := in.Namespace, in.Slug
	s.background("REINDEX_FAILED", namespace, slug, func(ctx context.Context) error {
		_, err := s.Memory.Ingest(ctx, namespace, slug, updated)
		return err
	})
	s.broadcast(namespace, slug, "edited")

	out := Edited{Replacements: 1}
	if in.ReplaceAll {
		out.Replacements = count
	}
	if found {
		out.changedNote = &note
	}
	return out, nil
}

// MoveInput is one move. NewNamespace "" keeps the namespace.
// NewSlugNotString marks a new_slug of another JSON type.
type MoveInput struct {
	Namespace, Slug  string
	NewSlug          string
	NewSlugNotString bool
	NewNamespace     string
}

// Move renames a note (and optionally moves it to another namespace),
// recomputing its kind, and moves its chunks with it. The target slug must
// not hold a live note (409).
func (s *Service) Move(ctx context.Context, in MoveInput) (any, error) {
	if err := ValidateSlug(in.NewSlug, in.NewSlugNotString, false); err != nil {
		return nil, err
	}
	target := in.NewNamespace
	if target == "" {
		target = in.Namespace
	}

	var sourceID int
	var title *string
	var length *int
	err := s.DB.QueryRow(ctx,
		"SELECT id, title, LENGTH(content) AS content_length FROM documents WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NULL",
		in.Namespace, in.Slug).Scan(&sourceID, &title, &length)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound(in.Slug)
	}
	if err != nil {
		return nil, err
	}

	var conflict int
	err = s.DB.QueryRow(ctx,
		"SELECT id FROM documents WHERE namespace = $1 AND LOWER(slug) = LOWER($2) AND deleted_at IS NULL",
		target, in.NewSlug).Scan(&conflict)
	if err == nil {
		return nil, apierr.New(http.StatusConflict, "Target slug already exists: "+target+"/"+in.NewSlug)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	rows, err := s.DB.Query(ctx, `
        UPDATE documents
        SET slug = $1, namespace = $2, updated_at = NOW(), kind = $5
        WHERE namespace = $3 AND LOWER(slug) = LOWER($4)
        RETURNING id, namespace, slug, title, created_by_actor_id, created_at, updated_at
    `, in.NewSlug, target, in.Namespace, in.Slug, SlugToKind(in.NewSlug))
	if err != nil {
		return nil, err
	}
	note, found, err := scanChanged(rows)
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(ctx,
		"UPDATE memory_chunks SET source_file = $1, namespace = $2 WHERE namespace = $3 AND LOWER(source_file) = LOWER($4)",
		in.NewSlug, target, in.Namespace, in.Slug); err != nil {
		return nil, err
	}
	if target != in.Namespace {
		size := 0
		if length != nil {
			size = *length
		}
		s.updateUsage(in.Namespace, -1, -size)
		s.updateUsage(target, 1, size)
	}
	// Node resolved created_by here, after the chunks and the usage moved,
	// and read it from an undefined row when the UPDATE touched none.
	if !found {
		return nil, errors.New("Cannot read properties of undefined (reading 'created_by_actor_id')")
	}
	if note.CreatedBy, err = s.createdByName(ctx, note.CreatedByActorID); err != nil {
		return nil, err
	}
	s.broadcast(in.Namespace, in.Slug, "moved")
	s.broadcast(target, in.NewSlug, "moved")
	return note, nil
}
