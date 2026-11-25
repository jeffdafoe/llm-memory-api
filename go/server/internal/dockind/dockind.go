// Package dockind is the document kind a note's slug implies. The kind is
// written to documents.kind when a note is saved or moved; search weights,
// search decay, the cleanup job's decay and conversation retention, and
// note enrichment are all keyed on it.
package dockind

import "strings"

// Of is the kind a slug's folder implies. Matching is case-sensitive.
func Of(slug string) string {
	switch {
	case strings.HasPrefix(slug, "instructions/"):
		return "instruction"
	case strings.HasPrefix(slug, "notes/codebase/"):
		return "reference"
	case strings.HasPrefix(slug, "conversations/"):
		return "conversation"
	case strings.HasPrefix(slug, "context/"):
		return "context"
	case strings.HasPrefix(slug, "dreams/"):
		return "dream"
	case strings.HasPrefix(slug, "tasks/"):
		return "task"
	case strings.HasPrefix(slug, "learnings/"):
		return "learning"
	case isPrefixedConversation(slug):
		return "conversation"
	}
	return "note"
}

// IsConversation reports whether a slug is a conversation log.
func IsConversation(slug string) bool {
	return Of(slug) == "conversation"
}

// isPrefixedConversation matches <prefix>/conversations/..., where the
// prefix is one segment. A sim-shared virtual agent keeps one folder per
// villager, so its sim-day logs live at
// <villager>/conversations/<day>-sim-day (simapi/distill.go), and the
// villager prefix is a single kebab segment (sim.NormalizeSlugPrefix).
//
// Of checks it after every top-level folder, so a reserved folder keeps its
// kind (context/conversations/x stays context). Only the one-segment shape
// is matched: a conversation log falls under conversation retention and is
// deleted after conversation_retention_days, so an ordinary note in a
// deeper folder that happens to be named "conversations"
// (projects/acme/conversations/kickoff) must stay a note.
func isPrefixedConversation(slug string) bool {
	i := strings.IndexByte(slug, '/')
	return i > 0 && strings.HasPrefix(slug[i+1:], "conversations/")
}
