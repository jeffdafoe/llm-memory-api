package notes

import (
	"context"
	"net/http"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
)

// PrefixConflict is a live note at a destination slug of a prefix move.
type PrefixConflict struct {
	Slug  string  `json:"slug"`
	Title *string `json:"title"`
}

// PrefixPreview is a dry run of a prefix move.
type PrefixPreview struct {
	WouldMove int
	Conflicts []PrefixConflict
}

// PrefixMoved is the outcome of a prefix move.
type PrefixMoved struct {
	Moved, Skipped, Overwritten int
}

// MovePrefixInput is one prefix move. OverwriteSlugs are destination slugs
// to soft-delete first; any other conflict leaves its source note in place.
type MovePrefixInput struct {
	Namespace      string
	OldPrefix      string
	NewPrefix      string
	OverwriteSlugs []string
}

// PreviewMovePrefix is movePrefix with dryRun: how many notes would move and
// which destination slugs already hold a live note. Nothing changes.
func (s *Service) PreviewMovePrefix(ctx context.Context, in MovePrefixInput) (PrefixPreview, error) {
	sourceCount, conflicts, err := s.prefixScan(ctx, in)
	if err != nil {
		return PrefixPreview{}, err
	}
	return PrefixPreview{WouldMove: sourceCount, Conflicts: conflicts}, nil
}

// MovePrefix rewrites OldPrefix to NewPrefix on every live note under it in
// the namespace, with its chunks and sync mappings, in one transaction (the
// three must move together). A destination slug that already holds a live
// note is soft-deleted first when it is in OverwriteSlugs; otherwise the
// source note that collides with it stays where it is.
func (s *Service) MovePrefix(ctx context.Context, in MovePrefixInput) (PrefixMoved, error) {
	_, conflicts, err := s.prefixScan(ctx, in)
	if err != nil {
		return PrefixMoved{}, err
	}

	overwrite := map[string]bool{}
	for _, slug := range in.OverwriteSlugs {
		overwrite[slug] = true
	}
	var toSkip, toOverwrite []PrefixConflict
	for _, c := range conflicts {
		if overwrite[c.Slug] {
			toOverwrite = append(toOverwrite, c)
		} else {
			toSkip = append(toSkip, c)
		}
	}
	// Each skipped conflict's source slug, which stays put. A conflict's slug
	// equals NewPrefix plus the rest of its source slug, so cutting NewPrefix
	// off is Node's substring(newPrefix.length).
	skipSlugs := make([]string, len(toSkip))
	for i, c := range toSkip {
		skipSlugs[i] = in.OldPrefix + strings.TrimPrefix(c.Slug, in.NewPrefix)
	}
	oldLike := memory.EscapeLikePattern(in.OldPrefix)

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return PrefixMoved{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	if len(toOverwrite) > 0 {
		targets := make([]string, len(toOverwrite))
		for i, c := range toOverwrite {
			targets[i] = c.Slug
		}
		if _, err := tx.Exec(ctx, `
                UPDATE documents SET deleted_at = NOW()
                WHERE namespace = $1 AND slug = ANY($2) AND deleted_at IS NULL
            `, in.Namespace, targets); err != nil {
			return PrefixMoved{}, err
		}
		if _, err := tx.Exec(ctx, `
                DELETE FROM memory_chunks
                WHERE namespace = $1 AND source_file = ANY($2)
            `, in.Namespace, targets); err != nil {
			return PrefixMoved{}, err
		}
	}

	var moved int64
	if len(skipSlugs) > 0 {
		tag, err := tx.Exec(ctx, `
                UPDATE documents
                SET slug = $3 || substring(slug FROM length($2) + 1), updated_at = NOW()
                WHERE namespace = $1 AND slug LIKE $4 || '%' AND deleted_at IS NULL
                  AND slug != ALL($5)
                RETURNING id
            `, in.Namespace, in.OldPrefix, in.NewPrefix, oldLike, skipSlugs)
		if err != nil {
			return PrefixMoved{}, err
		}
		moved = tag.RowsAffected()
		if _, err := tx.Exec(ctx, `
                UPDATE memory_chunks
                SET source_file = $3 || substring(source_file FROM length($2) + 1)
                WHERE namespace = $1 AND source_file LIKE $4 || '%'
                  AND source_file != ALL($5)
            `, in.Namespace, in.OldPrefix, in.NewPrefix, oldLike, skipSlugs); err != nil {
			return PrefixMoved{}, err
		}
	} else {
		tag, err := tx.Exec(ctx, `
                UPDATE documents
                SET slug = $3 || substring(slug FROM length($2) + 1), updated_at = NOW()
                WHERE namespace = $1 AND slug LIKE $4 || '%' AND deleted_at IS NULL
                RETURNING id
            `, in.Namespace, in.OldPrefix, in.NewPrefix, oldLike)
		if err != nil {
			return PrefixMoved{}, err
		}
		moved = tag.RowsAffected()
		if _, err := tx.Exec(ctx, `
                UPDATE memory_chunks
                SET source_file = $3 || substring(source_file FROM length($2) + 1)
                WHERE namespace = $1 AND source_file LIKE $4 || '%'
            `, in.Namespace, in.OldPrefix, in.NewPrefix, oldLike); err != nil {
			return PrefixMoved{}, err
		}
	}

	// Sync mappings follow the notes, except a skipped note's (its document
	// stays at the old slug) and one whose actor already maps the
	// destination slug (note_synchronization is unique per actor, namespace
	// and slug; that row stays, and the next sync drops the old file).
	if len(skipSlugs) > 0 {
		if _, err := tx.Exec(ctx, `
                UPDATE note_synchronization src
                SET slug = $3 || substring(src.slug FROM length($2) + 1)
                WHERE src.namespace = $1 AND src.slug LIKE $4 || '%'
                  AND src.slug != ALL($5)
                  AND NOT EXISTS (
                      SELECT 1 FROM note_synchronization dst
                      WHERE dst.actor_id = src.actor_id
                        AND dst.namespace = src.namespace
                        AND dst.slug = $3 || substring(src.slug FROM length($2) + 1)
                  )
            `, in.Namespace, in.OldPrefix, in.NewPrefix, oldLike, skipSlugs); err != nil {
			return PrefixMoved{}, err
		}
	} else {
		if _, err := tx.Exec(ctx, `
                UPDATE note_synchronization src
                SET slug = $3 || substring(src.slug FROM length($2) + 1)
                WHERE src.namespace = $1 AND src.slug LIKE $4 || '%'
                  AND NOT EXISTS (
                      SELECT 1 FROM note_synchronization dst
                      WHERE dst.actor_id = src.actor_id
                        AND dst.namespace = src.namespace
                        AND dst.slug = $3 || substring(src.slug FROM length($2) + 1)
                  )
            `, in.Namespace, in.OldPrefix, in.NewPrefix, oldLike); err != nil {
			return PrefixMoved{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return PrefixMoved{}, err
	}
	return PrefixMoved{Moved: int(moved), Skipped: len(toSkip), Overwritten: len(toOverwrite)}, nil
}

// prefixScan validates the prefixes, then counts the live notes under the
// old prefix and lists the live notes at their destination slugs.
func (s *Service) prefixScan(ctx context.Context, in MovePrefixInput) (int, []PrefixConflict, error) {
	if err := ValidateSlug(in.OldPrefix, false, true); err != nil {
		return 0, nil, err
	}
	if in.NewPrefix != "" {
		if err := ValidateSlug(in.NewPrefix, false, true); err != nil {
			return 0, nil, err
		}
	}
	// A move into itself or under itself would rewrite a moved note again
	// (it is still in the source range). A move to the root ("") never nests.
	if in.NewPrefix != "" && strings.HasPrefix(in.NewPrefix, in.OldPrefix) {
		return 0, nil, apierr.New(http.StatusBadRequest,
			`Cannot move prefix "`+in.OldPrefix+`" to "`+in.NewPrefix+`": the destination is the same as, or nested under, the source.`)
	}
	oldLike := memory.EscapeLikePattern(in.OldPrefix)

	tag, err := s.DB.Exec(ctx, `
        SELECT slug, title FROM documents
        WHERE namespace = $1 AND slug LIKE $2 || '%' AND deleted_at IS NULL
    `, in.Namespace, oldLike)
	if err != nil {
		return 0, nil, err
	}

	rows, err := s.DB.Query(ctx, `
        SELECT d2.slug, d2.title FROM documents d1
        JOIN documents d2
          ON d2.namespace = d1.namespace
          AND d2.slug = $3 || substring(d1.slug FROM length($2) + 1)
          AND d2.deleted_at IS NULL
        WHERE d1.namespace = $1
          AND d1.slug LIKE $4 || '%'
          AND d1.deleted_at IS NULL
        ORDER BY d2.slug
    `, in.Namespace, in.OldPrefix, in.NewPrefix, oldLike)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	conflicts := []PrefixConflict{}
	for rows.Next() {
		var c PrefixConflict
		if err := rows.Scan(&c.Slug, &c.Title); err != nil {
			return 0, nil, err
		}
		conflicts = append(conflicts, c)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	return int(tag.RowsAffected()), conflicts, nil
}
