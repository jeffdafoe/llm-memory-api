package simapi

import (
	"context"
	"strconv"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Turn is one virtual_agent_calls row of /sim/raw-turns, with node-postgres's
// types: the bigint id and the numeric cost as text, the timestamp as
// toISOString.
type Turn struct {
	ID               string  `json:"id"`
	Agent            string  `json:"agent"`
	SceneID          *string `json:"scene_id"`
	Context          *string `json:"context"`
	ContextID        *string `json:"context_id"`
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	ServedBy         *string `json:"served_by"`
	SimActorID       *string `json:"sim_actor_id"`
	SimActorName     *string `json:"sim_actor_name"`
	SystemPrompt     *string `json:"system_prompt"`
	UserMessage      *string `json:"user_message"`
	Response         *string `json:"response"`
	Status           string  `json:"status"`
	StatusCode       *int    `json:"status_code"`
	ErrorMessage     *string `json:"error_message"`
	InputTokens      *int    `json:"input_tokens"`
	OutputTokens     *int    `json:"output_tokens"`
	CacheReadTokens  *int    `json:"cache_read_tokens"`
	CacheWriteTokens *int    `json:"cache_write_tokens"`
	Cost             *string `json:"cost"`
	DurationMs       *int    `json:"duration_ms"`
	CreatedAt        string  `json:"created_at"`
}

// RawTurns runs the raw-turns query: where is the WHERE clause over
// params, and the last parameter ($limitParam) is the row limit.
func (s *Service) RawTurns(ctx context.Context, where string, limitParam int, params []any) ([]Turn, error) {
	rows, err := s.DB.Query(ctx, `SELECT c.id::text, ac.name AS agent, c.scene_id::text, c.context, c.context_id, c.provider, c.model, c.served_by,
                c.sim_actor_id, c.sim_actor_name,
                c.system_prompt, c.user_message, c.response,
                c.status, c.status_code, c.error_message,
                c.input_tokens, c.output_tokens, c.cache_read_tokens, c.cache_write_tokens,
                c.cost::text, c.duration_ms, c.created_at
         FROM virtual_agent_calls c
         JOIN actors ac ON ac.id = c.actor_id
         `+where+`
         ORDER BY c.created_at DESC
         LIMIT $`+strconv.Itoa(limitParam), params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := []Turn{}
	for rows.Next() {
		var t Turn
		var created time.Time
		if err := rows.Scan(&t.ID, &t.Agent, &t.SceneID, &t.Context, &t.ContextID, &t.Provider, &t.Model, &t.ServedBy,
			&t.SimActorID, &t.SimActorName, &t.SystemPrompt, &t.UserMessage, &t.Response,
			&t.Status, &t.StatusCode, &t.ErrorMessage,
			&t.InputTokens, &t.OutputTokens, &t.CacheReadTokens, &t.CacheWriteTokens,
			&t.Cost, &t.DurationMs, &created); err != nil {
			return nil, err
		}
		t.CreatedAt = jsutil.DateISO(created)
		turns = append(turns, t)
	}
	return turns, rows.Err()
}
