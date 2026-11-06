package virtualagent

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/events"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/mail"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
)

// Config is the config table (config.Store).
type Config interface {
	Get(key string) (string, error)
}

// Logger writes activity lines and error_log rows (logger.Logger).
type Logger interface {
	Log(subsystem, action string, details logger.Fields)
	LogError(subsystem, action string, opts logger.ErrorOpts)
}

// Actors resolves agent names (actors.Cache).
type Actors interface {
	RequireByName(ctx context.Context, name string) (actors.Actor, error)
}

// NoteStore saves and reads notes (notes.Service).
type NoteStore interface {
	Save(ctx context.Context, in notes.SaveInput) (notes.SavedNote, error)
	Read(ctx context.Context, namespace, slug string) (notes.Note, error)
}

// Searcher searches notes (memory.Service), for the recalled notes in a
// prompt.
type Searcher interface {
	Search(ctx context.Context, in memory.SearchInput) (memory.SearchResults, error)
}

// Mailer sends mail (mail.Service): a virtual agent's replies and notices.
type Mailer interface {
	Send(ctx context.Context, toAgent, fromAgent, subject, body, inReplyTo string) (*mail.Sent, error)
}

// Providers builds provider calls (providers.Providers).
type Providers interface {
	CreateProvider(provider, model, apiKey string, configuration any) (providers.Call, error)
	DecryptAPIKey(encrypted string) (string, error)
}

// Service is Node's services/virtual-agent.js: the API-backed agents.
// The rate limiter, its per-agent override cache and the error-ping timer
// are in memory, as in Node; they reset on restart.
type Service struct {
	DB        db.DB
	Config    Config
	Actors    Actors
	Log       Logger
	Events    events.Broadcaster
	Providers Providers
	Notes     NoteStore
	Memory    Searcher
	Mail      Mailer
	// Chat sends the replies and notices of direct chat and discussions.
	Chat ChatSender
	// Discussions evaluates a virtual participant's ballot and takes it out
	// of a discussion after a failure.
	Discussions Discussions

	// Now is the clock; tests replace it.
	Now func() time.Time
	// Sleep replaces the timer for setTimeout(fn, ms) waits (the retry
	// backoff, the activity spinner's off-switch); it gets Node's ms as
	// given. Tests replace it.
	Sleep func(ctx context.Context, ms float64) error

	limiter       limiter
	overrideMu    sync.Mutex
	overrideCache map[string]overrideEntry
	pingOnce      sync.Once
	sceneCache    sceneCache
	// inFlight holds the discussion generations running now, by
	// "<discussion>:<agent>" (Node's inFlightVA).
	inFlightMu sync.Mutex
	inFlight   map[string]*inFlight
	// beforeRerunCheck runs after a discussion generation, before its rerun
	// check; tests set it.
	beforeRerunCheck func(key string)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// nowMs is Date.now().
func (s *Service) nowMs() float64 {
	return float64(s.now().UnixMilli())
}

// setTimeout waits as setTimeout(fn, ms) would: under 1 ms, over 2^31-1 ms
// or NaN waits 1 ms.
func (s *Service) setTimeout(ctx context.Context, ms float64) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, ms)
	}
	t := time.NewTimer(timerDelay(ms))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) broadcast(event string, data any) {
	if s.Events != nil {
		s.Events.Broadcast(event, data)
	}
}

func (s *Service) logVA(action string, details logger.Fields) {
	s.Log.Log("virtual-agent", action, details)
}

// Agent is loadAgent's row: an agent with its virtual-agent settings.
// Numeric columns are their text, as node-postgres returned them.
type Agent struct {
	ActorID             int
	Name                string
	Virtual             bool
	Provider            *string
	Model               *string
	APIKey              *string
	Configuration       *string
	StartupInstructions *string
	Personality         *string
	Expertise           []byte
	CostBudgetDaily     *string
	CostBudgetMonthly   *string
	CachePrompts        bool
	LearningEnabled     bool
	MaxTokens           *int
	Temperature         *string
}

// str is a nullable text column as JavaScript read it: "" for NULL.
func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// LoadAgent is loadAgent: the agent's row, or nil when it has no
// agent_configuration row.
func (s *Service) LoadAgent(ctx context.Context, name string) (*Agent, error) {
	var a Agent
	err := s.DB.QueryRow(ctx,
		`SELECT ac.id AS actor_id, ac.name AS agent, agc.virtual, agc.provider, agc.model, agc.api_key, agc.configuration,
                agc.startup_instructions, agc.personality, ac.expertise, agc.cost_budget_daily::text, agc.cost_budget_monthly::text,
                agc.cache_prompts, agc.learning_enabled, agc.max_tokens, agc.temperature::text
         FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE ac.name = $1`, name).Scan(
		&a.ActorID, &a.Name, &a.Virtual, &a.Provider, &a.Model, &a.APIKey, &a.Configuration,
		&a.StartupInstructions, &a.Personality, &a.Expertise, &a.CostBudgetDaily, &a.CostBudgetMonthly,
		&a.CachePrompts, &a.LearningEnabled, &a.MaxTokens, &a.Temperature)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// setAgentStatus sets a virtual agent's status (available, degraded, error).
func (s *Service) setAgentStatus(ctx context.Context, agentName, status string) error {
	if _, err := s.DB.Exec(ctx, "UPDATE actors SET status = $1 WHERE name = $2", status, agentName); err != nil {
		return err
	}
	s.broadcast("agent_activity", logger.Fields{{Key: "agent", Value: agentName}, {Key: "status", Value: status}})
	s.logVA("status-change", logger.Fields{{Key: "agent", Value: agentName}, {Key: "status", Value: status}})
	return nil
}

// minActivityMs is how long the activity spinner stays on at least.
const minActivityMs = 3000

// WithActivity is withActivityIndicator: fn runs with the agent's activity
// spinner on (active_since set, an agent_activity event), and the spinner
// goes off at least minActivity after it started, in the background.
func WithActivity[T any](ctx context.Context, s *Service, agentName string, fn func() (T, error)) (T, error) {
	var zero T
	start := s.nowMs()
	actor, err := s.Actors.RequireByName(ctx, agentName)
	if err != nil {
		return zero, err
	}
	if _, err := s.DB.Exec(ctx, "UPDATE actors SET active_since = NOW(), last_seen = NOW() WHERE id = $1", actor.ID); err != nil {
		return zero, err
	}
	s.broadcast("agent_activity", logger.Fields{{Key: "agent", Value: agentName}, {Key: "active", Value: true}})
	defer func() {
		remaining := math.Max(0, minActivityMs-(s.nowMs()-start))
		db.Background(func(ctx context.Context) {
			if s.setTimeout(ctx, remaining) != nil {
				return
			}
			s.DB.Exec(ctx, "UPDATE actors SET active_since = NULL, last_seen = NOW() WHERE id = $1", actor.ID)
			s.broadcast("agent_activity", logger.Fields{{Key: "agent", Value: agentName}, {Key: "active", Value: false}})
		})
	}()
	return fn()
}
