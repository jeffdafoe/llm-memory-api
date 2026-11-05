package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apibase"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// EmbeddingModel is the OpenAI model every stored chunk was embedded with.
// A search must use the same model, or the vectors do not compare.
const EmbeddingModel = "text-embedding-3-small"

// Embedder turns texts into vectors.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float64, error)
}

// OpenAI calls POST <OPENAI_API_BASE>/v1/embeddings.
type OpenAI struct {
	Client *http.Client
}

// Embed returns one vector per text, in order. The values are kept as
// float64, as JSON.parse read them, so VectorLiteral writes the same text
// Node sent to Postgres.
func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	base, err := apibase.Base("OPENAI_API_BASE")
	if err != nil {
		return nil, err
	}
	body, err := jsonutil.Marshal(struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{EmbeddingModel, texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Node interpolated process.env.OPENAI_API_KEY, which is "undefined"
	// when the variable is not set.
	key, ok := os.LookupEnv("OPENAI_API_KEY")
	if !ok {
		key = "undefined"
	}
	req.Header.Set("Authorization", "Bearer "+key)

	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("OpenAI API error %d: %s", resp.StatusCode, data)
	}
	var parsed struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	if parsed.Data == nil {
		return nil, fmt.Errorf("OpenAI API response has no data")
	}
	out := make([][]float64, len(parsed.Data))
	for i, d := range parsed.Data {
		out[i] = d.Embedding
	}
	return out, nil
}

// VectorLiteral is pgvector.toSql: the vector as JSON.stringify writes the
// array, which Postgres parses into float4 values. Sending this text, not a
// float32 vector, keeps the rounding Node had (double to shortest decimal to
// float4).
func VectorLiteral(v []float64) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = jsutil.NumberString(x)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
