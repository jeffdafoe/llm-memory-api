package notes

import (
	"context"
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
)

// PrefixConflict is a live note at a destination slug of a prefix move.
// source is the slug of the note that would move onto it, and overwrite
// whether the caller named the destination in OverwriteSlugs; neither is
// part of the response.
type PrefixConflict struct {
	Slug      string  `json:"slug"`
	Title     *string `json:"title"`
	source    string
	overwrite bool
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
// three must move together). The chunks and mappings take the note's exact
// new slug. A destination slug that already holds a live
// note is soft-deleted first when it is in OverwriteSlugs; otherwise the
// source note that collides with it stays where it is.
func (s *Service) MovePrefix(ctx context.Context, in MovePrefixInput) (PrefixMoved, error) {
	_, conflicts, err := s.prefixScan(ctx, in)
	if err != nil {
		return PrefixMoved{}, err
	}

	var toSkip, toOverwrite []PrefixConflict
	for _, c := range conflicts {
		if c.overwrite {
			toOverwrite = append(toOverwrite, c)
		} else {
			toSkip = append(toSkip, c)
		}
	}
	// Each skipped conflict's source slug, which stays put.
	skipSlugs := make([]string, len(toSkip))
	for i, c := range toSkip {
		skipSlugs[i] = c.source
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
                WHERE namespace = $1
                  AND LOWER(source_file) IN (SELECT LOWER(t) FROM unnest($2::text[]) t)
            `, in.Namespace, targets); err != nil {
			return PrefixMoved{}, err
		}
	}

	// The notes move first. The self-join keeps each row's old slug for
	// RETURNING (old is the row as read, d the row as written), so the chunks
	// and sync mappings below follow the note to its exact new slug, whatever
	// case they themselves are stored in. skipSlugs (possibly empty: != ALL
	// of an empty array is true) are the skipped conflicts' sources.
	rows, err := tx.Query(ctx, `
            UPDATE documents d
            SET slug = $3 || substring(d.slug FROM length($2) + 1), updated_at = NOW()
            FROM documents old
            WHERE old.id = d.id
              AND d.namespace = $1 AND LOWER(d.slug) LIKE LOWER($4) || '%' AND d.deleted_at IS NULL
              AND d.slug != ALL($5)
            RETURNING old.slug, d.slug
        `, in.Namespace, in.OldPrefix, in.NewPrefix, oldLike, skipSlugs)
	if err != nil {
		return PrefixMoved{}, err
	}
	oldSlugs, newSlugs := []string{}, []string{}
	for rows.Next() {
		var oldSlug, newSlug string
		if err := rows.Scan(&oldSlug, &newSlug); err != nil {
			rows.Close()
			return PrefixMoved{}, err
		}
		oldSlugs = append(oldSlugs, oldSlug)
		newSlugs = append(newSlugs, newSlug)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PrefixMoved{}, err
	}

	// A moved note's chunks take its new slug. unnest of two arrays pairs
	// them up row by row: (old_slug, new_slug).
	if _, err := tx.Exec(ctx, `
            UPDATE memory_chunks mc
            SET source_file = m.new_slug
            FROM unnest($2::text[], $3::text[]) AS m(old_slug, new_slug)
            WHERE mc.namespace = $1 AND LOWER(mc.source_file) = LOWER(m.old_slug)
        `, in.Namespace, oldSlugs, newSlugs); err != nil {
		return PrefixMoved{}, err
	}
	// Chunks under the old prefix with no live note (raw ingests) keep the
	// prefix rule. A skipped note's chunks have their live note and stay.
	if _, err := tx.Exec(ctx, `
            UPDATE memory_chunks mc
            SET source_file = $3 || substring(mc.source_file FROM length($2) + 1)
            WHERE mc.namespace = $1 AND LOWER(mc.source_file) LIKE LOWER($4) || '%'
              AND NOT EXISTS (
                  SELECT 1 FROM documents d
                  WHERE d.namespace = mc.namespace
                    AND LOWER(d.slug) = LOWER(mc.source_file)
                    AND d.deleted_at IS NULL
              )
        `, in.Namespace, in.OldPrefix, in.NewPrefix, oldLike); err != nil {
		return PrefixMoved{}, err
	}

	// Sync mappings follow the moved notes to their exact new slug.
	// note_synchronization is unique per actor, namespace and slug by exact
	// case, so per actor at most one mapping of a note moves: the one spelled
	// like the note, else the oldest (lowest id). The others, and a mapping
	// whose actor already maps the new slug, stay; the next sync drops the
	// old file.
	if _, err := tx.Exec(ctx, `
            UPDATE note_synchronization src
            SET slug = m.new_slug
            FROM unnest($2::text[], $3::text[]) AS m(old_slug, new_slug)
            WHERE src.namespace = $1 AND LOWER(src.slug) = LOWER(m.old_slug)
              AND NOT EXISTS (
                  SELECT 1 FROM note_synchronization twin
                  WHERE twin.actor_id = src.actor_id
                    AND twin.namespace = src.namespace
                    AND LOWER(twin.slug) = LOWER(src.slug)
                    AND twin.id <> src.id
                    AND (
                        (twin.slug = m.old_slug AND src.slug <> m.old_slug)
                        OR ((twin.slug = m.old_slug) = (src.slug = m.old_slug) AND twin.id < src.id)
                    )
              )
              AND NOT EXISTS (
                  SELECT 1 FROM note_synchronization dst
                  WHERE dst.actor_id = src.actor_id
                    AND dst.namespace = src.namespace
                    AND dst.slug = m.new_slug
              )
        `, in.Namespace, oldSlugs, newSlugs); err != nil {
		return PrefixMoved{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return PrefixMoved{}, err
	}
	return PrefixMoved{Moved: len(newSlugs), Skipped: len(toSkip), Overwritten: len(toOverwrite)}, nil
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
	// Prefixes compare without case, as slugs do; a rename that only changes
	// the prefix's case ("notes/" to "Notes/") is allowed. Postgres does the
	// lowercasing, so the guard follows the same collation as the queries
	// that pick the notes (Go's strings.ToLower can disagree, e.g. with a
	// Turkish collation).
	if in.NewPrefix != "" {
		var sameIgnoringCase, nested bool
		if err := s.DB.QueryRow(ctx,
			"SELECT LOWER($1) = LOWER($2), starts_with(LOWER($2), LOWER($1))",
			in.OldPrefix, in.NewPrefix).Scan(&sameIgnoringCase, &nested); err != nil {
			return 0, nil, err
		}
		caseOnly := sameIgnoringCase && in.NewPrefix != in.OldPrefix
		if nested && !caseOnly {
			return 0, nil, apierr.New(http.StatusBadRequest,
				`Cannot move prefix "`+in.OldPrefix+`" to "`+in.NewPrefix+`": the destination is the same as, or nested under, the source.`)
		}
	}
	oldLike := memory.EscapeLikePattern(in.OldPrefix)

	tag, err := s.DB.Exec(ctx, `
        SELECT slug, title FROM documents
        WHERE namespace = $1 AND LOWER(slug) LIKE LOWER($2) || '%' AND deleted_at IS NULL
    `, in.Namespace, oldLike)
	if err != nil {
		return 0, nil, err
	}

	// overwrite is whether the caller named the destination in
	// OverwriteSlugs, matched without case by the same LOWER as the slugs.
	overwrites := in.OverwriteSlugs
	if overwrites == nil {
		overwrites = []string{}
	}
	rows, err := s.DB.Query(ctx, `
        SELECT d2.slug, d2.title, d1.slug,
               LOWER(d2.slug) IN (SELECT LOWER(t) FROM unnest($5::text[]) t) AS overwrite
        FROM documents d1
        JOIN documents d2
          ON d2.namespace = d1.namespace
          AND LOWER(d2.slug) = LOWER($3 || substring(d1.slug FROM length($2) + 1))
          AND d2.deleted_at IS NULL
          AND d2.id <> d1.id
        WHERE d1.namespace = $1
          AND LOWER(d1.slug) LIKE LOWER($4) || '%'
          AND d1.deleted_at IS NULL
        ORDER BY d2.slug
    `, in.Namespace, in.OldPrefix, in.NewPrefix, oldLike, overwrites)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	conflicts := []PrefixConflict{}
	for rows.Next() {
		var c PrefixConflict
		if err := rows.Scan(&c.Slug, &c.Title, &c.source, &c.overwrite); err != nil {
			return 0, nil, err
		}
		conflicts = append(conflicts, c)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	return int(tag.RowsAffected()), conflicts, nil
}
