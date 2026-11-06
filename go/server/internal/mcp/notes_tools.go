package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/agent"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
)

// The memory, note and instruction tools (Node's TOOL_HANDLERS search
// through save_instructions). Each reads validated arguments, so a field
// is either absent (jsutil.Undefined) or of its declared JSON type.

// str returns args[k] when it is a string.
func str(args *jsutil.Object, k string) (string, bool) {
	s, ok := args.Value(k).(string)
	return s, ok
}

// text is `${args[k]}`: the string, or "undefined" when absent.
func text(args *jsutil.Object, k string) string {
	s, err := jsutil.ValueString(args.Value(k))
	if err != nil {
		return ""
	}
	return s
}

// raw is args[k] as JSON for a service that takes the caller's raw value;
// nil when absent.
func raw(args *jsutil.Object, k string) json.RawMessage {
	v := args.Value(k)
	if v == jsutil.Undefined {
		return nil
	}
	return json.RawMessage(jsutil.Marshal(v))
}

// orNamespace is `args[k] || namespace`.
func orNamespace(args *jsutil.Object, k, namespace string) string {
	if s, ok := str(args, k); ok && s != "" {
		return s
	}
	return namespace
}

// orText is `${value}` for a nullable column: "null" for NULL.
func orText(s *string) string {
	if s == nil {
		return "null"
	}
	return *s
}

func forbidden(format string, args ...any) error {
	return apierr.New(http.StatusForbidden, fmt.Sprintf(format, args...))
}

// noteAccess is the note tools' check: access to the namespace, else a note
// share of the slug for the same operation.
func (s *Server) noteAccess(ctx context.Context, c caller, ns string, args *jsutil.Object, op permissions.Operation) error {
	ok, err := s.Namespaces.HasAccess(ctx, c.actorID, c.agent, ns, op)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	var slug *string
	if v, isString := str(args, "slug"); isString {
		slug = &v
	}
	shared, err := s.Shares.HasNoteAccess(ctx, ns, slug, c.actorID, op)
	if err != nil {
		return err
	}
	if !shared {
		return forbidden(`Actor "%s" does not have %s access to "%s/%s"`, c.agent, op, ns, text(args, "slug"))
	}
	return nil
}

// readable is the namespace filter of a search or grep across namespaces.
func (s *Server) readable(ctx context.Context, c caller) ([]string, bool, error) {
	namespaces, all, err := s.Namespaces.ReadableNamespaces(ctx, c.actorID, c.agent)
	if err != nil {
		return nil, false, err
	}
	return namespaces, !all, nil
}

func (s *Server) search(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	targetNs := orNamespace(args, "namespace", c.namespace)
	if targetNs != "*" {
		if err := permissions.ValidateNamespace(targetNs); err != nil {
			return "", err
		}
		if err := s.Namespaces.RequireAccess(ctx, c.actorID, c.agent, targetNs, permissions.Read); err != nil {
			return "", err
		}
	}
	query, _ := str(args, "query")
	// `args.limit || 5`: the validated limit is a finite number or absent.
	limit := 5.0
	if f, ok := jsutil.AsFloat(args.Value("limit")); ok && f != 0 {
		limit = f
	}
	in := memory.SearchInput{Query: sanitize.Content(query), Namespace: targetNs, Limit: limit, ActorID: c.actorID}
	if targetNs == "*" {
		readable, filter, err := s.readable(ctx, c)
		if err != nil {
			return "", err
		}
		in.Readable, in.FilterReadable = readable, filter
	}
	data, err := s.Memory.Search(ctx, in)
	if err != nil {
		return "", err
	}
	lines := make([]string, len(data.Results))
	for i, r := range data.Results {
		heading := "(no heading)"
		if r.Heading != nil && *r.Heading != "" {
			heading = *r.Heading
		}
		// A NULL similarity is null * 100, which is 0.
		similarity := 0.0
		if r.Similarity != nil {
			similarity = float64(*r.Similarity)
		}
		lines[i] = fmt.Sprintf("[%s] %s — %s (%s%%)\n%s", r.Namespace, r.SourceFile, heading,
			jsutil.ToFixed(similarity*100, 1), r.ChunkText)
	}
	if len(lines) == 0 {
		return "No results found.", nil
	}
	return strings.Join(lines, "\n\n---\n\n"), nil
}

func (s *Server) saveNote(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	targetNs := orNamespace(args, "namespace", c.namespace)
	if err := permissions.ValidateNamespace(targetNs); err != nil {
		return "", err
	}
	if err := s.noteAccess(ctx, c, targetNs, args, permissions.Write); err != nil {
		return "", err
	}
	title, _ := str(args, "title")
	content, _ := str(args, "content")
	slug, _ := str(args, "slug")
	doc, err := s.Notes.Save(ctx, notes.SaveInput{
		Namespace: targetNs,
		Title:     sanitize.Content(title),
		Content:   sanitize.Content(content),
		Slug:      sanitize.Identifier(slug),
		CreatedBy: c.agent,
	})
	if err != nil {
		// The service's message also offers upsert:true, which only the REST
		// route accepts. MCP save_note is insert-only.
		var dup *notes.DuplicateSlugError
		if errors.As(err, &dup) {
			return "", apierr.WithCode(http.StatusConflict, "DUPLICATE_SLUG", fmt.Sprintf(
				`Note already exists at slug "%s" in namespace "%s". save_note only creates new notes; use edit_note to change an existing one.`,
				dup.Slug, dup.Namespace))
		}
		return "", err
	}
	s.refreshActivity(c)
	return "Saved: " + doc.Namespace + "/" + doc.Slug, nil
}

// refreshActivity turns the activity spinner on (or keeps it on), in the
// background, and broadcasts it once the write lands. A failure is dropped.
func (s *Server) refreshActivity(c caller) {
	db.Background(func(ctx context.Context) {
		if _, err := s.DB.Exec(ctx, "UPDATE actors SET active_since = NOW() WHERE id = $1", c.actorID); err != nil {
			return
		}
		s.Events.Broadcast("agent_activity", activityEvent{Agent: c.agent, Active: true})
	})
}

// activityEvent is the agent_activity payload, in Node's key order.
type activityEvent struct {
	Agent  string `json:"agent"`
	Active bool   `json:"active"`
}

func (s *Server) listNotes(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	targetNs := orNamespace(args, "namespace", c.namespace)

	// The special scope "shared-with-me" lists the notes shared with the
	// agent.
	if targetNs == "shared-with-me" {
		docs, err := s.Shares.ListSharedDocuments(ctx, c.actorID)
		if err != nil {
			return "", err
		}
		if len(docs) == 0 {
			return "No shared notes found.", nil
		}
		lines := make([]string, len(docs))
		for i, d := range docs {
			mode := "read-only"
			if d.CanWrite {
				mode = "read/write"
			}
			lines[i] = fmt.Sprintf("[%s] %s — %s (%s)", d.Namespace, d.Slug, orText(d.Title), mode)
		}
		return strings.Join(lines, "\n"), nil
	}

	if err := permissions.ValidateNamespace(targetNs); err != nil {
		return "", err
	}
	if err := s.Namespaces.RequireAccess(ctx, c.actorID, c.agent, targetNs, permissions.Read); err != nil {
		return "", err
	}
	prefix, _ := str(args, "prefix")
	listed, err := s.Notes.ListLive(ctx, notes.ListInput{
		Namespace: targetNs, Limit: raw(args, "limit"), Offset: raw(args, "offset"), Prefix: prefix,
	})
	if err != nil {
		return "", err
	}
	if len(listed) == 0 {
		return "No notes found.", nil
	}

	// Notes under a live share are marked.
	shares, err := s.Shares.ListSharesByOwner(ctx, targetNs)
	if err != nil {
		return "", err
	}
	lines := make([]string, len(listed))
	for i, n := range listed {
		line := fmt.Sprintf("%s — %s (updated %s)", n.Slug, orText(n.Title), s.dateText(n.UpdatedAt))
		for _, sh := range shares {
			if sh.SlugPattern == n.Slug || (strings.HasSuffix(sh.SlugPattern, "/") && strings.HasPrefix(n.Slug, sh.SlugPattern)) {
				line += " [shared]"
				break
			}
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n"), nil
}

// dateText is `${date}` for a timestamptz the notes service returned as
// toISOString text: Date.prototype.toString in the server's zone, or
// "null".
func (s *Server) dateText(iso *string) string {
	if iso == nil {
		return "null"
	}
	t, err := time.Parse(time.RFC3339Nano, *iso)
	if err != nil {
		return *iso
	}
	return jsutil.DateString(t, s.location())
}

func (s *Server) location() *time.Location {
	if s.Location != nil {
		return s.Location
	}
	return time.Local
}

func (s *Server) readNote(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	targetNs := orNamespace(args, "namespace", c.namespace)
	if err := permissions.ValidateNamespace(targetNs); err != nil {
		return "", err
	}
	if err := s.noteAccess(ctx, c, targetNs, args, permissions.Read); err != nil {
		return "", err
	}
	slug, _ := str(args, "slug")
	doc, err := s.Notes.Read(ctx, targetNs, slug)
	if err != nil {
		return "", err
	}
	// Neither offset nor limit returns the content as is.
	page, err := notes.PaginateContent(doc.Content, raw(args, "offset"), raw(args, "limit"))
	if err != nil {
		return "", err
	}
	if page == nil {
		return doc.Content, nil
	}
	return page.Text, nil
}

func (s *Server) deleteNote(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	targetNs := orNamespace(args, "namespace", c.namespace)
	if err := permissions.ValidateNamespace(targetNs); err != nil {
		return "", err
	}
	if err := s.noteAccess(ctx, c, targetNs, args, permissions.Delete); err != nil {
		return "", err
	}
	slug, _ := str(args, "slug")
	if _, err := s.Notes.Delete(ctx, targetNs, slug, nil); err != nil {
		return "", err
	}
	return "Deleted: " + targetNs + "/" + slug, nil
}

func (s *Server) restoreNote(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	targetNs := orNamespace(args, "namespace", c.namespace)
	if err := permissions.ValidateNamespace(targetNs); err != nil {
		return "", err
	}
	if err := s.Namespaces.RequireAccess(ctx, c.actorID, c.agent, targetNs, permissions.Write); err != nil {
		return "", err
	}
	slug, _ := str(args, "slug")
	doc, err := s.Notes.Restore(ctx, targetNs, slug)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`Restored: %s/%s — "%s"`, doc.Namespace, doc.Slug, orText(doc.Title)), nil
}

func (s *Server) editNote(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	targetNs := orNamespace(args, "namespace", c.namespace)
	if err := permissions.ValidateNamespace(targetNs); err != nil {
		return "", err
	}
	if err := s.noteAccess(ctx, c, targetNs, args, permissions.Write); err != nil {
		return "", err
	}
	slug, _ := str(args, "slug")
	oldString, _ := str(args, "old_string")
	newString, _ := str(args, "new_string")
	newString = sanitize.Content(newString)
	replaceAll, _ := args.Value("replace_all").(bool)
	result, err := s.Notes.Edit(ctx, notes.EditInput{
		Namespace:  targetNs,
		Slug:       sanitize.Identifier(slug),
		OldString:  sanitize.Content(oldString),
		NewString:  &newString,
		ReplaceAll: replaceAll,
	})
	if err != nil {
		return "", err
	}
	// An update that touched no row leaves the location undefined.
	location := "undefined/undefined"
	if n := result.Note(); n != nil {
		location = n.Namespace + "/" + n.Slug
	}
	plural := "s"
	if result.Replacements == 1 {
		plural = ""
	}
	return fmt.Sprintf("Edited: %s (%d replacement%s)", location, result.Replacements, plural), nil
}

func (s *Server) moveNote(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	sourceNs := orNamespace(args, "namespace", c.namespace)
	targetNs := orNamespace(args, "new_namespace", sourceNs)
	if err := permissions.ValidateNamespace(sourceNs); err != nil {
		return "", err
	}
	if targetNs != sourceNs {
		if err := permissions.ValidateNamespace(targetNs); err != nil {
			return "", err
		}
	}
	// Write on the source (to remove) and on the target (to create).
	if err := s.Namespaces.RequireAccess(ctx, c.actorID, c.agent, sourceNs, permissions.Write); err != nil {
		return "", err
	}
	if targetNs != sourceNs {
		if err := s.Namespaces.RequireAccess(ctx, c.actorID, c.agent, targetNs, permissions.Write); err != nil {
			return "", err
		}
	}
	slug, _ := str(args, "slug")
	newSlug, _ := str(args, "new_slug")
	newNamespace, _ := str(args, "new_namespace")
	moved, err := s.Notes.Move(ctx, notes.MoveInput{
		Namespace:    sourceNs,
		Slug:         sanitize.Identifier(slug),
		NewSlug:      sanitize.Identifier(newSlug),
		NewNamespace: newNamespace,
	})
	if err != nil {
		return "", err
	}
	doc, ok := moved.(notes.ChangedNote)
	if !ok {
		return "", fmt.Errorf("move returned %T", moved)
	}
	return fmt.Sprintf("Moved: %s/%s → %s/%s", sourceNs, slug, doc.Namespace, doc.Slug), nil
}

func (s *Server) moveFolder(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	targetNs := orNamespace(args, "namespace", c.namespace)
	if err := permissions.ValidateNamespace(targetNs); err != nil {
		return "", err
	}
	// Moving notes rewrites slugs in place: a write on the namespace.
	if err := s.Namespaces.RequireAccess(ctx, c.actorID, c.agent, targetNs, permissions.Write); err != nil {
		return "", err
	}
	from, _ := str(args, "from_prefix")
	to, _ := str(args, "to_prefix")
	in := notes.MovePrefixInput{Namespace: targetNs, OldPrefix: from, NewPrefix: to}

	if dryRun, _ := args.Value("dry_run").(bool); dryRun {
		preview, err := s.Notes.PreviewMovePrefix(ctx, in)
		if err != nil {
			return "", err
		}
		out := fmt.Sprintf("Dry run: %d note(s) would move %s/%s* → %s/%s*", preview.WouldMove, targetNs, from, targetNs, to)
		if len(preview.Conflicts) > 0 {
			list := make([]string, len(preview.Conflicts))
			for i, cf := range preview.Conflicts {
				list[i] = "  - " + cf.Slug
			}
			out += "\nConflicts (destination already exists):\n" + strings.Join(list, "\n")
		} else {
			out += "\nNo conflicts."
		}
		return out, nil
	}

	for _, v := range arrayOf(args, "overwrite_slugs") {
		slug, _ := v.(string)
		in.OverwriteSlugs = append(in.OverwriteSlugs, slug)
	}

	// With nothing to overwrite, conflicts refuse the move rather than skip
	// the colliding notes (a half-moved folder), as move_note and save_note
	// refuse a collision.
	if len(in.OverwriteSlugs) == 0 {
		preview, err := s.Notes.PreviewMovePrefix(ctx, in)
		if err != nil {
			return "", err
		}
		if len(preview.Conflicts) > 0 {
			slugs := make([]string, len(preview.Conflicts))
			for i, cf := range preview.Conflicts {
				slugs[i] = cf.Slug
			}
			return "", apierr.New(http.StatusConflict, fmt.Sprintf(
				"Refusing to move: %d destination slug(s) already exist: %s. Re-run with dry_run to inspect, pass overwrite_slugs to replace them, or choose a different to_prefix.",
				len(preview.Conflicts), strings.Join(slugs, ", ")))
		}
	}

	result, err := s.Notes.MovePrefix(ctx, in)
	if err != nil {
		return "", err
	}
	summary := fmt.Sprintf("Moved %d note(s): %s/%s* → %s/%s*", result.Moved, targetNs, from, targetNs, to)
	if result.Overwritten > 0 {
		summary += fmt.Sprintf("; overwrote %d", result.Overwritten)
	}
	if result.Skipped > 0 {
		summary += fmt.Sprintf("; skipped %d (unresolved conflict)", result.Skipped)
	}
	return summary, nil
}

// arrayOf returns args[k] when it is an array.
func arrayOf(args *jsutil.Object, k string) []any {
	a, _ := args.Value(k).([]any)
	return a
}

func (s *Server) grep(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	targetNs := orNamespace(args, "namespace", c.namespace)
	if targetNs != "*" {
		if err := permissions.ValidateNamespace(targetNs); err != nil {
			return "", err
		}
		if err := s.Namespaces.RequireAccess(ctx, c.actorID, c.agent, targetNs, permissions.Read); err != nil {
			return "", err
		}
	}
	pattern, _ := str(args, "pattern")
	regex, _ := args.Value("regex").(bool)
	in := notes.GrepInput{
		Pattern:       sanitize.Content(pattern),
		Namespace:     targetNs,
		Limit:         raw(args, "limit"),
		ContextBefore: raw(args, "context_before"),
		ContextAfter:  raw(args, "context_after"),
		Context:       raw(args, "context"),
		Regex:         regex,
	}
	if targetNs == "*" {
		readable, filter, err := s.readable(ctx, c)
		if err != nil {
			return "", err
		}
		in.Readable, in.FilterReadable = readable, filter
	}
	results, err := s.Notes.Grep(ctx, in)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return `No notes matching "` + pattern + `".`, nil
	}
	// Like grep's output: a header per note, then its lines, with "--"
	// before a block that does not follow the previous line.
	sections := make([]string, len(results))
	for i, doc := range results {
		plural := "es"
		if doc.MatchCount == 1 {
			plural = ""
		}
		var b strings.Builder
		fmt.Fprintf(&b, "[%s] %s — %s (%d match%s)", doc.Namespace, doc.Slug, orText(doc.Title), doc.MatchCount, plural)
		for _, m := range doc.Matches {
			if m.NewBlock {
				b.WriteString("\n--")
			}
			mark := " "
			if m.IsMatch {
				mark = ">"
			}
			fmt.Fprintf(&b, "\n%s %4d: %s", mark, m.LineNumber, m.Line)
		}
		sections[i] = b.String()
	}
	return strings.Join(sections, "\n\n---\n\n"), nil
}

func (s *Server) readInstructions(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	config, found, err := agent.LoadInstructions(ctx, s.DB, c.actorID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", errors.New("Agent not found")
	}
	var parts []string
	globalBootstrap, err := s.Config.Get("global_bootstrap")
	if err != nil {
		return "", err
	}
	if globalBootstrap != "" {
		parts = append(parts, globalBootstrap)
	}
	if config.Startup != nil && *config.Startup != "" {
		parts = append(parts, *config.Startup)
	}
	if config.Dreams() {
		dreamBootstrap, err := s.Config.Get("dream_bootstrap")
		if err != nil {
			return "", err
		}
		if dreamBootstrap != "" {
			parts = append(parts, dreamBootstrap)
		}
	}
	// The living soul document dream processing maintains; a note that does
	// not exist (or any read failure) is skipped.
	if soul, err := s.Notes.Read(ctx, c.namespace, "context/soul"); err == nil && soul.Content != "" {
		parts = append(parts, soul.Content)
	}
	if len(parts) == 0 {
		return "(no instructions set)", nil
	}
	return strings.Join(parts, "\n\n"), nil
}

func (s *Server) saveInstructions(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	content, _ := str(args, "content")
	if content == "" {
		return "", apierr.New(http.StatusBadRequest, "Required field: content")
	}
	content = sanitize.Content(content)
	if err := agent.SaveInstructions(ctx, s.DB, c.actorID, &content); err != nil {
		return "", err
	}
	return fmt.Sprintf("Instructions saved (%d characters)", jsutil.UTF16Len(content)), nil
}
