package admin

import (
	"encoding/json"
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
)

// writeJS sends a jsutil value with JSON.stringify's bytes.
func writeJS(w http.ResponseWriter, v any) error {
	return httpapi.WriteJSON(w, http.StatusOK, json.RawMessage(jsutil.Marshal(v)))
}

// providersRegistry is POST /admin/providers/registry: the provider and
// model registry. Static reference data, so Node checks no permission.
func (rt *Routes) providersRegistry(w http.ResponseWriter, r *http.Request) error {
	return httpapi.WriteJSON(w, http.StatusOK, json.RawMessage(providers.Registry()))
}

// providersDefaults is POST /admin/providers/defaults: the configuration a
// new agent on provider/model starts with.
func (rt *Routes) providersDefaults(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("provider")
	}
	if !f.Truthy("provider") || !f.Truthy("model") {
		return httpapi.BadRequest(w, "provider and model are required")
	}
	provider, err := f.RequireString("provider")
	if err != nil {
		return err
	}
	model, err := f.RequireString("model")
	if err != nil {
		return err
	}
	defaults, err := providers.GetDefaultConfiguration(provider, model)
	if err != nil {
		return err
	}
	out := jsutil.NewObject()
	out.Set("defaults", defaults)
	return writeJS(w, out)
}

// openRouterModels is POST /admin/providers/openrouter/models: the
// OpenRouter catalog (fetched when the cache is cold) as a list for the UI.
func (rt *Routes) openRouterModels(w http.ResponseWriter, r *http.Request) error {
	catalog := rt.Providers.FetchCatalog(r.Context())
	models := []any{}
	for _, e := range catalog.Entries() {
		pricing := jsutil.NewObject()
		pricing.Set("input", e.Input)
		pricing.Set("output", e.Output)
		if e.CacheRead != nil {
			pricing.Set("cache_read", *e.CacheRead)
		} else {
			pricing.Set("cache_read", nil)
		}
		m := jsutil.NewObject()
		m.Set("id", e.ID)
		m.Set("name", e.Name)
		m.Set("pricing", pricing)
		m.Set("context_length", e.ContextLength)
		models = append(models, m)
	}
	out := jsutil.NewObject()
	out.Set("models", models)
	return writeJS(w, out)
}
