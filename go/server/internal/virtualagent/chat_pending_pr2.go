package virtualagent

import (
	"context"
	"math"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// TEMPORARY (LLM-933 PR 3): the helpers mail and chat share are ported by
// PR 2. Until it merges, chat reaches them through contextLoader; the
// merge deletes this file and calls PR 2's methods directly.

// contextLoader is loadRAGContext, loadSoul and loadPeopleContext.
type contextLoader interface {
	loadRAGContext(ctx context.Context, agentName, query string) string
	loadSoul(ctx context.Context, agentName string) string
	loadPeopleContext(ctx context.Context, agentName string, counterparts []string) string
}

type noContext struct{}

func (noContext) loadRAGContext(context.Context, string, string) string      { return "" }
func (noContext) loadSoul(context.Context, string) string                    { return "" }
func (noContext) loadPeopleContext(context.Context, string, []string) string { return "" }

func (s *Service) contexts() contextLoader {
	if s.contextLoader != nil {
		return s.contextLoader
	}
	return noContext{}
}

// formatDuration is "45 seconds", "5 minutes", "1 hour(s) 10 minutes".
func formatDuration(totalSeconds float64) string {
	if totalSeconds < 60 {
		return jsutil.NumberString(totalSeconds) + " seconds"
	}
	hours := math.Floor(totalSeconds / 3600)
	minutes := math.Ceil(math.Mod(totalSeconds, 3600) / 60)
	switch {
	case hours > 0 && minutes > 0:
		return jsutil.NumberString(hours) + " hour(s) " + jsutil.NumberString(minutes) + " minutes"
	case hours > 0:
		return jsutil.NumberString(hours) + " hour(s)"
	}
	return jsutil.NumberString(minutes) + " minutes"
}

// missingConfigParts names the settings an agent needs before it can call
// its provider.
func missingConfigParts(a *Agent) []string {
	var missing []string
	if str(a.APIKey) == "" {
		missing = append(missing, "API key")
	}
	if str(a.Provider) == "" {
		missing = append(missing, "provider")
	}
	if str(a.Model) == "" {
		missing = append(missing, "model")
	}
	return missing
}

func unanswerableNotice(agentName, reason string, missing []string) string {
	return Seam{}.UnanswerableNotice(agentName, reason, missing)
}
