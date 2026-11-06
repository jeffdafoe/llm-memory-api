package admin

import (
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// secretConfigKeys are config keys whose values never go to the client.
var secretConfigKeys = map[string]bool{
	"mcp_oauth_bearer_secret":      true,
	"virtual_agent_encryption_key": true,
}

const redactedValue = "••••••••"

type configRow struct {
	Key         string  `json:"key"`
	Value       *string `json:"value"`
	Description *string `json:"description"`
}

// configList is POST /admin/config/list: every config row by key, secrets
// redacted.
func (rt *Routes) configList(w http.ResponseWriter, r *http.Request) error {
	rows, err := rt.DB.Query(r.Context(), "SELECT key, value, description FROM config ORDER BY key")
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []configRow{}
	for rows.Next() {
		var row configRow
		if err := rows.Scan(&row.Key, &row.Value, &row.Description); err != nil {
			return err
		}
		if secretConfigKeys[row.Key] {
			redacted := redactedValue
			row.Value = &redacted
		}
		list = append(list, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]configRow{"config": list})
}

// configUpdate is POST /admin/config/update: one value by key, written to
// the table and to the in-memory store, so it takes effect at once.
func (rt *Routes) configUpdate(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("key")
	}
	if !f.Truthy("key") {
		return httpapi.BadRequest(w, "Required field: key")
	}
	key, err := f.RequireString("key")
	if err != nil {
		return err
	}
	// value || ''
	value, err := f.OptionalString("value")
	if err != nil {
		return err
	}
	// Node's write and its in-memory update ran on after a disconnect.
	ctx, cancel := detached(r)
	defer cancel()
	tag, err := rt.DB.Exec(ctx, "UPDATE config SET value = $1 WHERE key = $2", value, key)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Config key not found")
		return nil
	}
	rt.Config.Set(key, value)
	rt.LogAdmin("config_update", logger.Fields{{Key: "key", Value: key}})
	return httpapi.WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}
