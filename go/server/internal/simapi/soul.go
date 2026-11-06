package simapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/dream"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sim"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
)

// soulExpertiseTag is the shared soul agent's tag, the one the nightly
// dream resolves for sim souls, so both writers use the same agent.
const soulExpertiseTag = "dream-sim-soul"

// maxFieldLen is a sanity ceiling on each field (UTF-16 units).
const maxFieldLen = 100000

// SoulInput is the /sim/soul body as parsed JSON values.
type SoulInput struct {
	CharacterDescription, CurrentSoul, DaySnapshot, Day any
}

func requireString(name string, v any) (string, error) {
	s, ok := v.(string)
	if !ok || jsutil.Trim(s) == "" {
		return "", badRequest(name + " required")
	}
	if jsutil.UTF16Len(s) > maxFieldLen {
		return "", badRequest(name + " is too long")
	}
	return s, nil
}

func optionalString(name string, v any) (string, error) {
	if v == nil || v == jsutil.Undefined {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", badRequest(name + " must be a string")
	}
	if jsutil.UTF16Len(s) > maxFieldLen {
		return "", badRequest(name + " is too long")
	}
	return s, nil
}

// optionalDay: the day goes into the snapshot header verbatim, so only a
// strict YYYY-MM-DD label is accepted.
func optionalDay(name string, v any) (string, error) {
	if v == nil || v == jsutil.Undefined || v == "" {
		return "", nil
	}
	s, ok := v.(string)
	if !ok || !dayPattern.MatchString(s) {
		return "", badRequest(name + " must be YYYY-MM-DD")
	}
	return s, nil
}

// SynthesizeSoul is synthesizeSimSoul: a shared NPC's soul from the
// engine's day material, through the dream-sim-soul agent with its own
// startup instructions and its rate and cost limits on. A truncated reply,
// an empty one and one opening with the model's reasoning come back as
// {text: "", rejected}, and the engine keeps the prior soul.
func (s *Service) SynthesizeSoul(ctx context.Context, in SoulInput) (logger.Fields, error) {
	description, err := requireString("character_description", in.CharacterDescription)
	if err != nil {
		return nil, err
	}
	snapshot, err := requireString("day_snapshot", in.DaySnapshot)
	if err != nil {
		return nil, err
	}
	current, err := optionalString("current_soul", in.CurrentSoul)
	if err != nil {
		return nil, err
	}
	day, err := optionalDay("day", in.Day)
	if err != nil {
		return nil, err
	}

	finder := &dream.Job{DB: s.DB, Log: s.Log, Trust: s.Trust}
	agent, err := finder.FindDreamAgent(ctx, soulExpertiseTag)
	if err != nil {
		return nil, err
	}
	if agent == "" {
		// Node's 503, which its route wrapper answers as a 500.
		return nil, apierr.New(http.StatusServiceUnavailable, "soul agent (dream-sim-soul) not available")
	}

	res, err := s.Agents.Invoke(ctx, agent, virtualagent.InvokeOptions{
		UserMessage: sim.BuildSoulUserMessage(sim.SoulMessage{
			CharacterDescription: description, CurrentSoul: current, DaySnapshot: snapshot, Day: day,
		}),
		Context: "soul",
		Retry:   true,
	})
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		s.Log.Log("sim-soul", "truncated-rejected", logger.Fields{{Key: "agent", Value: agent}, {Key: "finishReason", Value: res.FinishReason}})
		return logger.Fields{{Key: "text", Value: ""}, {Key: "rejected", Value: "truncated"}}, nil
	}
	trimmed := ""
	if jsutil.Truthy(res.Text) {
		t, ok := res.Text.(string)
		if !ok {
			return nil, errors.New(`(text || "").trim is not a function`)
		}
		trimmed = jsutil.Trim(t)
	}
	if trimmed == "" {
		s.Log.Log("sim-soul", "empty-reply", logger.Fields{{Key: "agent", Value: agent}})
		return logger.Fields{{Key: "text", Value: ""}, {Key: "rejected", Value: "empty-reply"}}, nil
	}
	if marker := dream.DetectReasoningPreamble(trimmed); marker != "" {
		s.Log.Log("sim-soul", "reasoning-preamble-rejected", logger.Fields{{Key: "agent", Value: agent}, {Key: "marker", Value: marker}})
		return logger.Fields{{Key: "text", Value: ""}, {Key: "rejected", Value: "reasoning-preamble"}}, nil
	}
	s.Log.Log("sim-soul", "synthesized", logger.Fields{{Key: "agent", Value: agent}, {Key: "bytes", Value: jsutil.UTF16Len(trimmed)}})
	return logger.Fields{{Key: "text", Value: trimmed}}, nil
}
