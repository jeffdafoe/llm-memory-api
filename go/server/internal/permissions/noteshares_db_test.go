package permissions

import (
	"context"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// The share rule (ShareCovers) on a real Postgres: a pattern without a
// trailing "/" is one note, a pattern with one is a folder, both compare
// without case, and "_" or "%" in a pattern is a plain character.
func TestHasNoteAccessShareCoverage(t *testing.T) {
	pool := testutil.ScratchDB(t, `
CREATE TABLE note_permissions (
	owner_namespace text NOT NULL,
	slug_pattern text NOT NULL,
	grantee_actor_id integer,
	can_read boolean NOT NULL DEFAULT false,
	can_write boolean NOT NULL DEFAULT false,
	can_delete boolean NOT NULL DEFAULT false,
	revoked_at timestamptz
);
INSERT INTO note_permissions (owner_namespace, slug_pattern, grantee_actor_id, can_read) VALUES
	('owner', 'notes/a', 7, true),
	('owner', 'my_notes/', 7, true),
	('owner', 'Public/', 7, true);`)
	shares := &NoteShares{DB: pool}

	cases := []struct {
		slug string
		want bool
	}{
		{"notes/a", true},
		{"NOTES/A", true},
		{"notes/a-private", false},
		{"notes/a/x", false},
		{"notes/ab", false},
		{"my_notes/x", true},
		{"myXnotes/x", false},
		{"public/x", true},
		{"PUBLIC/x/y", true},
		{"public", false},
		{"other/x", false},
	}
	for _, c := range cases {
		slug := c.slug
		got, err := shares.HasNoteAccess(context.Background(), "owner", &slug, 7, Read)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s: access %v, want %v", c.slug, got, c.want)
		}
	}
	// Another grantee, or no slug, has no access.
	slug := "notes/a"
	if got, err := shares.HasNoteAccess(context.Background(), "owner", &slug, 8, Read); err != nil || got {
		t.Errorf("other grantee: %v, %v", got, err)
	}
	if got, err := shares.HasNoteAccess(context.Background(), "owner", nil, 7, Read); err != nil || got {
		t.Errorf("nil slug: %v, %v", got, err)
	}
}
