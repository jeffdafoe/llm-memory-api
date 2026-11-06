// Package simapi is the /v1/sim routes (Node's routes/sim.js) and the
// services behind them: the daily sim-conversation distiller
// (sim-conversation-distiller.js) and shared-NPC soul synthesis
// (sim-soul.js). The pure helpers are in internal/sim.
package simapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/dream"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sim"
)

// Logger is the part of *logger.Logger the services use.
type Logger interface {
	Log(subsystem, action string, details logger.Fields)
	LogError(subsystem, action string, opts logger.ErrorOpts)
	Errorf(format string, args ...any)
}

// Notes saves notes (notes.Service).
type Notes interface {
	Save(ctx context.Context, in notes.SaveInput) (notes.SavedNote, error)
}

// Service holds the /sim services.
type Service struct {
	DB     db.DB
	Log    Logger
	Notes  Notes
	Agents dream.Agents
	Trust  dream.Trust

	unmapped     sim.UnmappedKinds
	unmappedOnce sync.Once
}

// unmappedKinds logs each action type with no narration once.
func (s *Service) unmappedKinds() *sim.UnmappedKinds {
	s.unmappedOnce.Do(func() {
		s.unmapped.Log = func(kind string) {
			s.Log.Log("sim-distill", "unmapped-kind", logger.Fields{{Key: "kind", Value: kind}})
		}
	})
	return &s.unmapped
}

var dayPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func badRequest(msg string) error { return apierr.New(http.StatusBadRequest, msg) }

// DistillInput is the /sim/conversation-day body: the agent (already
// sanitized; AgentIsString false when the field was not a string), the
// day, the events and, for a shared-VA push, slug_prefix and actor, as
// parsed JSON values (jsutil).
type DistillInput struct {
	Agent         string
	AgentIsString bool
	Day           any
	Events        any
	SlugPrefix    any
	Actor         any
}

type narrationLine struct {
	atMs float64
	seq  int
	line string
}

// weekdays index time.Weekday.
var weekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

// formatTimestamp is "Weekday HH:MM" of the instant in UTC, or "??:??".
func formatTimestamp(v any) (string, error) {
	ms, ok, err := jsutil.DateValue(v, time.Local)
	if err != nil {
		return "", err
	}
	if !ok {
		return "??:??", nil
	}
	t := time.UnixMilli(int64(ms)).UTC()
	return weekdays[t.Weekday()] + " " + t.Format("15:04"), nil
}

// Distill is distillSimConversationDay: one sim NPC's in-sim day, pushed
// by the engine, written as conversations/<day>-sim-day ("[Weekday HH:MM
// Speaker] text" lines) in the agent's namespace, under the villager's
// prefix for a shared-VA push. Re-pushing a day overwrites its note. A
// shared-VA push also enrolls the villager in sim_shared_actor, the dream
// roster.
func (s *Service) Distill(ctx context.Context, in DistillInput) (logger.Fields, error) {
	if !in.AgentIsString || in.Agent == "" {
		return nil, badRequest("agent required")
	}
	day, isString := in.Day.(string)
	if !isString || day == "" || !dayPattern.MatchString(day) {
		return nil, badRequest("day required (YYYY-MM-DD)")
	}
	events, isArray := in.Events.([]any)
	if !isArray {
		return nil, badRequest("events must be an array")
	}

	var actorID int
	var name, mode string
	err := s.DB.QueryRow(ctx, `SELECT ac.id, ac.name, agc.dream_mode
         FROM actors ac
         JOIN agent_configuration agc ON agc.actor_id = ac.id
         WHERE ac.name = $1`, in.Agent).Scan(&actorID, &name, &mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.New(http.StatusNotFound, "unknown agent: "+in.Agent)
	}
	if err != nil {
		return nil, err
	}
	if mode != "sim" && mode != "sim-shared" {
		return nil, badRequest("agent " + in.Agent + " is not configured for a sim dream_mode")
	}

	// A sim-shared push is one villager of the pool: its note goes under
	// the villager's prefix and its lines carry the villager's name. A
	// dedicated sim VA is one NPC per namespace, labeled from its slug.
	shared := mode == "sim-shared"
	slugPrefix := ""
	var actorName string
	if shared {
		if raw, ok := in.SlugPrefix.(string); ok {
			slugPrefix = sim.NormalizeSlugPrefix(raw)
		}
		if slugPrefix == "" {
			return nil, badRequest("slug_prefix required (safe kebab path) for a sim-shared agent")
		}
		display := ""
		if raw, ok := in.Actor.(string); ok {
			display = jsutil.Trim(raw)
		}
		if display == "" {
			return nil, badRequest("actor (display name) required for a sim-shared agent")
		}
		actorName, err = sim.SanitizeLabel(display)
		if err != nil {
			return nil, err
		}
		if actorName == "" || jsutil.UTF16Len(actorName) > 100 {
			return nil, badRequest("actor (display name) is empty after sanitizing, or too long, for a sim-shared agent")
		}
	} else {
		actorName, err = sim.SanitizeLabel(sim.SlugToDisplay(name))
		if err != nil {
			return nil, err
		}
	}

	// The day's UTC window; a date V8 rolls over (2026-02-31) is refused
	// rather than distilled under the wrong slug.
	startMs, ok := jsutil.ParseDate(day+"T00:00:00.000Z", time.UTC)
	if !ok || jsutil.DateISO(time.UnixMilli(int64(startMs)))[:10] != day {
		return nil, badRequest("invalid day: " + day)
	}
	endMs := startMs + 24*60*60*1000

	var lines []narrationLine
	seq := 0
	for _, raw := range events {
		event, isObject := raw.(*jsutil.Object)
		if !isObject {
			continue
		}
		at, atIsString := event.Value("at").(string)
		if _, kindIsString := event.Value("kind").(string); !atIsString || !kindIsString {
			continue
		}
		atMs, valid, err := jsutil.DateValue(at, time.Local)
		if err != nil {
			return nil, err
		}
		if !valid || atMs < startMs || atMs >= endMs {
			continue
		}
		text, err := sim.NarrateEvent(event, s.unmappedKinds())
		if err != nil {
			return nil, err
		}
		if text == "" {
			continue
		}
		speaker := actorName
		if sp := event.Value("speaker"); jsutil.Truthy(sp) {
			if speaker, err = sim.SanitizeLabel(sp); err != nil {
				return nil, err
			}
		}
		stamp, err := formatTimestamp(at)
		if err != nil {
			return nil, err
		}
		lines = append(lines, narrationLine{atMs: atMs, seq: seq, line: "[" + stamp + " " + speaker + "] " + text})
		seq++
	}
	// The engine sends rows in (occurred_at, id) order; seq keeps that order
	// for rows stamped the same millisecond.
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].atMs != lines[j].atMs {
			return lines[i].atMs < lines[j].atMs
		}
		return lines[i].seq < lines[j].seq
	})

	slug := slugPrefix + "conversations/" + day + "-sim-day"
	title := "Sim day — " + actorName + " — " + day
	if len(lines) == 0 {
		// No note, and no roster enrollment: the roster holds villagers
		// with material to dream.
		s.Log.Log("sim-distill", "skip-empty", logger.Fields{{Key: "agent", Value: in.Agent}, {Key: "day", Value: day}})
		return logger.Fields{{Key: "skipped", Value: true}, {Key: "reason", Value: "no narratable events"}}, nil
	}
	text := make([]string, len(lines))
	for i, l := range lines {
		text[i] = l.line
	}
	content := "Day: " + day + " — " + actorName + "\n\n" + strings.Join(text, "\n") + "\n"

	if _, err := s.Notes.Save(ctx, notes.SaveInput{
		Namespace: in.Agent, Title: title, Content: content, Slug: slug, CreatedBy: in.Agent, Upsert: true,
	}); err != nil {
		return nil, err
	}
	if shared {
		if _, err := s.DB.Exec(ctx, `INSERT INTO sim_shared_actor (shared_actor_id, slug_prefix, display_name, last_pushed_day)
             VALUES ($1, $2, $3, $4)
             ON CONFLICT (shared_actor_id, slug_prefix)
             DO UPDATE SET display_name = EXCLUDED.display_name,
                           last_pushed_day = EXCLUDED.last_pushed_day`,
			actorID, slugPrefix, actorName, day); err != nil {
			return nil, err
		}
	}
	bytes := jsutil.UTF16Len(content)
	s.Log.Log("sim-distill", "saved", logger.Fields{{Key: "agent", Value: in.Agent}, {Key: "day", Value: day}, {Key: "lines", Value: len(lines)}, {Key: "bytes", Value: bytes}})
	return logger.Fields{{Key: "saved", Value: true}, {Key: "slug", Value: slug}, {Key: "lines", Value: len(lines)}, {Key: "bytes", Value: bytes}}, nil
}
