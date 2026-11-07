package notes

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Enricher classifies a saved note's cognitive type (semantic, episodic,
// procedural, reflective), which drives the per-type decay in search
// (enrichment.Enricher). Save runs it in the background; an error goes to
// system_errors as NOTE_ENRICHMENT_FAILED. metadata is the note's metadata
// as EnrichMetadata gives it: the JSON text of the parsed value, nil for
// none.
type Enricher interface {
	Enrich(ctx context.Context, namespace, slug, title, content string, metadata []byte) error
}

// EnrichMetadata is what save and the admin reindex hand to the enricher,
// Node's
// `doc.metadata ? (typeof doc.metadata === 'object' ? doc.metadata : JSON.parse(doc.metadata)) : null`
// over the jsonb text: a falsy value is null, an object or array passes, and
// any other value is given to JSON.parse (a JSON string its contents, a
// number or boolean its own text), which may fail. A jsonb number past the
// float64 range was Infinity to node-postgres, and JSON.parse("Infinity")
// throws.
func EnrichMetadata(raw []byte) ([]byte, error) {
	v, err := jsutil.Parse(raw)
	if err != nil || raw == nil || !jsutil.Truthy(v) {
		return nil, nil
	}
	switch x := v.(type) {
	case string:
		if !json.Valid([]byte(x)) {
			var probe any
			return nil, json.Unmarshal([]byte(x), &probe)
		}
		return []byte(x), nil
	case json.Number:
		// V8's two SyntaxError texts for JSON.parse(String(±Infinity)).
		if f, _ := jsutil.AsFloat(x); math.IsInf(f, 1) {
			return nil, errors.New(`"Infinity" is not valid JSON`)
		} else if math.IsInf(f, -1) {
			return nil, errors.New("No number after minus sign in JSON at position 1 (line 1 column 2)")
		}
		return raw, nil
	default:
		return raw, nil
	}
}
