package notes

import (
	"context"
	"errors"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
)

// Enricher classifies a saved note's cognitive type (semantic, episodic,
// procedural, reflective), which drives the per-type decay in search. Save
// runs it in the background; an error goes to system_errors as
// NOTE_ENRICHMENT_FAILED.
type Enricher interface {
	Enrich(ctx context.Context, namespace, slug, title, content string, metadata []byte) error
}

// GatedEnricher applies Node's on-off gate: nothing happens unless config
// note_enrichment_enabled is "true" (a missing key is off). With the gate
// on it returns an error, because the classification calls a virtual agent
// through the LLM providers, which this server does not run yet; the error
// makes the gap visible in system_errors instead of silent.
type GatedEnricher struct {
	Config *config.Store
}

// Enrich is a no-op while enrichment is off.
func (g *GatedEnricher) Enrich(ctx context.Context, namespace, slug, title, content string, metadata []byte) error {
	if v, ok := g.Config.Lookup("note_enrichment_enabled"); !ok || v != "true" {
		return nil
	}
	return errors.New("note enrichment needs the virtual-agent providers, which the Go server does not have")
}
