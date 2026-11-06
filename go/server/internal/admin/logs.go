package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgjson"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/signup"
)

type noteTotals struct {
	Total      int `json:"total"`
	Namespaces int `json:"namespaces"`
}

type dashboard struct {
	Agents        []pgjson.Row `json:"agents"`
	Discussions   []pgjson.Row `json:"discussions"`
	Mail          []pgjson.Row `json:"mail"`
	Notes         noteTotals   `json:"notes"`
	ErrorCount24h int          `json:"error_count_24h"`
}

// dashboardSummary is POST /admin/dashboard: the agents, recent
// discussions and mail the caller can see, note counts of the readable
// namespaces, and the 5xx count of the last 24 hours.
func (rt *ContentRoutes) dashboardSummary(w http.ResponseWriter, r *http.Request) error {
	ids, filter, err := rt.visibleFilter(r)
	if err != nil {
		return err
	}
	var visible []any
	if filter {
		visible = []any{ids}
	}

	agentsSQL := `SELECT agent, status, last_seen, registered_at, provider, model, virtual, active_since
         FROM agent_status`
	if filter {
		agentsSQL += " WHERE actor_id = ANY($1)"
	}
	agentsSQL += ` ORDER BY CASE status WHEN 'online' THEN 0 WHEN 'available' THEN 1 WHEN 'offline' THEN 2 ELSE 3 END, last_seen DESC NULLS LAST`
	agents, err := pgjson.Query(r.Context(), rt.DB, agentsSQL, visible...)
	if err != nil {
		return err
	}

	discussionsSQL := `
        SELECT d.id, d.topic, d.status, d.outcome, ac.name AS created_by, d.created_at,
                COUNT(dp.actor_id) AS participant_count
         FROM discussions d
         LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
         LEFT JOIN discussion_participants dp ON dp.discussion_id = d.id`
	if filter {
		discussionsSQL += ` WHERE NOT EXISTS (
            SELECT 1 FROM discussion_participants dp2
            WHERE dp2.discussion_id = d.id AND dp2.actor_id != ALL($1))`
	}
	discussionsSQL += ` GROUP BY d.id, ac.name
         ORDER BY CASE d.status WHEN 'active' THEN 0 ELSE 1 END, d.created_at DESC
         LIMIT 10`
	discussions, err := pgjson.Query(r.Context(), rt.DB, discussionsSQL, visible...)
	if err != nil {
		return err
	}

	mailSQL := `SELECT m.id, fa.name AS from_agent, ta.name AS to_agent, m.subject, m.body, m.sent_at, m.acked_at
         FROM mail m
         JOIN actors fa ON fa.id = m.from_actor_id
         JOIN actors ta ON ta.id = m.to_actor_id
         WHERE m.deleted_at IS NULL`
	if filter {
		mailSQL += " AND (m.from_actor_id = ANY($1) OR m.to_actor_id = ANY($1))"
	}
	mailSQL += ` ORDER BY CASE WHEN m.acked_at IS NULL THEN 0 ELSE 1 END, m.sent_at DESC LIMIT 15`
	mail, err := pgjson.Query(r.Context(), rt.DB, mailSQL, visible...)
	if err != nil {
		return err
	}

	type nsCount struct {
		namespace string
		count     string
	}
	rows, err := rt.DB.Query(r.Context(), "SELECT namespace, COUNT(*) AS count FROM documents WHERE deleted_at IS NULL GROUP BY namespace ORDER BY namespace")
	if err != nil {
		return err
	}
	var counts []nsCount
	for rows.Next() {
		var c nsCount
		var n int64
		if err := rows.Scan(&c.namespace, &n); err != nil {
			rows.Close()
			return err
		}
		c.count = strconv.FormatInt(n, 10)
		counts = append(counts, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	readable, all, err := rt.readable(r)
	if err != nil {
		return err
	}
	totals := noteTotals{}
	for _, c := range counts {
		if !all && !contains(readable, c.namespace) {
			continue
		}
		n, _ := strconv.Atoi(c.count)
		totals.Total += n
		totals.Namespaces++
	}

	// Only 5xx; a NULL status is a row from before the column existed.
	// Unattributed rows count for a filtered viewer too.
	errorSQL := `SELECT COUNT(*) AS count FROM error_log WHERE created_at > NOW() - INTERVAL '24 hours' AND (status_code IS NULL OR status_code >= 500)`
	if filter {
		errorSQL += " AND (actor_id = ANY($1) OR actor_id IS NULL)"
	}
	var errorCount int64
	if err := rt.DB.QueryRow(r.Context(), errorSQL, visible...).Scan(&errorCount); err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, dashboard{
		Agents: agents, Discussions: discussions, Mail: mail, Notes: totals, ErrorCount24h: int(errorCount),
	})
}

// logEntries is getEntries / getErrorLogEntries: rows after since_id
// (oldest first, at most 500), or the newest `limit` rows in id order.
// visible filters by actor; an empty list sees nothing. The actor ids go as
// one placeholder each, as Node wrote them.
func logEntries(ctx context.Context, rt *ContentRoutes, cols, table, alias string, includeUnattributed bool, sinceID, limit json.RawMessage, ids []int, filter bool) ([]pgjson.Row, error) {
	var params []any
	visFilter := ""
	if filter {
		if len(ids) == 0 {
			return []pgjson.Row{}, nil
		}
		placeholders := make([]string, len(ids))
		for i, id := range ids {
			placeholders[i] = "$" + strconv.Itoa(i+1)
			params = append(params, id)
		}
		visFilter = " AND " + alias + ".actor_id IN (" + strings.Join(placeholders, ", ") + ")"
		if includeUnattributed {
			visFilter = " AND (" + alias + ".actor_id IN (" + strings.Join(placeholders, ", ") + ") OR " + alias + ".actor_id IS NULL)"
		}
	}
	from := " FROM " + table + " " + alias + " LEFT JOIN actors ac ON ac.id = " + alias + ".actor_id"
	// since_id || 0, then if (sinceId)
	if pgbind.Truthy(sinceID) {
		params = append(params, pgbind.Value(sinceID))
		return pgjson.Query(ctx, rt.DB,
			"SELECT "+cols+from+" WHERE "+alias+".id > $"+strconv.Itoa(len(params))+visFilter+" ORDER BY "+alias+".id ASC LIMIT 500", params...)
	}
	// limit || 100
	var lim any = 100
	if pgbind.Truthy(limit) {
		lim = pgbind.Value(limit)
	}
	params = append(params, lim)
	rows, err := pgjson.Query(ctx, rt.DB,
		"SELECT "+cols+from+" WHERE 1=1"+visFilter+" ORDER BY "+alias+".id DESC LIMIT $"+strconv.Itoa(len(params)), params...)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

// apiLog is POST /admin/api-log: request_log rows of the actors the caller
// can see (unauthenticated rows only for one who sees everyone).
func (rt *ContentRoutes) apiLog(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("since_id")
	}
	ids, filter, err := rt.visibleFilter(r)
	if err != nil {
		return err
	}
	entries, err := logEntries(r.Context(), rt,
		`r.id, r.timestamp, r.method, r.path, r.status,
                  r.duration_ms AS duration, ac.name AS agent, r.ip,
                  r.request_length, r.response_length`,
		"request_log", "r", false, f.Raw("since_id"), f.Raw("limit"), ids, filter)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"entries": entries})
}

// errorLog is POST /admin/error-log: error_log rows of the actors the
// caller can see and the unattributed ones.
func (rt *ContentRoutes) errorLog(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("since_id")
	}
	ids, filter, err := rt.visibleFilter(r)
	if err != nil {
		return err
	}
	entries, err := logEntries(r.Context(), rt,
		`e.id, e.created_at, e.subsystem, e.action, ac.name AS agent,
                  e.context, e.context_id, e.error_message, e.error_detail, e.status_code`,
		"error_log", "e", true, f.Raw("since_id"), f.Raw("limit"), ids, filter)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"entries": entries})
}

// funnelAccount is one row of signup-funnel.js funnelRows, in its keys.
type funnelAccount struct {
	Name              string   `json:"name"`
	Email             *string  `json:"email"`
	CreatedAt         string   `json:"created_at"`
	Tracked           bool     `json:"tracked"`
	GuideShownAt      *string  `json:"guide_shown_at"`
	Client            *string  `json:"client"`
	ClientPicked      bool     `json:"client_picked"`
	Copies            []string `json:"copies"`
	Connected         bool     `json:"connected"`
	FirstConnectedAt  *string  `json:"first_connected_at"`
	KeyLastUsedAt     *string  `json:"key_last_used_at"`
	FirstToolCallAt   *string  `json:"first_tool_call_at"`
	LastToolCallAt    *string  `json:"last_tool_call_at"`
	ToolCallCount     int      `json:"tool_call_count"`
	ActiveAfter7Days  *bool    `json:"active_after_7_days"`
	ActiveAfter30Days *bool    `json:"active_after_30_days"`
}

type funnelSummary struct {
	SignedUp           int `json:"signed_up"`
	SawGuide           int `json:"saw_guide"`
	PickedClient       int `json:"picked_client"`
	Connected          int `json:"connected"`
	FirstToolCall      int `json:"first_tool_call"`
	ActiveAfter7Days   int `json:"active_after_7_days"`
	OldEnoughFor7Days  int `json:"old_enough_for_7_days"`
	ActiveAfter30Days  int `json:"active_after_30_days"`
	OldEnoughFor30Days int `json:"old_enough_for_30_days"`
}

type funnelResponse struct {
	Since      *string         `json:"since"`
	Summary    funnelSummary   `json:"summary"`
	SummaryAll funnelSummary   `json:"summary_all"`
	Accounts   []funnelAccount `json:"accounts"`
}

func isoPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := jsutil.DateISO(*t)
	return &s
}

// activeAfter is signup-funnel.js activeAfter: nil while the account is
// younger than days, else whether it called a tool at least that long
// after it was created. Times are Dates, so milliseconds.
func activeAfter(now time.Time, created time.Time, lastToolCall *time.Time, days int) *bool {
	threshold := created.UnixMilli() + int64(days)*86400000
	if now.UnixMilli() < threshold {
		return nil
	}
	v := lastToolCall != nil && lastToolCall.UnixMilli() >= threshold
	return &v
}

// funnel is POST /admin/funnel: the signup funnel of the outside accounts
// (self-registered people: not jeff, not the zbbs- sim accounts, not
// virtual agents), newest first, with its stage counts over the tracked
// accounts and over all of them.
func (rt *ContentRoutes) funnel(w http.ResponseWriter, r *http.Request) error {
	since, err := signup.TrackingSince(rt.Config)
	if err != nil {
		return err
	}
	rows, err := rt.DB.Query(r.Context(),
		`SELECT a.id, a.name, a.email, a.created_at,
                a.first_connected_at, a.first_tool_call_at, a.last_tool_call_at, a.tool_call_count,
                (a.created_at >= $1::timestamptz) AS tracked,
                ev.picked_client, ev.shown_client, ev.copies, ev.guide_shown_at,
                keys.key_last_used_at
         FROM actors a
         JOIN agent_configuration agc ON agc.actor_id = a.id
         LEFT JOIN LATERAL (
             SELECT (ARRAY_AGG(value ORDER BY created_at DESC) FILTER (WHERE event = 'client'))[1] AS picked_client,
                    (ARRAY_AGG(value ORDER BY created_at) FILTER (WHERE event = 'guide_shown'))[1] AS shown_client,
                    MIN(created_at) FILTER (WHERE event = 'guide_shown') AS guide_shown_at,
                    COALESCE(ARRAY_AGG(DISTINCT value) FILTER (WHERE event = 'copy'), '{}') AS copies
             FROM signup_events s
             WHERE s.actor_id = a.id
         ) ev ON TRUE
         LEFT JOIN LATERAL (
             SELECT MAX(k.last_used_at) AS key_last_used_at
             FROM agent_api_keys k
             WHERE k.actor_id = a.id
         ) keys ON TRUE
         WHERE a.created_by = a.id
           AND NOT agc.virtual
           AND a.name <> 'jeff'
           AND a.name NOT LIKE 'zbbs-%'
         ORDER BY a.created_at DESC`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	now := time.Now()
	accounts := []funnelAccount{}
	for rows.Next() {
		var (
			id                                                 int
			a                                                  funnelAccount
			created                                            time.Time
			firstConnected, firstToolCall, lastToolCall, shown *time.Time
			keyUsed                                            *time.Time
			tracked                                            *bool
			picked, shownClient                                *string
			copies                                             []string
		)
		if err := rows.Scan(&id, &a.Name, &a.Email, &created, &firstConnected, &firstToolCall, &lastToolCall,
			&a.ToolCallCount, &tracked, &picked, &shownClient, &copies, &shown, &keyUsed); err != nil {
			return err
		}
		a.CreatedAt = jsutil.DateISO(created)
		a.Tracked = tracked != nil && *tracked
		a.GuideShownAt = isoPtr(shown)
		// picked_client || shown_client || null: an empty string is falsy.
		switch {
		case picked != nil && *picked != "":
			a.Client = picked
		case shownClient != nil && *shownClient != "":
			a.Client = shownClient
		}
		a.ClientPicked = picked != nil && *picked != ""
		a.Copies = copies
		if a.Copies == nil {
			a.Copies = []string{}
		}
		a.Connected = firstConnected != nil || (!a.Tracked && keyUsed != nil)
		a.FirstConnectedAt = isoPtr(firstConnected)
		a.KeyLastUsedAt = isoPtr(keyUsed)
		a.FirstToolCallAt = isoPtr(firstToolCall)
		a.LastToolCallAt = isoPtr(lastToolCall)
		a.ActiveAfter7Days = activeAfter(now, created, lastToolCall, 7)
		a.ActiveAfter30Days = activeAfter(now, created, lastToolCall, 30)
		accounts = append(accounts, a)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var tracked []funnelAccount
	for _, a := range accounts {
		if a.Tracked {
			tracked = append(tracked, a)
		}
	}
	return httpapi.WriteJSON(w, http.StatusOK, funnelResponse{
		Since: since, Summary: summarize(tracked), SummaryAll: summarize(accounts), Accounts: accounts,
	})
}

// summarize is funnelSummary: the 7- and 30-day stages count only the
// accounts old enough to have reached them.
func summarize(rows []funnelAccount) funnelSummary {
	s := funnelSummary{SignedUp: len(rows)}
	for _, r := range rows {
		if r.GuideShownAt != nil {
			s.SawGuide++
		}
		if r.ClientPicked {
			s.PickedClient++
		}
		if r.Connected {
			s.Connected++
		}
		if r.FirstToolCallAt != nil || r.ToolCallCount > 0 {
			s.FirstToolCall++
		}
		if r.ActiveAfter7Days != nil {
			s.OldEnoughFor7Days++
			if *r.ActiveAfter7Days {
				s.ActiveAfter7Days++
			}
		}
		if r.ActiveAfter30Days != nil {
			s.OldEnoughFor30Days++
			if *r.ActiveAfter30Days {
				s.ActiveAfter30Days++
			}
		}
	}
	return s
}
