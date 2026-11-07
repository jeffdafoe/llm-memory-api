package admin

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
)

// enrichPause is the pause after each note's enrichment call in a reindex,
// so the provider is not hammered.
const enrichPause = 200 * time.Millisecond

// reindexState is Node's in-memory reindexState: it survives page reloads
// and is lost on restart. Nil (no reindex since start, or cleared) answers
// {running: false}.
type reindexState struct {
	mu    sync.Mutex
	state *reindexProgress
}

type reindexError struct {
	Namespace string `json:"namespace"`
	Slug      string `json:"slug"`
	Error     string `json:"error"`
}

// reindexResult is the finished result, or {error} when the reindex itself
// failed.
type reindexResult struct {
	ChunksDeleted int64          `json:"chunks_deleted"`
	DocsIndexed   int            `json:"docs_indexed"`
	ChunksCreated int            `json:"chunks_created"`
	Errors        []reindexError `json:"errors"`
}

type reindexFailure struct {
	Error string `json:"error"`
}

type reindexProgress struct {
	running       bool
	current       int
	total         int
	chunksCreated int
	errors        []reindexError
	result        any // nil, reindexResult or reindexFailure
}

type reindexStatus struct {
	Running       bool `json:"running"`
	Current       int  `json:"current"`
	Total         int  `json:"total"`
	ChunksCreated int  `json:"chunks_created"`
	ErrorsCount   int  `json:"errors_count"`
	Result        any  `json:"result"`
}

// notesReindex is POST /admin/notes/reindex: it answers at once and then,
// in the background, deletes every chunk and indexes every live note again,
// one at a time, oldest namespace and slug first.
func (rt *ContentRoutes) notesReindex(w http.ResponseWriter, r *http.Request) error {
	rt.reindex.mu.Lock()
	if rt.reindex.state != nil && rt.reindex.state.running {
		rt.reindex.mu.Unlock()
		httpapi.WriteError(w, http.StatusConflict, "CONFLICT", "Reindex already in progress")
		return nil
	}
	progress := &reindexProgress{running: true, errors: []reindexError{}}
	rt.reindex.state = progress
	rt.reindex.mu.Unlock()

	userID := User(r).ID
	// The run starts before the answer is written, so a failed write (the
	// client gone) cannot leave a state that says running with no run
	// behind it. Node's run had no bound; each step waits on the embeddings
	// API.
	if !db.BackgroundUnbounded(func(ctx context.Context) {
		rt.runReindex(ctx, progress, userID)
	}) {
		// Shutting down: nothing will run, so nothing is running.
		rt.reindex.mu.Lock()
		progress.running = false
		progress.result = reindexFailure{Error: "server is shutting down"}
		rt.reindex.mu.Unlock()
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"started": true})
}

func (rt *ContentRoutes) runReindex(ctx context.Context, p *reindexProgress, userID int) {
	fail := func(err error) {
		rt.Log.Errorf("Admin notes reindex error: %s", err)
		rt.reindex.mu.Lock()
		p.running = false
		p.result = reindexFailure{Error: err.Error()}
		rt.reindex.mu.Unlock()
	}
	tag, err := rt.DB.Exec(ctx, "DELETE FROM memory_chunks")
	if err != nil {
		fail(err)
		return
	}
	chunksDeleted := tag.RowsAffected()

	type doc struct {
		namespace, slug, content string
		title                    *string
		metadata                 []byte
	}
	rows, err := rt.DB.Query(ctx, "SELECT namespace, slug, content, title, metadata FROM documents WHERE deleted_at IS NULL ORDER BY namespace, slug")
	if err != nil {
		fail(err)
		return
	}
	var docs []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.namespace, &d.slug, &d.content, &d.title, &d.metadata); err != nil {
			rows.Close()
			fail(err)
			return
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		fail(err)
		return
	}
	rt.reindex.mu.Lock()
	p.total = len(docs)
	rt.reindex.mu.Unlock()

	// config.get threw on a missing key, outside the per-note try: the
	// whole reindex fails, its chunks already deleted.
	enabled, err := rt.Config.Get("note_enrichment_enabled")
	if err != nil {
		fail(err)
		return
	}
	enrich := enabled == "true"
	for _, d := range docs {
		res, err := rt.Memory.Ingest(ctx, d.namespace, d.slug, d.content)
		rt.reindex.mu.Lock()
		if err != nil {
			p.errors = append(p.errors, reindexError{Namespace: d.namespace, Slug: d.slug, Error: err.Error()})
		} else {
			p.chunksCreated += res.ChunksCreated
		}
		rt.reindex.mu.Unlock()
		if err == nil && enrich && rt.Notes.Enricher != nil {
			metadata, perr := notes.EnrichMetadata(d.metadata)
			if perr != nil {
				// JSON.parse threw inside the per-note try: the note counts
				// as an error, its chunks stay.
				rt.reindex.mu.Lock()
				p.errors = append(p.errors, reindexError{Namespace: d.namespace, Slug: d.slug, Error: perr.Error()})
				rt.reindex.mu.Unlock()
			} else {
				// Node concatenated a NULL title into the prompt as "null".
				title := "null"
				if d.title != nil {
					title = *d.title
				}
				// Fire and forget, its failure dropped, as Node's .catch(() => {}).
				// Unbounded: the provider call has its own timeout.
				n := d
				db.BackgroundUnbounded(func(ctx context.Context) {
					rt.Notes.Enricher.Enrich(ctx, n.namespace, n.slug, title, n.content, metadata)
				})
				select {
				case <-ctx.Done():
				case <-time.After(enrichPause):
				}
			}
		}
		rt.reindex.mu.Lock()
		p.current++
		rt.reindex.mu.Unlock()
	}

	rt.reindex.mu.Lock()
	result := reindexResult{
		ChunksDeleted: chunksDeleted,
		DocsIndexed:   p.current - len(p.errors),
		ChunksCreated: p.chunksCreated,
		Errors:        p.errors,
	}
	p.running = false
	p.result = result
	rt.reindex.mu.Unlock()
	rt.LogAdmin("notes_reindex", logger.Fields{
		{Key: "user_id", Value: userID},
		{Key: "chunks_deleted", Value: result.ChunksDeleted},
		{Key: "docs_indexed", Value: result.DocsIndexed},
		{Key: "chunks_created", Value: result.ChunksCreated},
		{Key: "errors", Value: len(result.Errors)},
	})
}

// notesReindexStatus is POST /admin/notes/reindex-status: the progress of
// the current or last reindex.
func (rt *ContentRoutes) notesReindexStatus(w http.ResponseWriter, r *http.Request) {
	rt.reindex.mu.Lock()
	p := rt.reindex.state
	if p == nil {
		rt.reindex.mu.Unlock()
		httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"running": false})
		return
	}
	st := reindexStatus{
		Running: p.running, Current: p.current, Total: p.total, ChunksCreated: p.chunksCreated,
		ErrorsCount: len(p.errors), Result: p.result,
	}
	rt.reindex.mu.Unlock()
	httpapi.WriteJSON(w, http.StatusOK, st)
}

// notesReindexClear is POST /admin/notes/reindex-clear: forget a finished
// reindex's result; a running one is kept.
func (rt *ContentRoutes) notesReindexClear(w http.ResponseWriter, r *http.Request) {
	rt.reindex.mu.Lock()
	if rt.reindex.state != nil && !rt.reindex.state.running {
		rt.reindex.state = nil
	}
	rt.reindex.mu.Unlock()
	httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
