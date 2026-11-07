package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
)

type movePrefixPreview struct {
	DryRun    bool                   `json:"dry_run"`
	WouldMove int                    `json:"would_move"`
	Conflicts []notes.PrefixConflict `json:"conflicts"`
}

type movePrefixResult struct {
	Moved       int `json:"moved"`
	Skipped     int `json:"skipped"`
	Overwritten int `json:"overwritten"`
}

// notesMovePrefix is POST /admin/notes/move-prefix: movePrefix of
// services/documents.js on one namespace the caller may write. Only a
// dry_run of true previews; a real move is logged.
func (rt *ContentRoutes) notesMovePrefix(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("namespace")
	}
	newRaw := f.Raw("new_prefix")
	if !f.Truthy("namespace") || !f.Truthy("old_prefix") || newRaw == nil || string(newRaw) == "null" {
		return httpapi.BadRequest(w, "Required fields: namespace, old_prefix, new_prefix")
	}
	namespace, err := f.RequireString("namespace")
	if err != nil {
		return err
	}
	if err := permissions.ValidateNamespace(namespace); err != nil {
		return err
	}
	if err := rt.requireAccess(r, namespace, permissions.Write); err != nil {
		return err
	}

	// movePrefix validates the old prefix, then the new one unless it is "".
	// A prefix of another JSON type fails its check as "Slug is required".
	oldPrefix, oldIsString := f.String("old_prefix")
	newPrefix, newIsString := f.String("new_prefix")
	if !oldIsString || !newIsString {
		if err := notes.ValidateSlug(oldPrefix, !oldIsString, true); err != nil {
			return err
		}
		return notes.ValidateSlug(newPrefix, true, true)
	}
	in := notes.MovePrefixInput{Namespace: namespace, OldPrefix: oldPrefix, NewPrefix: newPrefix}

	if string(f.Raw("dry_run")) == "true" {
		preview, err := rt.Notes.PreviewMovePrefix(r.Context(), in)
		if err != nil {
			return err
		}
		return httpapi.WriteJSON(w, http.StatusOK, movePrefixPreview{
			DryRun: true, WouldMove: preview.WouldMove, Conflicts: preview.Conflicts,
		})
	}

	overwrite, setErr := overwriteSlugs(f)
	if setErr != nil {
		// Node built the Set after its two scan queries, so a value it
		// cannot iterate failed there, after the prefix checks.
		if _, err := rt.Notes.PreviewMovePrefix(r.Context(), in); err != nil {
			return err
		}
		return setErr
	}
	in.OverwriteSlugs = overwrite
	moved, err := rt.Notes.MovePrefix(r.Context(), in)
	if err != nil {
		return err
	}
	rt.LogAdmin("note_move_prefix", logger.Fields{
		{Key: "namespace", Value: namespace}, {Key: "old_prefix", Value: oldPrefix}, {Key: "new_prefix", Value: newPrefix},
		{Key: "moved", Value: moved.Moved}, {Key: "skipped", Value: moved.Skipped}, {Key: "overwritten", Value: moved.Overwritten},
		{Key: "user_id", Value: User(r).ID},
	})
	return httpapi.WriteJSON(w, http.StatusOK, movePrefixResult{
		Moved: moved.Moved, Skipped: moved.Skipped, Overwritten: moved.Overwritten,
	})
}

const notIterable = " is not iterable (cannot read property Symbol(Symbol.iterator))"

// overwriteSlugs is new Set(overwrite_slugs || []) as the move reads it:
// the string members of an array, or each code point of a string (Set.has
// compares a conflict's slug with them). Any other truthy value is V8's
// TypeError.
func overwriteSlugs(f httpapi.Fields) ([]string, error) {
	if !f.Truthy("overwrite_slugs") {
		return nil, nil
	}
	if elems, ok := f.Array("overwrite_slugs"); ok {
		var out []string
		for _, e := range elems {
			var s string
			if len(e) > 0 && e[0] == '"' && json.Unmarshal(e, &s) == nil {
				out = append(out, s)
			}
		}
		return out, nil
	}
	if s, ok := f.String("overwrite_slugs"); ok {
		var out []string
		for _, c := range s {
			out = append(out, string(c))
		}
		return out, nil
	}
	raw := f.Raw("overwrite_slugs")
	switch raw[0] {
	case '{':
		return nil, errors.New("object" + notIterable)
	case 't':
		return nil, errors.New("boolean true" + notIterable)
	}
	return nil, errors.New("number " + jsutil.NumberString(jsutil.ToNumber(raw)) + notIterable)
}
