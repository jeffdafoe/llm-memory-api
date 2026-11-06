package virtualagent

import (
	"context"
	"math"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// loadDirectChatHistory is the newest maxMessages direct-chat rows between
// two agents, oldest first, from the last virtual_agent_chat_history_hours
// (parseInt, else 4) counted from the start of the hour. Error rows
// ([Retrying], [Error]) are left out, so a tool-using agent never reads its
// own failures back as conversation. sceneID, when set, keeps only that
// scene's rows.
func (s *Service) loadDirectChatHistory(ctx context.Context, agent1, agent2 string, sceneID *string, maxMessages int) ([]chatRow, error) {
	value, err := s.Config.Get("virtual_agent_chat_history_hours")
	if err != nil {
		return nil, err
	}
	hours, ok := jsutil.ParseInt(value)
	if !ok || hours == 0 {
		hours = 4
	}
	actor1, err := s.Actors.RequireByName(ctx, agent1)
	if err != nil {
		return nil, err
	}
	actor2, err := s.Actors.RequireByName(ctx, agent2)
	if err != nil {
		return nil, err
	}
	var scene any
	if sceneID != nil && *sceneID != "" {
		scene = *sceneID
	}
	rows, err := s.DB.Query(ctx,
		`SELECT fa.name AS from_agent, ta.name AS to_agent, cmt.message, cmt.sent_at,
                cmt.tool_calls, cmt.tool_call_id
         FROM chat_messages cm
         JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
         JOIN actors fa ON fa.id = cmt.from_actor_id
         JOIN actors ta ON ta.id = cm.to_actor_id
         WHERE cmt.discussion_id IS NULL AND cm.deleted_at IS NULL
           AND NOT cmt.is_error
         AND ((cmt.from_actor_id = $1 AND cm.to_actor_id = $2) OR (cmt.from_actor_id = $2 AND cm.to_actor_id = $1))
         AND cmt.sent_at >= date_trunc('hour', NOW()) - INTERVAL '1 hour' * $3
         AND ($5::uuid IS NULL OR cmt.scene_id = $5::uuid)
         ORDER BY cm.id DESC LIMIT $4`,
		actor1.ID, actor2.ID, jsutil.Num(hours), maxMessages, scene)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chatRow
	for rows.Next() {
		var r chatRow
		if err := rows.Scan(&r.FromAgent, &r.ToAgent, &r.Message, &r.SentAt, &r.ToolCalls, &r.ToolCallID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if out == nil {
		out = []chatRow{}
	}
	return out, nil
}

// The grid the head of a full history window is cut to, and the rows the
// cut always keeps.
const (
	historyHeadGridMs  = 15 * 60 * 1000
	historyHeadMinKeep = 25
)

// stabilizeHistoryWindow keeps the replayed prefix of a busy NPC's history
// the same between turns (LLM-501). At the row cap, a plain newest-N window
// drops its oldest row on every new one, which changes the first byte of
// the replay and misses the provider's prompt cache every turn. So a window
// that hit the cap is cut at a 15-minute line in absolute time (the first
// row's time rounded up to the grid); the head then moves once per band,
// not once per row. It never keeps fewer than the newest 25 rows; when that
// floor binds the head slides again, as context matters more than the cache
// there. A window under the cap is returned as is.
func stabilizeHistoryWindow(rows []chatRow, maxMessages int) []chatRow {
	if len(rows) < maxMessages || len(rows) == 0 || rows[0].SentAt == nil {
		return rows
	}
	firstMs := sentMs(*rows[0].SentAt)
	gridLine := math.Ceil(firstMs/historyHeadGridMs) * historyHeadGridMs
	cut := 0
	for cut < len(rows)-historyHeadMinKeep {
		if t := rows[cut].SentAt; t != nil && sentMs(*t) < gridLine {
			cut++
		} else {
			break
		}
	}
	return rows[cut:]
}
