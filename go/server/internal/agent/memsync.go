package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
)

// /agent/memory/sync is the memory-sync client's one round trip. Its memory
// half compares the client's flat memory directory (filename, content,
// mtime) with the notes under instructions/memory/ in the agent's own
// namespace: a newer local file is saved, a newer or remote-only note is
// returned for the client to write, and in prune mode (LLM-565) a remote-only
// note the client retired is soft-deleted. Its conversations half reports the
// retention window, which of the client's sessions the server lacks or holds
// a smaller copy of, and saves uploaded conversation logs.

// memoryPrefix is the slug folder memory files sync to. The server fixes it
// and the namespace; the client names only flat files.
const memoryPrefix = "instructions/memory/"

// maxScanAgeMs is the oldest local scan a prune run may rely on.
const maxScanAgeMs = 60 * 60 * 1000

// maxSessionIDs is how many sessions one conversation diff may name.
const maxSessionIDs = 1000

// syncTimeout bounds a sync, which runs on after the client hangs up: Node's
// saves and deletes went on after a disconnect, and stopping halfway would
// leave a run that saved some files and not others.
const syncTimeout = 2 * time.Minute

// prototypeNames are the members every JavaScript object inherits from
// Object.prototype (besides __proto__). Node kept the remote notes in a
// plain object, so remoteByFilename[name] was truthy for a local file with
// one of these names, and the file compared as unchanged.
var prototypeNames = map[string]bool{
	"constructor": true, "hasOwnProperty": true, "isPrototypeOf": true, "propertyIsEnumerable": true,
	"toString": true, "toLocaleString": true, "valueOf": true,
	"__defineGetter__": true, "__defineSetter__": true, "__lookupGetter__": true, "__lookupSetter__": true,
}

// isSafeFilename reports whether name is a flat basename: a non-empty
// string with no slash or backslash that does not start with a dot.
func isSafeFilename(name any) bool {
	s, ok := name.(string)
	if !ok || s == "" {
		return false
	}
	if strings.ContainsAny(s, `/\`) {
		return false
	}
	return !strings.HasPrefix(s, ".")
}

// resolvePruneCutoff is the instant a prune run compares remote notes with:
// the moment the client listed its directory, on this server's clock. The
// client sends how long ago it scanned, the difference of two readings of
// its own clock, so an offset between the two clocks does not matter. ok is
// false when the age is not a finite number from 0 to maxScanAgeMs.
func resolvePruneCutoff(scanAge any, nowMs float64) (float64, bool) {
	age, isNumber := jsutil.AsFloat(scanAge)
	if !isNumber || math.IsNaN(age) || math.IsInf(age, 0) {
		return 0, false
	}
	if age < 0 || age > maxScanAgeMs {
		return 0, false
	}
	return nowMs - age, true
}

// remoteOnlyAction decides what happens to a note that has no local file:
// "pull" outside prune mode; in prune mode "prune", unless the note was
// updated after the cutoff (a concurrent session wrote it after the scan) or
// its time is missing or not a date, which keeps it ("pull").
func remoteOnlyAction(prune bool, remoteUpdatedAt *string, cutoff float64, loc *time.Location) string {
	if !prune || remoteUpdatedAt == nil || *remoteUpdatedAt == "" {
		return "pull"
	}
	updated, ok := jsutil.ParseDate(*remoteUpdatedAt, loc)
	if !ok || updated > cutoff {
		return "pull"
	}
	return "prune"
}

// sessionItem is one of the client's conversation sessions.
type sessionItem struct {
	id       string
	fileSize float64
}

// sessionRow is the server's largest recorded file_size for a session,
// across live, soft-deleted and tombstoned notes.
type sessionRow struct {
	sessionID string
	fileSize  float64
}

// classifySessions decides which of the client's sessions it should upload.
// A session with any row at all is present: retention soft-deletes a
// conversation and later empties it to a tombstone, and reporting it missing
// would have the client upload it again as a new row (LLM-642). A present
// session is stale only when the local file grew past every size the server
// recorded; legacy clients send no sizes and are never stale.
func classifySessions(items []sessionItem, rows []sessionRow, hasNewFormat bool) (missing, stale []string) {
	existing := map[string]float64{}
	for _, row := range rows {
		existing[jsutil.ToLower(row.sessionID)] = row.fileSize
	}
	missing, stale = []string{}, []string{}
	for _, item := range items {
		size, ok := existing[jsutil.ToLower(item.id)]
		if !ok {
			missing = append(missing, item.id)
		} else if hasNewFormat && item.fileSize > size {
			stale = append(stale, item.id)
		}
	}
	return missing, stale
}

// jsSpace is JavaScript's \s and jsDot its "." (anything but a line
// terminator); Go's \s and . are narrower.
const (
	jsSpace = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`
	jsDot   = `[^\n\r\x{2028}\x{2029}]`
)

var (
	// /^---\s*\n[\s\S]*?name:\s*(.+)\n[\s\S]*?---/. Go's leftmost-first
	// matching picks the same submatch as a backtracking engine.
	frontmatterName = regexp.MustCompile(`\A---` + jsSpace + `*\n(?s:.)*?name:` + jsSpace + `*(` + jsDot + `+)\n(?s:.)*?---`)
	// /^#\s+(.+)/m: with the m flag ^ also matches after \r, U+2028 and
	// U+2029.
	firstHeading = regexp.MustCompile(`(?:\A|[\n\r\x{2028}\x{2029}])#` + jsSpace + `+(` + jsDot + `+)`)
)

// extractTitle is the note title for a pushed file: the frontmatter name
// field, else the first H1 heading, else the filename without ".md" and with
// "_" and "-" as spaces.
func extractTitle(content, filename string) string {
	if content != "" {
		if m := frontmatterName.FindStringSubmatch(content); m != nil {
			return jsutil.Trim(m[1])
		}
		if m := firstHeading.FindStringSubmatch(content); m != nil {
			return jsutil.Trim(m[1])
		}
	}
	name := strings.TrimSuffix(filename, ".md")
	return strings.NewReplacer("_", " ", "-", " ").Replace(name)
}

// member is v[key] for a parsed JSON value: undefined for anything but an
// object's own member (the route reads no inherited names).
func member(v any, key string) any {
	if o, ok := v.(*jsutil.Object); ok {
		return o.Value(key)
	}
	return jsutil.Undefined
}

// isObject is `typeof v === 'object'` for a non-null JSON value.
func isObject(v any) bool {
	switch v.(type) {
	case *jsutil.Object, []any:
		return true
	}
	return false
}

func (rt *Routes) location() *time.Location {
	if rt.Location != nil {
		return rt.Location
	}
	return time.Local
}

// memorySync is POST /agent/memory/sync. The response has a memory member
// when the body's memory field is truthy and a conversations member when
// its conversations field is neither undefined nor null.
func (rt *Routes) memorySync(w http.ResponseWriter, r *http.Request) error {
	agent, ok := requireAgent(w, r)
	if !ok {
		return nil
	}
	raw := httpapi.StateOf(r).Body
	if raw == nil {
		return noBodyRead("memory")
	}
	body, err := jsutil.Parse(raw)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), syncTimeout)
	defer cancel()

	response := jsutil.NewObject()
	if memory := member(body, "memory"); jsutil.Truthy(memory) {
		actions, refusal, err := rt.syncMemory(ctx, httpapi.StateOf(r).ActorID, agent, memory)
		if err != nil {
			return err
		}
		if refusal != "" {
			return httpapi.BadRequest(w, refusal)
		}
		result := jsutil.NewObject()
		result.Set("actions", actions)
		response.Set("memory", result)
	}
	if conversations := member(body, "conversations"); conversations != jsutil.Undefined && conversations != nil {
		response.Set("conversations", rt.syncConversations(ctx, agent, conversations))
	}
	return writeObject(w, response)
}

// localFile is one entry of memory.files.
type localFile struct {
	content string
	mtimeMs float64
}

// syncMemory runs the memory half. refusal is a 400 message Node wrote
// directly, before any query.
func (rt *Routes) syncMemory(ctx context.Context, actorID int, agent string, memory any) (actions []any, refusal string, err error) {
	files, isArray := member(memory, "files").([]any)
	if !isArray {
		return nil, "memory.files must be an array of {filename, content, mtime}", nil
	}

	loc := rt.location()
	locals := jsutil.NewObject()
	for i, file := range files {
		index := strconv.Itoa(i)
		if !isObject(file) {
			return nil, "memory.files[" + index + "] is not an object", nil
		}
		filename := member(file, "filename")
		if !isSafeFilename(filename) {
			return nil, "memory.files[" + index + "].filename is invalid: must be a safe basename (no slashes, no .., no leading dot)", nil
		}
		content, isString := member(file, "content").(string)
		if !isString {
			return nil, "memory.files[" + index + "].content must be a string", nil
		}
		mtime := member(file, "mtime")
		if !jsutil.Truthy(mtime) {
			return nil, "memory.files[" + index + "].mtime must be a valid ISO8601 timestamp", nil
		}
		ms, valid, err := jsutil.DateValue(mtime, loc)
		if err != nil {
			return nil, "", err
		}
		if !valid {
			return nil, "memory.files[" + index + "].mtime must be a valid ISO8601 timestamp", nil
		}
		// A file named __proto__ set the prototype of Node's lookup object
		// and so never showed up in Object.keys; it is skipped.
		if name := filename.(string); name != "__proto__" {
			locals.Set(name, &localFile{content: content, mtimeMs: ms})
		}
	}

	// Prune mode cannot tell a retired note from one a concurrent session
	// just created without the scan time, so it is refused without one.
	prune := member(memory, "prune") == true
	var cutoff float64
	if prune {
		var ok bool
		cutoff, ok = resolvePruneCutoff(member(memory, "scan_age_ms"), float64(rt.now().UnixMilli()))
		if !ok {
			return nil, "memory.scan_age_ms must be a non-negative number of milliseconds no greater than " +
				strconv.Itoa(maxScanAgeMs) + " when memory.prune is true", nil
		}
	}

	namespace := agent
	if err := rt.Namespaces.RequireAccess(ctx, actorID, agent, namespace, permissions.Read); err != nil {
		return nil, "", err
	}
	listed, err := rt.Notes.ListLive(ctx, notes.ListInput{
		Namespace: namespace, Limit: []byte("500"), Offset: []byte("0"), Prefix: memoryPrefix,
	})
	if err != nil {
		return nil, "", err
	}

	// The remote notes by filename: direct children of the prefix only (the
	// LIKE listing is case-insensitive, this check is not). A name with no
	// dot gets ".md", so "foo" and "foo.md" both map to foo.md; when two
	// notes map to one name, the later updated_at (to the millisecond) wins.
	remotes := jsutil.NewObject()
	for i := range listed {
		note := &listed[i]
		if !strings.HasPrefix(note.Slug, memoryPrefix) {
			continue
		}
		derived := note.Slug[len(memoryPrefix):]
		if !isSafeFilename(derived) {
			continue
		}
		if !strings.Contains(derived, ".") {
			derived += ".md"
		}
		if existing, ok := remotes.Get(derived); ok {
			if rt.noteTime(note.UpdatedAt) > rt.noteTime(existing.(*notes.ListedNote).UpdatedAt) {
				remotes.Set(derived, note)
			}
			continue
		}
		remotes.Set(derived, note)
	}

	// Remote names first, then local-only names, in Object.keys order.
	names := remotes.Keys()
	for _, name := range locals.Keys() {
		if !remotes.Has(name) {
			names = append(names, name)
		}
	}

	var hasWrite, hasDelete bool
	access := func(op permissions.Operation, granted *bool) error {
		if *granted {
			return nil
		}
		if err := rt.Namespaces.RequireAccess(ctx, actorID, agent, namespace, op); err != nil {
			return err
		}
		*granted = true
		return nil
	}

	actions = []any{}
	for _, filename := range names {
		remoteValue, hasRemote := remotes.Get(filename)
		localValue, hasLocal := locals.Get(filename)

		if !hasRemote && prototypeNames[filename] {
			actions = append(actions, action(filename, "unchanged"))
			continue
		}

		switch {
		case hasRemote && !hasLocal:
			remote := remoteValue.(*notes.ListedNote)
			if remoteOnlyAction(prune, remote.UpdatedAt, cutoff, loc) == "prune" {
				if err := access(permissions.Delete, &hasDelete); err != nil {
					return nil, "", err
				}
				// Conditional on the listed version, which covers a write
				// between the listing and this delete. A 409 (rewritten
				// since) pulls instead; a 404 (deleted by someone else) is
				// the outcome wanted.
				_, err := rt.Notes.Delete(ctx, namespace, remote.Slug, remote.UpdatedAtExact)
				if err == nil || statusOf(err) == http.StatusNotFound {
					a := action(filename, "prune")
					a.Set("remote_updated_at", jsValue(remote.UpdatedAt))
					actions = append(actions, a)
					continue
				}
				if statusOf(err) != http.StatusConflict {
					return nil, "", err
				}
			}
			// The note can vanish between the listing and this read (a
			// concurrent delete, or one that raced the 409 above); the next
			// run reconciles it, so it gets no action.
			note, err := rt.Notes.Read(ctx, namespace, remote.Slug)
			if statusOf(err) == http.StatusNotFound {
				continue
			}
			if err != nil {
				return nil, "", err
			}
			actions = append(actions, pullAction(filename, note))

		case hasLocal && !hasRemote:
			local := localValue.(*localFile)
			if err := access(permissions.Write, &hasWrite); err != nil {
				return nil, "", err
			}
			doc, err := rt.Notes.Save(ctx, notes.SaveInput{
				Namespace: namespace, Title: extractTitle(local.content, filename), Content: local.content,
				Slug: memoryPrefix + filename, CreatedBy: agent,
			})
			if err != nil {
				return nil, "", err
			}
			actions = append(actions, pushAction(filename, doc))

		default:
			remote, local := remoteValue.(*notes.ListedNote), localValue.(*localFile)
			remoteTime := rt.noteTime(remote.UpdatedAt)
			switch {
			case remoteTime > local.mtimeMs:
				note, err := rt.Notes.Read(ctx, namespace, remote.Slug)
				if err != nil {
					return nil, "", err
				}
				actions = append(actions, pullAction(filename, note))
			case local.mtimeMs > remoteTime:
				if err := access(permissions.Write, &hasWrite); err != nil {
					return nil, "", err
				}
				doc, err := rt.Notes.Save(ctx, notes.SaveInput{
					Namespace: namespace, Title: extractTitle(local.content, filename), Content: local.content,
					Slug: memoryPrefix + filename, CreatedBy: agent, Upsert: true,
				})
				if err != nil {
					return nil, "", err
				}
				actions = append(actions, pushAction(filename, doc))
			default:
				actions = append(actions, action(filename, "unchanged"))
			}
		}
	}
	return actions, "", nil
}

// noteTime is new Date(updated_at).getTime() for a listed or read time;
// null is the epoch, as new Date(null) is.
func (rt *Routes) noteTime(t notes.Time) float64 {
	if t == nil {
		return 0
	}
	ms, ok := jsutil.ParseDate(*t, rt.location())
	if !ok {
		return math.NaN()
	}
	return ms
}

// statusOf is the statusCode a Node service error carried; 0 for none.
func statusOf(err error) int {
	var e *apierr.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func action(filename, name string) *jsutil.Object {
	a := jsutil.NewObject()
	a.Set("filename", filename)
	a.Set("action", name)
	return a
}

func pullAction(filename string, note notes.Note) *jsutil.Object {
	a := action(filename, "pull")
	a.Set("content", note.Content)
	a.Set("title", jsValue(note.Title))
	a.Set("remote_updated_at", jsValue(note.UpdatedAt))
	return a
}

func pushAction(filename string, doc notes.SavedNote) *jsutil.Object {
	a := action(filename, "push")
	a.Set("remote_updated_at", jsValue(doc.UpdatedAt))
	return a
}

// sessionIDPattern is Node's UUID check (/^...$/i; the i flag matches ASCII
// letters only here).
var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// syncConversations runs the conversations half. Any error ends it: the
// members set so far stay, error says what failed, and the memory half's
// result is kept.
func (rt *Routes) syncConversations(ctx context.Context, agent string, conversations any) *jsutil.Object {
	result := jsutil.NewObject()
	if err := rt.conversations(ctx, agent, conversations, result); err != nil {
		message := httpapi.ErrorMessage(err)
		result.Set("error", "Conversation sync failed: "+message)
		rt.Log.LogError("agent", "conversation-sync", logger.ErrorOpts{
			Agent: agent, Message: message, Detail: fmt.Sprintf("%+v", err),
		})
	}
	return result
}

// normalizeFileSize is Number(v) when that is an integer of at least 0,
// else 0. Number() of an object with its own toString member throws.
func normalizeFileSize(v any) (float64, error) {
	n, err := jsutil.ValueNumber(v)
	if err != nil {
		return 0, err
	}
	if math.IsInf(n, 0) || math.IsNaN(n) || n != math.Trunc(n) || n < 0 {
		return 0, nil
	}
	return n, nil
}

func (rt *Routes) conversations(ctx context.Context, agent string, conversations any, result *jsutil.Object) error {
	value, err := rt.Config.Get("conversation_retention_days")
	if err != nil {
		return err
	}
	retention, ok := jsutil.ParseInt(value)
	if !ok || retention == 0 {
		retention = 30
	}
	result.Set("retention_days", retention)

	// Two formats: sessions [{id, file_size}] (with stale detection) and the
	// legacy session_ids [id] (without). sessions wins when it is a
	// non-empty array.
	sessions, sessionsIsArray := member(conversations, "sessions").([]any)
	ids, idsIsArray := member(conversations, "session_ids").([]any)
	hasNewFormat := sessionsIsArray && len(sessions) > 0
	hasLegacyFormat := idsIsArray && len(ids) > 0

	if hasNewFormat || hasLegacyFormat {
		type rawItem struct {
			id       any
			fileSize float64
		}
		var items []rawItem
		if hasNewFormat {
			for _, s := range sessions {
				if !jsutil.Truthy(s) || !isObject(s) {
					continue
				}
				size, err := normalizeFileSize(member(s, "file_size"))
				if err != nil {
					return err
				}
				items = append(items, rawItem{id: member(s, "id"), fileSize: size})
			}
		} else {
			for _, id := range ids {
				items = append(items, rawItem{id: id})
			}
		}

		if len(items) > maxSessionIDs {
			result.Set("error", "Too many session IDs (max "+strconv.Itoa(maxSessionIDs)+")")
		} else {
			seen := map[string]bool{}
			valid := []sessionItem{}
			validIDs := []string{}
			for _, item := range items {
				id, isString := item.id.(string)
				if !isString || !sessionIDPattern.MatchString(id) {
					continue
				}
				lower := strings.ToLower(id)
				if seen[lower] {
					continue
				}
				seen[lower] = true
				valid = append(valid, sessionItem{id: lower, fileSize: item.fileSize})
				validIDs = append(validIDs, lower)
			}

			// Not filtered on deleted_at: a session retention retired
			// (soft-deleted or emptied to a tombstone) must still read as
			// present (LLM-642, classifySessions). MAX covers several rows
			// for one session; the regex keeps a non-numeric stored value
			// from failing the cast.
			rows, err := rt.DB.Query(ctx, `
                        SELECT metadata->>'session_id' AS session_id,
                               MAX(
                                   CASE
                                       WHEN metadata->>'file_size' ~ '^[0-9]+$'
                                       THEN (metadata->>'file_size')::bigint
                                       ELSE 0
                                   END
                               ) AS file_size
                        FROM documents
                        WHERE namespace = $1 AND kind = 'conversation'
                          AND metadata->>'session_id' = ANY($2)
                        GROUP BY metadata->>'session_id'
                    `, agent, validIDs)
			if err != nil {
				return err
			}
			var existing []sessionRow
			for rows.Next() {
				var sessionID string
				var size *int64
				if err := rows.Scan(&sessionID, &size); err != nil {
					rows.Close()
					return err
				}
				// parseInt(row.file_size) || 0 of the bigint text.
				row := sessionRow{sessionID: sessionID}
				if size != nil {
					row.fileSize = float64(*size)
				}
				existing = append(existing, row)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}

			missing, stale := classifySessions(valid, existing, hasNewFormat)
			result.Set("missing", stringValues(missing))
			if len(stale) > 0 {
				result.Set("stale", stringValues(stale))
			}
		}
	}

	uploads, uploadsIsArray := member(conversations, "uploads").([]any)
	if !uploadsIsArray || len(uploads) == 0 {
		return nil
	}
	uploaded := 0
	uploadErrors := []any{}
	for _, upload := range uploads {
		if upload == nil {
			// Node read upload.content inside its try, and then
			// upload.session_id in the catch, which threw out of the loop.
			return errors.New("Cannot read properties of null (reading 'session_id')")
		}
		sessionID := member(upload, "session_id")
		reported := sessionID
		if !jsutil.Truthy(reported) {
			reported = "unknown"
		}
		uploadError := func(message string) {
			e := jsutil.NewObject()
			e.Set("session_id", reported)
			e.Set("error", message)
			uploadErrors = append(uploadErrors, e)
		}
		content, date := member(upload, "content"), member(upload, "date")
		if !jsutil.Truthy(content) || !jsutil.Truthy(sessionID) || !jsutil.Truthy(date) {
			uploadError("Missing required fields: content, session_id, date")
			continue
		}
		if err := rt.saveConversation(ctx, agent, content, sessionID, date, member(upload, "metadata")); err != nil {
			message := httpapi.ErrorMessage(err)
			uploadError(message)
			// The route answers 200 with the error in the body, so the
			// route wrapper never logs it; this does.
			rt.Log.LogError("agent", "conversation-upload", logger.ErrorOpts{
				Agent: agent, Message: message, Detail: fmt.Sprintf("%+v", err),
			})
			continue
		}
		uploaded++
	}
	result.Set("uploaded", uploaded)
	if len(uploadErrors) > 0 {
		result.Set("upload_errors", uploadErrors)
	}
	return nil
}

// saveConversation saves one upload as conversations/<date>-<session_id>.
// The slug is the server's and a later sync resends a grown log, so it is
// always an upsert. Fields that are not strings are refused (go-server
// note, Known differences).
func (rt *Routes) saveConversation(ctx context.Context, agent string, contentValue, sessionValue, dateValue, metadata any) error {
	date, isString := dateValue.(string)
	if !isString {
		return httpapi.NotString("date")
	}
	sessionID, isString := sessionValue.(string)
	if !isString {
		return httpapi.NotString("session_id")
	}
	content, isString := contentValue.(string)
	if !isString {
		return httpapi.NotString("content")
	}
	var meta []byte
	if jsutil.Truthy(metadata) {
		meta = []byte(jsutil.Marshal(metadata))
	}
	_, err := rt.Notes.Save(ctx, notes.SaveInput{
		Namespace: agent,
		Title:     "Conversation " + date + " (" + sliceUTF16(sessionID, 8) + ")",
		Content:   content,
		Slug:      "conversations/" + date + "-" + sessionID,
		CreatedBy: agent,
		Metadata:  meta,
		Upsert:    true,
	})
	return err
}

// sliceUTF16 is s.slice(0, n) as stored: a surrogate pair cut in half
// leaves a lone surrogate, which reaches Postgres as U+FFFD.
func sliceUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if units+w > n {
			if units < n {
				return s[:i] + "�"
			}
			return s[:i]
		}
		units += w
	}
	return s
}

func stringValues(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}
