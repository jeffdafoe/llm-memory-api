package dockind

import "testing"

func TestOf(t *testing.T) {
	cases := []struct{ in, out string }{
		// The folder rules notes.SlugToKind recorded from Node.
		{"instructions/x", "instruction"},
		{"notes/codebase/x", "reference"},
		{"conversations/x", "conversation"},
		{"context/soul", "context"},
		{"dreams/x", "dream"},
		{"tasks/done/x", "task"},
		{"tasks/x", "task"},
		{"learnings/x", "learning"},
		{"notes/x", "note"},
		{"other/x", "note"},
		{"Notes/Codebase/x", "note"},
		{"instructions", "note"},
		// A sim-shared villager's sim-day log.
		{"lewis-walker/conversations/2026-11-24-sim-day", "conversation"},
		// The villager's other folders stay notes, so they stay indexed for
		// recall.
		{"lewis-walker/context/people/anne-walker", "note"},
		{"lewis-walker/dreams/2026-11-24", "note"},
		{"lewis-walker/learnings/2026-11-24-sim-day", "note"},
		// Deeper folders named conversations are ordinary notes.
		{"projects/acme/conversations/kickoff", "note"},
		{"a/b/conversations/x", "note"},
		// Not a folder named conversations.
		{"x/conversations", "note"},
		{"x/conversationsx/y", "note"},
		{"/conversations/x", "note"},
		{"Conversations/x", "note"},
		{"x/Conversations/y", "note"},
		// Every top-level folder wins over the prefixed rule.
		{"instructions/conversations/x", "instruction"},
		{"context/conversations/x", "context"},
		{"dreams/conversations/x", "dream"},
		{"tasks/conversations/x", "task"},
		{"learnings/conversations/x", "learning"},
		{"conversations/conversations/x", "conversation"},
		// notes/ is not a reserved folder; notes/codebase/ is two segments.
		{"notes/conversations/x", "conversation"},
		{"notes/codebase/conversations/x", "reference"},
	}
	for _, c := range cases {
		if got := Of(c.in); got != c.out {
			t.Errorf("Of(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

func TestIsConversation(t *testing.T) {
	yes := []string{"conversations/2026-11-25-abc", "conversations/", "silence-walker/conversations/2026-11-25-sim-day", "a/conversations/"}
	no := []string{"", "conversations", "notes/x", "a/b/conversations/x", "/conversations/x", "a//conversations/x", "dreams/x",
		"instructions/conversations/x", "context/conversations/x", "dreams/conversations/x", "tasks/conversations/x", "learnings/conversations/x"}
	for _, s := range yes {
		if !IsConversation(s) {
			t.Errorf("IsConversation(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if IsConversation(s) {
			t.Errorf("IsConversation(%q) = true, want false", s)
		}
	}
}
