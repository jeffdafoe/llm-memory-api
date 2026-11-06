package virtualagent

import (
	"context"
	"errors"
	"math"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// The rate limiter (Node's callHistory and countedScenes). Times are
// milliseconds as float64, and the limits are parseInt results, so a
// garbage setting is NaN and every comparison with it is false, as in Node.
//
// A salem tick makes several provider calls under one sceneId. Only the
// first call of a sceneId counts toward the limit, and the later calls of
// that tick bypass the limit and the cooldown, so the limit bounds ticks and
// a tool-use exchange is never cut off mid-tick (ZBBS-HOME-332). Calls with
// no sceneId count one by one.
type limiter struct {
	mu      sync.Mutex
	history map[string]*callHistory
	// scenes: agent -> sceneId -> when it was first counted.
	scenes map[string]map[string]float64
}

type callHistory struct {
	calls []float64
	// cooldownUntil is Node's history._cooldownUntil; it is set when
	// truthy (not 0, not NaN).
	cooldownUntil float64
}

func cooldownSet(x float64) bool {
	return x != 0 && !math.IsNaN(x)
}

func (l *limiter) historyFor(agent string) *callHistory {
	if l.history == nil {
		l.history = map[string]*callHistory{}
	}
	h := l.history[agent]
	if h == nil {
		h = &callHistory{}
		l.history[agent] = h
	}
	return h
}

// pruneScenes drops sceneIds first counted before cutoff, and the agent's
// map once it is empty.
func (l *limiter) pruneScenes(agent string, cutoff float64) {
	scenes := l.scenes[agent]
	if scenes == nil {
		return
	}
	for id, first := range scenes {
		if first < cutoff {
			delete(scenes, id)
		}
	}
	if len(scenes) == 0 {
		delete(l.scenes, agent)
	}
}

// RateLimit is the limit the limiter enforces for an agent: calls per
// window, and the cooldown after the limit trips, in milliseconds.
type RateLimit struct {
	Limit      float64
	WindowMs   float64
	CooldownMs float64
}

// rateOverrides is an agent's own rate_limit / rate_window_seconds /
// cooldown_seconds; nil is not set. A set value may be NaN.
type rateOverrides struct {
	limit, windowMs, cooldownMs *float64
}

type overrideEntry struct {
	at        float64
	overrides *rateOverrides
}

const overrideCacheTTL = 60 * 1000

// mergeRateLimit is virtual-agent-rate-limit.js: each override that is set
// replaces its global.
func mergeRateLimit(globals RateLimit, o *rateOverrides) RateLimit {
	out := globals
	if o != nil {
		if o.limit != nil {
			out.Limit = *o.limit
		}
		if o.windowMs != nil {
			out.WindowMs = *o.windowMs
		}
		if o.cooldownMs != nil {
			out.CooldownMs = *o.cooldownMs
		}
	}
	return out
}

// parseIntValue is parseInt(v) for a JSON value: String(v), then parseInt.
func parseIntValue(v any) (float64, error) {
	s, err := jsutil.ValueString(v)
	if err != nil {
		return 0, err
	}
	return parseIntJS(s), nil
}

// parseIntJS is parseInt(s), NaN where JavaScript gives NaN.
func parseIntJS(s string) float64 {
	n, ok := jsutil.ParseInt(s)
	if !ok {
		return math.NaN()
	}
	return n
}

// configInt is parseInt(config.get(key)).
func (s *Service) configInt(key string) (float64, error) {
	v, err := s.Config.Get(key)
	if err != nil {
		return 0, err
	}
	return parseIntJS(v), nil
}

func isNullish(v any) bool {
	return v == nil || v == jsutil.Undefined
}

// rateLimitOverrides reads the agent's overrides from its configuration
// JSON, cached for 60 seconds. A failure is logged and caches no overrides.
func (s *Service) rateLimitOverrides(ctx context.Context, agentName string) *rateOverrides {
	now := s.nowMs()
	s.overrideMu.Lock()
	if e, ok := s.overrideCache[agentName]; ok && now-e.at < overrideCacheTTL {
		s.overrideMu.Unlock()
		return e.overrides
	}
	s.overrideMu.Unlock()

	overrides, err := s.readRateLimitOverrides(ctx, agentName)
	if err != nil {
		s.logVA("rate-limit-override-read-failed", logger.Fields{{Key: "agent", Value: agentName}, {Key: "error", Value: err.Error()}})
		overrides = nil
	}
	s.overrideMu.Lock()
	if s.overrideCache == nil {
		s.overrideCache = map[string]overrideEntry{}
	}
	s.overrideCache[agentName] = overrideEntry{at: s.nowMs(), overrides: overrides}
	s.overrideMu.Unlock()
	return overrides
}

func (s *Service) readRateLimitOverrides(ctx context.Context, agentName string) (*rateOverrides, error) {
	var raw *string
	err := s.DB.QueryRow(ctx,
		`SELECT ac.configuration FROM agent_configuration ac
             JOIN actors a ON a.id = ac.actor_id
             WHERE a.name = $1`, agentName).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if raw == nil || *raw == "" {
		return nil, nil
	}
	cfg, err := jsutil.Parse([]byte(*raw))
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("Cannot read properties of null (reading 'rate_limit')")
	}
	member := func(k string) any {
		if o, ok := cfg.(*jsutil.Object); ok {
			return o.Value(k)
		}
		return jsutil.Undefined
	}
	limit, window, cooldown := member("rate_limit"), member("rate_window_seconds"), member("cooldown_seconds")
	if isNullish(limit) && isNullish(window) && isNullish(cooldown) {
		return nil, nil
	}
	o := &rateOverrides{}
	if !isNullish(limit) {
		n, err := parseIntValue(limit)
		if err != nil {
			return nil, err
		}
		o.limit = &n
	}
	if !isNullish(window) {
		n, err := parseIntValue(window)
		if err != nil {
			return nil, err
		}
		n *= 1000
		o.windowMs = &n
	}
	if !isNullish(cooldown) {
		n, err := parseIntValue(cooldown)
		if err != nil {
			return nil, err
		}
		n *= 1000
		o.cooldownMs = &n
	}
	return o, nil
}

// EffectiveRateLimit is effectiveRateLimit: the config-table limits with the
// agent's overrides applied. The /agent/rate-limit route reports these same
// numbers, so the engine paces against what is enforced.
func (s *Service) EffectiveRateLimit(ctx context.Context, agentName string) (RateLimit, error) {
	limit, err := s.configInt("virtual_agent_rate_limit")
	if err != nil {
		return RateLimit{}, err
	}
	window, err := s.configInt("virtual_agent_rate_window_seconds")
	if err != nil {
		return RateLimit{}, err
	}
	cooldown, err := s.configInt("virtual_agent_cooldown_seconds")
	if err != nil {
		return RateLimit{}, err
	}
	globals := RateLimit{Limit: limit, WindowMs: window * 1000, CooldownMs: cooldown * 1000}
	return mergeRateLimit(globals, s.rateLimitOverrides(ctx, agentName)), nil
}

// IsRateLimited is isRateLimited: whether a call should be refused. A call
// whose sceneId was already counted in the window is let through.
func (s *Service) IsRateLimited(ctx context.Context, agentName, sceneID string) (bool, error) {
	rl, err := s.EffectiveRateLimit(ctx, agentName)
	if err != nil {
		return false, err
	}
	l := &s.limiter
	l.mu.Lock()
	defer l.mu.Unlock()

	now := s.nowMs()
	cutoff := now - rl.WindowMs
	h := l.historyFor(agentName)
	for len(h.calls) > 0 && h.calls[0] < cutoff {
		h.calls = h.calls[1:]
	}
	l.pruneScenes(agentName, cutoff)

	if sceneID != "" {
		if _, ok := l.scenes[agentName][sceneID]; ok {
			return false, nil
		}
	}
	if cooldownSet(h.cooldownUntil) && now < h.cooldownUntil {
		s.logVA("rate-limited", logger.Fields{
			{Key: "agent", Value: agentName},
			{Key: "reason", Value: "cooldown"},
			{Key: "resumesIn", Value: jsutil.NumberString(math.Ceil((h.cooldownUntil-now)/1000)) + "s"},
		})
		return true, nil
	}
	if cooldownSet(h.cooldownUntil) && now >= h.cooldownUntil {
		h.cooldownUntil = 0
	}
	if float64(len(h.calls)) >= rl.Limit {
		h.cooldownUntil = now + rl.CooldownMs
		s.logVA("rate-limit-triggered", logger.Fields{
			{Key: "agent", Value: agentName},
			{Key: "calls", Value: len(h.calls)},
			{Key: "windowSeconds", Value: jsNumber(rl.WindowMs / 1000)},
			{Key: "cooldownSeconds", Value: jsNumber(rl.CooldownMs / 1000)},
		})
		return true, nil
	}
	return false, nil
}

// RateLimitResumeSeconds is the seconds until the agent's cooldown lifts,
// 0 when none is active.
func (s *Service) RateLimitResumeSeconds(agentName string) float64 {
	l := &s.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.history[agentName]
	if h == nil || !cooldownSet(h.cooldownUntil) {
		return 0
	}
	return math.Max(0, math.Ceil((h.cooldownUntil-s.nowMs())/1000))
}

// RecordCall is recordCall: one provider call counted toward the limit.
// With a sceneId only the first call of that scene counts. The window for
// the scene prune is the global one, as in Node.
func (s *Service) RecordCall(agentName, sceneID string) error {
	var window float64
	if sceneID != "" {
		w, err := s.configInt("virtual_agent_rate_window_seconds")
		if err != nil {
			return err
		}
		window = w * 1000
	}
	l := &s.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	now := s.nowMs()
	h := l.historyFor(agentName)
	if sceneID != "" {
		l.pruneScenes(agentName, now-window)
		if l.scenes == nil {
			l.scenes = map[string]map[string]float64{}
		}
		if l.scenes[agentName] == nil {
			l.scenes[agentName] = map[string]float64{}
		}
		if _, ok := l.scenes[agentName][sceneID]; ok {
			return nil
		}
		l.scenes[agentName][sceneID] = now
	}
	h.calls = append(h.calls, now)
	return nil
}

// jsNumber is a number as JSON.stringify writes it: NaN and the
// infinities are null.
func jsNumber(x float64) any {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return nil
	}
	return x
}
