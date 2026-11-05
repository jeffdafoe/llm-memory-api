package memory

import (
	"fmt"
	"math"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// SearchInput is one search request.
type SearchInput struct {
	Query string
	// Namespace "" or "*" searches every namespace the caller may read.
	Namespace string
	// Limit is Number(limit) as the caller sent it; NaN when it was not
	// sent. It becomes max(1, floor(limit) || 5).
	Limit float64
	// Readable filters a search across namespaces. FilterReadable false means
	// no filter: the caller reads every namespace, or a namespace was given.
	Readable       []string
	FilterReadable bool
	// ActorID adds notes shared with the actor through note_permissions to a
	// filtered search. 0 means none.
	ActorID int
	// SlugPrefix narrows the search to source files under the prefix. ""
	// means no filter.
	SlugPrefix string
}

func (in SearchInput) wildcard() bool {
	return in.Namespace == "" || in.Namespace == "*"
}

// searchConfig holds the config values a search reads. Each key must exist
// in the config table (Node's config.get threw on a missing key).
type searchConfig struct {
	poolMultiplier     float64
	filenameBoost      float64
	bm25Boost          float64
	minSimilarity      float64
	halfLives          []namedValue
	cognitiveHalfLives []namedValue
	accessBoostMax     float64
	accessWindowDays   float64
	conversationWeight float64
	dreamWeight        float64
}

type namedValue struct {
	name  string
	value float64
}

// Kinds and cognitive types in the order their decay cases are written.
var (
	decayKinds     = []string{"task", "learning", "note", "reference", "instruction", "conversation", "dream"}
	cognitiveTypes = []string{"semantic", "episodic", "procedural", "reflective"}
	// Fallbacks for cognitive half-lives when the value does not parse.
	cognitiveFallbacks = map[string]float64{"episodic": 90, "reflective": 180}
)

func loadSearchConfig(store *config.Store) (searchConfig, error) {
	var firstErr error
	num := func(key string, fallback float64) float64 {
		v, err := store.Get(key)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return fallback
		}
		return config.ParseNonNegativeFinite(v, true, fallback)
	}
	c := searchConfig{
		poolMultiplier: math.Min(num("search_pool_multiplier", 3), 10),
		filenameBoost:  num("search_filename_boost", 0.15),
		bm25Boost:      num("search_bm25_boost", 0.1),
		minSimilarity:  num("search_min_similarity", 0),
	}
	for _, k := range decayKinds {
		c.halfLives = append(c.halfLives, namedValue{k, num("search_decay_halflife_"+k, 0)})
	}
	for _, t := range cognitiveTypes {
		c.cognitiveHalfLives = append(c.cognitiveHalfLives, namedValue{t, num("search_decay_halflife_"+t, cognitiveFallbacks[t])})
	}
	c.accessBoostMax = num("search_access_boost_max", 0)
	c.accessWindowDays = num("search_access_boost_window_days", 0)
	c.conversationWeight = num("search_conversation_weight", 0.7)
	c.dreamWeight = num("search_dream_weight", 1.0)
	return c, firstErr
}

// maxResults is max(1, floor(limit) || 5).
func maxResults(limit float64) float64 {
	return math.Max(1, jsutil.FloorOr(limit, 5))
}

// buildSearch writes the search SQL and its parameters, in the same order
// and with the same placeholders as Node's searchMemory. Every config value
// is a bound parameter. Numbers go as jsutil.Number, the text node-postgres
// sent. cleaned is the preprocessed query and vector its embedding literal.
//
// The inner select scores each chunk: vector similarity plus the filename,
// BM25 and access boosts, times the decay and the kind weight. The outer
// select keeps one row per note (its best chunk) with the note's chunk count.
func buildSearch(in SearchInput, cfg searchConfig, cleaned, vector string) (string, []any) {
	limit := maxResults(in.Limit)
	// Fetch a larger candidate pool from pgvector, then let the boosts
	// reorder it before the final trim.
	poolSize := math.Min(math.Max(limit, jsutil.Round(limit*cfg.poolMultiplier)), 500)

	var params []any
	add := func(v any) string {
		params = append(params, v)
		return fmt.Sprintf("$%d", len(params))
	}

	add(vector)
	if !in.wildcard() {
		add(in.Namespace)
	}
	poolIdx := add(jsutil.Num(poolSize))

	// Words of three or more characters, for the filename boost and the
	// BM25 gate.
	var words []string
	for _, w := range jsutil.SplitSpace(jsutil.ToLower(cleaned)) {
		if jsutil.UTF16Len(w) > 2 {
			words = append(words, w)
		}
	}

	var clauses []string
	for _, w := range words {
		clauses = append(clauses, "mc.source_file ILIKE "+add("%"+w+"%"))
	}
	fnBoostIdx := add(jsutil.Num(cfg.filenameBoost))
	filenameBoost := "0"
	if len(clauses) > 0 {
		filenameBoost = "CASE WHEN " + strings.Join(clauses, " OR ") + " THEN " + fnBoostIdx + "::numeric ELSE 0.0 END"
	}

	// BM25 boost, only for chunks whose vector similarity is above 0.3, so
	// keyword-heavy but unrelated chunks are not lifted.
	bm25Boost := "0"
	if len(words) > 0 {
		tsqIdx := add(cleaned)
		scaleIdx := add(jsutil.Num(cfg.bm25Boost))
		bm25Boost = `CASE WHEN (1 - (mc.embedding <=> $1)) > 0.3 AND mc.tsv IS NOT NULL
            THEN ` + scaleIdx + `::numeric * ts_rank(mc.tsv, plainto_tsquery('english', ` + tsqIdx + `))
            ELSE 0.0 END`
	}

	// Across namespaces: the readable namespaces, plus notes shared with the
	// actor through note_permissions.
	nsFilter := ""
	if in.wildcard() && in.FilterReadable {
		nsIdx := add(nonNil(in.Readable))
		if in.ActorID != 0 {
			actorIdx := add(in.ActorID)
			nsFilter = `AND (mc.namespace = ANY(` + nsIdx + `) OR EXISTS (
                SELECT 1 FROM note_permissions np
                WHERE np.owner_namespace = mc.namespace
                  AND (np.slug_pattern = mc.source_file OR mc.source_file LIKE np.slug_pattern || '%')
                  AND (np.grantee_actor_id = ` + actorIdx + ` OR np.grantee_actor_id IS NULL)
                  AND np.revoked_at IS NULL AND np.can_read = true
            ))`
		} else {
			nsFilter = "AND mc.namespace = ANY(" + nsIdx + ")"
		}
	}

	// The prefix is matched case-insensitively and literally; the appended
	// "%" is the one wildcard. Both inner selects take the same filter.
	slugFilter := ""
	if in.SlugPrefix != "" {
		slugIdx := add(EscapeLikePattern(jsutil.ToLower(in.SlugPrefix)) + "%")
		slugFilter = "AND LOWER(mc.source_file) LIKE " + slugIdx + ` ESCAPE '\'`
	}

	// Decay: 0.5 ^ (age_days / half_life), where age runs from the latest of
	// created_at, updated_at and last_accessed, so a note that keeps being
	// found stays fresh. A cognitive type (from enrichment) takes priority
	// over the kind. A half-life of 0 means no decay.
	const age = `EXTRACT(EPOCH FROM (NOW() - GREATEST(d.created_at, COALESCE(d.updated_at, d.created_at), COALESCE(d.last_accessed, d.created_at)))) / 86400.0 / `
	var cases []string
	for _, c := range cfg.cognitiveHalfLives {
		when := "WHEN LOWER(TRIM(d.metadata->>'cognitive_type')) = '" + c.name + "' THEN "
		if c.value == 0 {
			cases = append(cases, when+"1.0")
			continue
		}
		cases = append(cases, when+"POWER(0.5, "+age+add(jsutil.Num(c.value))+"::numeric)")
	}
	for _, k := range cfg.halfLives {
		when := "WHEN d.kind = '" + k.name + "' THEN "
		if k.value == 0 {
			cases = append(cases, when+"1.0")
			continue
		}
		cases = append(cases, when+"POWER(0.5, "+age+add(jsutil.Num(k.value))+"::numeric)")
	}
	decay := "CASE " + strings.Join(cases, " ") + " ELSE 1.0 END"

	convIdx := add(jsutil.Num(cfg.conversationWeight))
	dreamIdx := add(jsutil.Num(cfg.dreamWeight))
	kindWeight := "CASE WHEN d.kind = 'conversation' THEN " + convIdx + "::numeric WHEN d.kind = 'dream' THEN " + dreamIdx + "::numeric ELSE 1.0 END"

	// Access boost: falls linearly from the max to 0 over the window.
	accessBoost := "0"
	windowSeconds := cfg.accessWindowDays * 86400
	if cfg.accessBoostMax > 0 && windowSeconds > 0 {
		maxIdx := add(jsutil.Num(cfg.accessBoostMax))
		winIdx := add(jsutil.Num(windowSeconds))
		accessBoost = `
            CASE WHEN d.last_accessed IS NOT NULL
                AND EXTRACT(EPOCH FROM (NOW() - d.last_accessed)) < ` + winIdx + `
            THEN ` + maxIdx + `::numeric * (1.0 - EXTRACT(EPOCH FROM (NOW() - d.last_accessed)) / ` + winIdx + `::numeric)
            ELSE 0.0 END`
	}

	// d.id IS NULL is a raw ingest with no document row, which is kept. The
	// deleted_at test must stay in WHERE: in the ON clause a deleted document
	// would look like a raw ingest and be kept.
	const softDelete = "AND (d.id IS NULL OR d.deleted_at IS NULL)"

	// created_at, not the decay age: last_accessed is stamped by every
	// search hit, so an age based on it would make every recalled memory
	// look new. NULL for raw ingests.
	score := `SELECT mc.source_file, mc.heading, mc.chunk_text, mc.namespace, d.created_at,
                   ((1 - (mc.embedding <=> $1)) + ` + filenameBoost + ` + ` + bm25Boost + ` + ` + accessBoost + `)
                   * ` + decay + ` * ` + kindWeight + ` AS similarity
            FROM memory_chunks mc
            LEFT JOIN documents d ON d.namespace = mc.namespace AND LOWER(d.slug) = LOWER(mc.source_file)`
	var inner string
	if in.wildcard() {
		inner = score + `
            WHERE 1=1 ` + nsFilter + ` ` + slugFilter + ` ` + softDelete + `
            ORDER BY similarity DESC
            LIMIT ` + poolIdx
	} else {
		inner = score + `
            WHERE mc.namespace = $2 ` + slugFilter + ` ` + softDelete + `
            ORDER BY similarity DESC
            LIMIT ` + poolIdx
	}

	finalIdx := add(jsutil.Num(limit))

	// Relevance floor on the composite score: below it a hit is dropped
	// instead of filling the top k. Off at 0.
	floor := ""
	if cfg.minSimilarity > 0 {
		floor = " AND similarity >= " + add(jsutil.Num(cfg.minSimilarity)) + "::numeric"
	}

	sql := `
        SELECT source_file, heading, chunk_text, namespace, created_at, similarity, chunk_count
        FROM (
            SELECT source_file, heading, chunk_text, namespace, created_at, similarity,
                   ROW_NUMBER() OVER (PARTITION BY namespace, source_file ORDER BY similarity DESC) AS rn,
                   COUNT(*) OVER (PARTITION BY namespace, source_file) AS chunk_count
            FROM (
            ` + inner + `
        ) candidates
        ) ranked
        WHERE rn = 1` + floor + `
        ORDER BY similarity DESC
        LIMIT ` + finalIdx + `
    `
	return sql, params
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
