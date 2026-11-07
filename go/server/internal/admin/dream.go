package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/dream"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/peopleslug"
)

// DreamRoutes is the dream admin route: /admin/dream/consolidate-people.
type DreamRoutes struct {
	*Common
	// Dream runs the people writer and finds the people agents. It needs
	// DB, Log, Notes, Agents and Trust.
	Dream *dream.Job
	Notes *notes.Service
	// Now is the clock for today's date; nil is time.Now.
	Now func() time.Time
}

// Register adds the route to the admin mux.
func (rt *DreamRoutes) Register(mux *httpapi.RawMux) {
	rt.HandleUnbounded(mux, "/admin/dream/consolidate-people", "agents", "write", "dream-consolidate-people", rt.consolidatePeople)
}

// peopleNotesPage is listAllPeopleNotes's page size.
const peopleNotesPage = 1000

const peoplePrefix = "context/people/"

type consolidateAgentError struct {
	Agent string `json:"agent"`
	Error string `json:"error"`
}

type consolidatePersonError struct {
	Agent  string `json:"agent"`
	Person string `json:"person"`
	Slug   string `json:"slug"`
	Error  string `json:"error"`
}

type consolidateResult struct {
	Agent         string  `json:"agent"`
	Person        string  `json:"person"`
	Slug          string  `json:"slug"`
	Changed       bool    `json:"changed"`
	Written       bool    `json:"written"`
	BeforeSize    int     `json:"before_size"`
	AfterSize     *int    `json:"after_size"`
	EmptyResponse bool    `json:"empty_response,omitempty"`
	Proposed      *string `json:"proposed,omitempty"`
}

// consolidatePeople is POST /admin/dream/consolidate-people: one
// consolidation-only people-writer pass (no new excerpts) over every
// context/people/* note of the agents in dream mode sim or companion, or of
// the one agent and person the body names. dry_run returns the changed
// files as proposed instead of saving them. Superadmin only: agents:write
// alone is granted to every self-signup.
func (rt *DreamRoutes) consolidatePeople(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	super, err := rt.Admin.IsSuperadmin(ctx, httpapi.StateOf(r).ActorID)
	if err != nil {
		return err
	}
	if !super {
		httpapi.WriteError(w, http.StatusForbidden, "FORBIDDEN", "Dream consolidation requires superadmin")
		return nil
	}
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("agent")
	}

	agentFilter, given, isString := filterField(f, "agent")
	if given && !isString {
		return httpapi.BadRequest(w, "agent must be a string")
	}
	personFilter, given, isString := filterField(f, "person_slug")
	if given {
		if !isString {
			return httpapi.BadRequest(w, "person_slug must be a string")
		}
		if canonical := peopleslug.Slug(personFilter); canonical == "" || canonical != personFilter {
			return httpapi.BadRequest(w, "invalid person_slug — must match [a-z0-9-]+ with no leading/trailing hyphen")
		}
	}
	dryRun := f.Truthy("dry_run")

	type agentRow struct{ name, mode string }
	var agents []agentRow
	query := `SELECT a.name, agc.dream_mode FROM actors a
              JOIN agent_configuration agc ON agc.actor_id = a.id
              WHERE agc.dream_mode IN ('sim', 'companion')
              ORDER BY a.name`
	var args []any
	if agentFilter != "" {
		query = `SELECT a.name, agc.dream_mode FROM actors a
              JOIN agent_configuration agc ON agc.actor_id = a.id
              WHERE a.name = $1 AND agc.dream_mode IN ('sim', 'companion')`
		args = []any{agentFilter}
	}
	rows, err := rt.DB.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a agentRow
		if err := rows.Scan(&a.name, &a.mode); err != nil {
			rows.Close()
			return err
		}
		agents = append(agents, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if agentFilter != "" && len(agents) == 0 {
		return notFound(w, `agent "`+agentFilter+`" not found or not in sim/companion mode`)
	}

	simPeople, err := rt.Dream.FindDreamAgent(ctx, "dream-sim-people")
	if err != nil {
		return err
	}
	companionPeople, err := rt.Dream.FindDreamAgent(ctx, "dream-companion-people")
	if err != nil {
		return err
	}

	now := time.Now
	if rt.Now != nil {
		now = rt.Now
	}
	today := now().UTC().Format("2006-01-02")
	results := []any{}

	for _, a := range agents {
		peopleAgent := companionPeople
		if a.mode == "sim" {
			peopleAgent = simPeople
		}
		if peopleAgent == "" {
			results = append(results, consolidateAgentError{a.name, "dream-" + a.mode + "-people not available"})
			continue
		}
		list, err := rt.allPeopleNotes(ctx, a.name)
		if err != nil {
			return err
		}
		for _, n := range list {
			if personFilter != "" && n.Slug != peoplePrefix+personFilter {
				continue
			}
			slug := strings.TrimPrefix(n.Slug, peoplePrefix)
			display := personDisplay(n.Title, slug)
			res, err := rt.Dream.RunPersonContextUpdate(ctx, a.name, peopleAgent, slug, display, "", today, dream.PersonOptions{DryRun: dryRun})
			if err != nil {
				results = append(results, consolidatePersonError{a.name, display, slug, httpapi.ErrorMessage(err)})
				continue
			}
			out := consolidateResult{
				Agent: a.name, Person: display, Slug: slug,
				Changed: res.Changed, Written: res.Written,
				BeforeSize:    jsutil.UTF16Len(res.ExistingFile),
				EmptyResponse: res.EmptyResponse,
			}
			if res.UpdatedFile != "" {
				size := jsutil.UTF16Len(res.UpdatedFile)
				out.AfterSize = &size
			}
			if dryRun && res.Changed {
				proposed := res.UpdatedFile
				out.Proposed = &proposed
			}
			results = append(results, out)
		}
	}

	return httpapi.WriteJSON(w, http.StatusOK, struct {
		DryRun  bool  `json:"dry_run"`
		Count   int   `json:"count"`
		Results []any `json:"results"`
	}{dryRun, len(results), results})
}

// filterField reads an optional string filter as the route did: absent,
// null and "" are not given; any other value is given, and isString says
// whether it is a string.
func filterField(f httpapi.Fields, name string) (value string, given, isString bool) {
	raw, ok := f[name]
	if !ok || string(raw) == "null" {
		return "", false, false
	}
	s, isString := f.String(name)
	if isString && s == "" {
		return "", false, false
	}
	return s, true, isString
}

// allPeopleNotes is every live context/people/* note of the namespace,
// listed a page at a time so a namespace with more than one page is not cut
// short.
func (rt *DreamRoutes) allPeopleNotes(ctx context.Context, namespace string) ([]notes.ListedNote, error) {
	var all []notes.ListedNote
	for offset := 0; ; offset += peopleNotesPage {
		page, err := rt.Notes.ListLive(ctx, notes.ListInput{
			Namespace: namespace,
			Limit:     json.RawMessage(strconv.Itoa(peopleNotesPage)),
			Offset:    json.RawMessage(strconv.Itoa(offset)),
			Prefix:    peoplePrefix,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < peopleNotesPage {
			return all, nil
		}
	}
}

// personDisplay is the person's name for the prompt and the result: the
// note title without its "People — " prefix, or the slug, on one line.
// Titles are editable in the admin UI, and a line break could add a "## "
// section to the prompt.
func personDisplay(title *string, slug string) string {
	display := ""
	if title != nil {
		display = strings.TrimPrefix(*title, "People — ")
	}
	if display == "" {
		display = slug
	}
	var b strings.Builder
	space := false
	for _, c := range display {
		if jsutil.IsSpace(c) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(c)
	}
	return jsutil.Trim(b.String())
}
