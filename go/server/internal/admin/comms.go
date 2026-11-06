package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/chat"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/discussion"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgjson"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/visibility"
)

// visibleFilter is getVisibleActorIds for the caller: filter false for a
// viewer who sees everyone (Node's null), else the ids as the array Node
// bound.
func (rt *ContentRoutes) visibleFilter(r *http.Request) (ids []int, filter bool, err error) {
	v, err := rt.Visibility.VisibleActorIDs(r.Context(), httpapi.StateOf(r).ActorID)
	if err != nil {
		return nil, false, err
	}
	return visibleIDs(v)
}

func visibleIDs(v visibility.Visible) ([]int, bool, error) {
	if v.All {
		return nil, false, nil
	}
	ids := v.IDs
	if ids == nil {
		ids = []int{}
	}
	return ids, true, nil
}

// limitOr is `const { limit = fallback } = req.body`: the default only for
// an absent field (null stays null, which LIMIT reads as no limit), bound
// as node-postgres bound the value.
func limitOr(f httpapi.Fields, fallback int) any {
	raw := f.Raw("limit")
	if raw == nil {
		return fallback
	}
	return pgbind.Value(raw)
}

// discussionsList is POST /admin/discussions: every discussion whose
// participants the caller can all see, newest first, optionally by status.
func (rt *ContentRoutes) discussionsList(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("status")
	}
	ids, filter, err := rt.visibleFilter(r)
	if err != nil {
		return err
	}
	sql := `
        SELECT d.id, d.topic, d.status, d.mode, d.outcome, ac.name AS created_by, d.created_at, d.concluded_at,
               COUNT(dp.actor_id) AS participant_count
        FROM discussions d
        LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
        LEFT JOIN discussion_participants dp ON dp.discussion_id = d.id
    `
	var conditions []string
	var params []any
	if f.Truthy("status") {
		params = append(params, pgbind.Value(f.Raw("status")))
		conditions = append(conditions, "d.status = $"+strconv.Itoa(len(params)))
	}
	if filter {
		params = append(params, ids)
		conditions = append(conditions, `NOT EXISTS (
            SELECT 1 FROM discussion_participants dp2
            WHERE dp2.discussion_id = d.id AND dp2.actor_id != ALL($`+strconv.Itoa(len(params))+`))`)
	}
	if len(conditions) > 0 {
		sql += " WHERE " + joinAnd(conditions)
	}
	sql += " GROUP BY d.id, ac.name ORDER BY d.created_at DESC"
	rows, err := pgjson.Query(r.Context(), rt.DB, sql, params...)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"discussions": rows})
}

func joinAnd(conditions []string) string {
	out := ""
	for i, c := range conditions {
		if i > 0 {
			out += " AND "
		}
		out += c
	}
	return out
}

type discussionDetail struct {
	Discussion   pgjson.Row   `json:"discussion"`
	Participants []pgjson.Row `json:"participants"`
	Votes        []pgjson.Row `json:"votes"`
}

// discussionsDetail is POST /admin/discussions/detail: the discussion, its
// participants and its votes with their ballots. A discussion with any
// participant the caller cannot see is the same 404 as a missing one.
func (rt *ContentRoutes) discussionsDetail(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("discussion_id")
	}
	if !f.Truthy("discussion_id") {
		return httpapi.BadRequest(w, "Required field: discussion_id")
	}
	id := pgbind.Value(f.Raw("discussion_id"))
	ids, filter, err := rt.visibleFilter(r)
	if err != nil {
		return err
	}
	if filter {
		hidden, err := pgjson.Query(r.Context(), rt.DB,
			`SELECT 1 FROM discussion_participants
             WHERE discussion_id = $1 AND actor_id != ALL($2) LIMIT 1`, id, ids)
		if err != nil {
			return err
		}
		if len(hidden) > 0 {
			httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Discussion not found")
			return nil
		}
	}
	d, found, err := pgjson.QueryOne(r.Context(), rt.DB,
		`SELECT d.*, ac.name AS created_by
         FROM discussions d
         LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
         WHERE d.id = $1`, id)
	if err != nil {
		return err
	}
	if !found {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Discussion not found")
		return nil
	}
	participants, err := pgjson.Query(r.Context(), rt.DB,
		`SELECT dp.*, ac.name AS agent
         FROM discussion_participants dp
         JOIN actors ac ON ac.id = dp.actor_id
         WHERE dp.discussion_id = $1
         ORDER BY ac.name`, id)
	if err != nil {
		return err
	}
	votes, err := pgjson.Query(r.Context(), rt.DB,
		`SELECT v.*, pac.name AS proposed_by,
                json_agg(json_build_object('agent', bac.name, 'choice', b.choice, 'reason', b.reason, 'cast_at', b.cast_at)) AS ballots
         FROM discussion_votes v
         LEFT JOIN actors pac ON pac.id = v.proposed_by_actor_id
         LEFT JOIN discussion_ballots b ON b.vote_id = v.id
         LEFT JOIN actors bac ON bac.id = b.actor_id
         WHERE v.discussion_id = $1
         GROUP BY v.id, pac.name
         ORDER BY v.id`, id)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, discussionDetail{Discussion: d, Participants: participants, Votes: votes})
}

// trimmedContent is sanitize.content(req.body[name] || ”).trim(): a
// truthy value that is not a string is refused (Node's .trim() threw).
func trimmedContent(f httpapi.Fields, name string) (string, error) {
	if !f.Truthy(name) {
		return "", nil
	}
	s, err := f.RequireString(name)
	if err != nil {
		return "", err
	}
	return jsutil.Trim(sanitize.Content(s)), nil
}

// discussionsCreate is POST /admin/discussions/create: a discussion
// created by the admin user, who is added to the participants when not
// named and joins at once (as do virtual agents).
func (rt *ContentRoutes) discussionsCreate(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return errors.New("Cannot read properties of undefined (reading 'topic')")
	}
	topic, err := trimmedContent(f, "topic")
	if err != nil {
		return err
	}
	// Array.from(new Set((participants || []).filter(Boolean)))
	var participants []string
	if f.Truthy("participants") {
		elems, isArray := f.Array("participants")
		if !isArray {
			return httpapi.NotString("participants")
		}
		seen := map[string]bool{}
		for _, e := range elems {
			if !pgbind.Truthy(e) {
				continue
			}
			var name string
			if len(e) == 0 || e[0] != '"' || json.Unmarshal(e, &name) != nil {
				return apierr.New(http.StatusBadRequest, "participants must be an array of strings")
			}
			if !seen[name] {
				seen[name] = true
				participants = append(participants, name)
			}
		}
	}
	mode := "realtime"
	if s, isString := f.String("mode"); isString && s == "async" {
		mode = "async"
	}
	context := ""
	if f.Truthy("context") {
		s, err := f.RequireString("context")
		if err != nil {
			return err
		}
		context = sanitize.Content(s)
	}
	if topic == "" || len(participants) == 0 {
		return httpapi.BadRequest(w, "Required fields: topic, participants (array of agent names)")
	}
	user := User(r)
	all := participants
	if !contains(participants, user.Username) {
		all = append([]string{user.Username}, participants...)
	}
	res, err := rt.Discussions.Create(r.Context(), discussion.CreateInput{
		Topic: topic, CreatedBy: user.Username, Participants: all, Optional: []string{}, Mode: mode, Context: context,
	})
	if err != nil {
		return err
	}
	// result.discussion_id is undefined in Node (the id is result.discussion.id),
	// so JSON.stringify leaves it out of the log line.
	rt.LogAdmin("discussion_create", logger.Fields{
		{Key: "topic", Value: topic}, {Key: "participants", Value: all}, {Key: "mode", Value: mode},
		{Key: "user_id", Value: user.ID},
	})
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

// discussionsSend is POST /admin/discussions/send: a chat message into the
// discussion from the admin user.
func (rt *ContentRoutes) discussionsSend(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return errors.New("Cannot read properties of undefined (reading 'discussion_id')")
	}
	message, err := trimmedContent(f, "message")
	if err != nil {
		return err
	}
	if !f.Truthy("discussion_id") || message == "" {
		return httpapi.BadRequest(w, "Required fields: discussion_id, message")
	}
	// chatSend bound discussion_id as sent; Postgres's cast refuses what
	// its queries refused, with the same message.
	var discussionID int64
	if err := rt.DB.QueryRow(r.Context(), "SELECT $1::integer", pgbind.Value(f.Raw("discussion_id"))).Scan(&discussionID); err != nil {
		return err
	}
	user := User(r)
	sent, _, err := rt.Chat.Send(r.Context(), chat.SendInput{FromAgent: user.Username, DiscussionID: discussionID, Message: &message})
	if err != nil {
		return err
	}
	rt.LogAdmin("discussion_send", logger.Fields{
		{Key: "discussion_id", Value: f.Raw("discussion_id")}, {Key: "from", Value: user.Username}, {Key: "user_id", Value: user.ID},
	})
	return httpapi.WriteJSON(w, http.StatusOK, discussionSent{FromAgent: sent.FromAgent, ToAgents: sent.ToAgents, SentAt: sent.SentAt})
}

// discussionSent is chatSend's whole return value, which this route sends
// as is: pendingReplyPromise is null for a discussion message (only a
// direct message to a virtual agent sets it).
type discussionSent struct {
	FromAgent           string          `json:"from_agent"`
	ToAgents            []chat.Delivery `json:"to_agents"`
	SentAt              chat.Time       `json:"sent_at"`
	PendingReplyPromise *struct{}       `json:"pendingReplyPromise"`
}

// discussionsConclude is POST /admin/discussions/conclude: conclude (or,
// for a waiting one, cancel) as the admin user.
func (rt *ContentRoutes) discussionsConclude(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("discussion_id")
	}
	if !f.Truthy("discussion_id") {
		return httpapi.BadRequest(w, "Required field: discussion_id")
	}
	user := User(r)
	res, err := rt.Discussions.Conclude(r.Context(), discussion.RawID(f.Raw("discussion_id")), user.Username, false)
	if err != nil {
		return err
	}
	rt.LogAdmin("discussion_conclude", logger.Fields{
		{Key: "discussion_id", Value: f.Raw("discussion_id")}, {Key: "agent", Value: user.Username}, {Key: "user_id", Value: user.ID},
	})
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

// chatList is POST /admin/chat: a discussion's messages (one row per text),
// or the delivery rows outside discussions, newest first.
func (rt *ContentRoutes) chatList(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		// const { limit = 50 } = req.body: with a default, V8 reports the
		// property read.
		return errors.New("Cannot read properties of undefined (reading 'limit')")
	}
	ids, filter, err := rt.visibleFilter(r)
	if err != nil {
		return err
	}
	limit := limitOr(f, 50)
	const sceneSelect = `, cmt.scene_structure AS structure_name`
	var sql string
	var params []any
	if discussionID, ok := SafeInt(f.Raw("discussion_id")); ok && discussionID != 0 {
		sql = `SELECT cmt.id, cmt.id AS text_id, fa.name AS from_agent, cmt.message, cmt.tool_calls, cmt.sent_at, cmt.discussion_id, cmt.scene_id, cmt.conversation_id` + sceneSelect + `
                   FROM chat_message_texts cmt
                   JOIN actors fa ON fa.id = cmt.from_actor_id`
		conditions := []string{"cmt.discussion_id = $1"}
		params = []any{discussionID}
		if filter {
			params = append(params, ids)
			conditions = append(conditions, "cmt.from_actor_id = ANY($"+strconv.Itoa(len(params))+")")
		}
		sql += " WHERE " + joinAnd(conditions)
	} else {
		sql = `SELECT cm.id, cmt.id AS text_id, fa.name AS from_agent, ta.name AS to_agent, cmt.message, cmt.tool_calls, cmt.sent_at, cm.acked_at, cmt.scene_id, cmt.conversation_id` + sceneSelect + `
                   FROM chat_messages cm
                   JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
                   JOIN actors fa ON fa.id = cmt.from_actor_id
                   JOIN actors ta ON ta.id = cm.to_actor_id`
		conditions := []string{"cm.deleted_at IS NULL", "cmt.discussion_id IS NULL"}
		if filter {
			params = append(params, ids)
			n := strconv.Itoa(len(params))
			conditions = append(conditions, "(cmt.from_actor_id = ANY($"+n+") OR cm.to_actor_id = ANY($"+n+"))")
		}
		sql += " WHERE " + joinAnd(conditions)
	}
	sql += " ORDER BY cmt.sent_at DESC LIMIT $" + strconv.Itoa(len(params)+1)
	params = append(params, limit)
	rows, err := pgjson.Query(r.Context(), rt.DB, sql, params...)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"messages": rows})
}

// mailList is POST /admin/mail: mail to or from the actors the caller can
// see, newest first, unsent mail included.
func (rt *ContentRoutes) mailList(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		// const { limit = 50 } = req.body: with a default, V8 reports the
		// property read.
		return errors.New("Cannot read properties of undefined (reading 'limit')")
	}
	ids, filter, err := rt.visibleFilter(r)
	if err != nil {
		return err
	}
	sql := `SELECT m.id, fa.name AS from_agent, ta.name AS to_agent, m.subject, m.body, m.sent_at, m.acked_at, m.deleted_at
         FROM mail m
         JOIN actors fa ON fa.id = m.from_actor_id
         JOIN actors ta ON ta.id = m.to_actor_id`
	var params []any
	if filter {
		params = append(params, ids)
		sql += " WHERE (m.from_actor_id = ANY($1) OR m.to_actor_id = ANY($1))"
	}
	sql += " ORDER BY m.sent_at DESC LIMIT $" + strconv.Itoa(len(params)+1)
	params = append(params, limitOr(f, 50))
	rows, err := pgjson.Query(r.Context(), rt.DB, sql, params...)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"messages": rows})
}

type deletedMessage struct {
	ID      json.RawMessage `json:"id"`
	Message string          `json:"message"`
}

// mailDelete is POST /admin/mail/delete: a soft delete, of mail whose
// sender or recipient the caller can see.
func (rt *ContentRoutes) mailDelete(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("id")
	}
	if !f.Truthy("id") {
		return httpapi.BadRequest(w, "Required field: id")
	}
	ids, filter, err := rt.visibleFilter(r)
	if err != nil {
		return err
	}
	sql := "UPDATE mail SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL"
	params := []any{pgbind.Value(f.Raw("id"))}
	if filter {
		params = append(params, ids)
		sql += " AND (from_actor_id = ANY($2) OR to_actor_id = ANY($2))"
	}
	sql += " RETURNING id"
	ctx, cancel := detached(r)
	defer cancel()
	rows, err := pgjson.Query(ctx, rt.DB, sql, params...)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Mail not found or already deleted")
		return nil
	}
	rt.LogAdmin("mail_delete", logger.Fields{{Key: "mail_id", Value: f.Raw("id")}, {Key: "user_id", Value: User(r).ID}})
	return httpapi.WriteJSON(w, http.StatusOK, deletedMessage{ID: f.Raw("id"), Message: "Mail deleted"})
}

// chatDelete is POST /admin/chat/delete: a soft delete of one delivery
// row, whose recipient or sender the caller can see.
func (rt *ContentRoutes) chatDelete(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("id")
	}
	if !f.Truthy("id") {
		return httpapi.BadRequest(w, "Required field: id")
	}
	ids, filter, err := rt.visibleFilter(r)
	if err != nil {
		return err
	}
	sql := "UPDATE chat_messages SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL"
	params := []any{pgbind.Value(f.Raw("id"))}
	if filter {
		params = append(params, ids)
		sql += ` AND (to_actor_id = ANY($2) OR EXISTS (
            SELECT 1 FROM chat_message_texts cmt WHERE cmt.id = chat_messages.message_text_id AND cmt.from_actor_id = ANY($2)
        ))`
	}
	sql += " RETURNING id"
	ctx, cancel := detached(r)
	defer cancel()
	rows, err := pgjson.Query(ctx, rt.DB, sql, params...)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Chat message not found or already deleted")
		return nil
	}
	rt.LogAdmin("chat_delete", logger.Fields{{Key: "chat_id", Value: f.Raw("id")}, {Key: "user_id", Value: User(r).ID}})
	return httpapi.WriteJSON(w, http.StatusOK, deletedMessage{ID: f.Raw("id"), Message: "Chat message deleted"})
}

type mailSent struct {
	ID      json.RawMessage `json:"id"`
	SentAt  json.RawMessage `json:"sent_at"`
	Message string          `json:"message"`
}

// mailSend is POST /admin/mail/send: mail from the admin user to an agent it
// can see. It is a plain insert: a virtual recipient is not triggered.
func (rt *ContentRoutes) mailSend(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return errors.New("Cannot read properties of undefined (reading 'to')")
	}
	to, toIsString := f.String("to")
	to = sanitize.AgentName(to)
	subject, subjectIsString := f.String("subject")
	subject = sanitize.Content(subject)
	body, bodyIsString := f.String("body")
	body = sanitize.Content(body)
	// A truthy value of another type passes Node's check; the port refuses
	// it below.
	present := func(name string, isString bool, s string) bool {
		if isString {
			return s != ""
		}
		return f.Truthy(name)
	}
	if !present("to", toIsString, to) || !present("subject", subjectIsString, subject) || !present("body", bodyIsString, body) {
		return httpapi.BadRequest(w, "Required fields: to, subject, body")
	}
	for _, n := range []struct {
		name     string
		isString bool
	}{{"to", toIsString}, {"subject", subjectIsString}, {"body", bodyIsString}} {
		if !n.isString {
			return httpapi.NotString(n.name)
		}
	}
	toActor, ok, err := rt.RequireVisibility(w, r, to)
	if err != nil || !ok {
		return err
	}
	user := User(r)
	fromActor, err := rt.Actors.RequireByName(r.Context(), user.Username)
	if err != nil {
		return err
	}
	ctx, cancel := detached(r)
	defer cancel()
	row, _, err := pgjson.QueryOne(ctx, rt.DB,
		"INSERT INTO mail (to_actor_id, from_actor_id, subject, body) VALUES ($1, $2, $3, $4) RETURNING id, sent_at",
		toActor.ID, fromActor.ID, subject, body)
	if err != nil {
		return err
	}
	id, _ := row.Get("id")
	sentAt, _ := row.Get("sent_at")
	rt.LogAdmin("mail_send", logger.Fields{
		{Key: "from", Value: user.Username}, {Key: "to", Value: to}, {Key: "mail_id", Value: id},
		{Key: "subject", Value: subject}, {Key: "user_id", Value: user.ID},
	})
	return httpapi.WriteJSON(w, http.StatusOK, mailSent{ID: id, SentAt: sentAt, Message: "Mail sent"})
}
