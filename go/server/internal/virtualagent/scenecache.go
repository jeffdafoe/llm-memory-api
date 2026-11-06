package virtualagent

import (
	"sort"
	"strings"
	"sync"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// isJSLineTerminator is a character JavaScript's ^ and $ (with m) and . stop
// at.
func isJSLineTerminator(r rune) bool {
	return r == '\n' || r == '\r' || r == ' ' || r == ' '
}

// extractCoLocatedNames reads the people around a sim NPC from the
// engine's perception: the first "huddle: with <names>" line (Node's
// /^huddle: with (.+)$/m). The engine names only acquaintances, so the
// descriptors it uses for others ("the keeper", "a stranger") are dropped;
// "the" is matched in lower case only, so a name such as "The Mountain"
// stays.
func extractCoLocatedNames(text string) []string {
	if text == "" {
		return nil
	}
	const prefix = "huddle: with "
	var rest string
	found := false
	for _, line := range strings.FieldsFunc("\n"+text+"\n", isJSLineTerminator) {
		if strings.HasPrefix(line, prefix) && len(line) > len(prefix) {
			rest, found = line[len(prefix):], true
			break
		}
	}
	if !found {
		return nil
	}
	var out []string
	for _, part := range strings.Split(rest, ",") {
		name := jsutil.Trim(part)
		if name == "" || name == "a stranger" || isTheDescriptor(name) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// isTheDescriptor is /^the\s+/.
func isTheDescriptor(s string) bool {
	rest, ok := strings.CutPrefix(s, "the")
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		return jsutil.IsSpace(r)
	}
	return false
}

// dedupe keeps the first of each name ([...new Set(names)]).
func dedupe(names []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// The scene co-located cache keeps the people a sim NPC saw in a scene's
// last fresh perception, so the follow-up calls of the same turn (tool
// results, with no perception) still load the Impressions of the people
// around it. An entry idle for 10 minutes is dropped when next touched;
// past 500 entries the least recently used go.
const (
	sceneCoLocatedIdleMs = 10 * 60 * 1000
	sceneCoLocatedMax    = 500
)

type sceneEntry struct {
	sceneID  string
	names    []string
	lastSeen float64
}

// sceneCache is a JavaScript Map: entries in insertion order, and a set on
// an existing key keeps its place.
type sceneCache struct {
	mu      sync.Mutex
	entries []*sceneEntry
	index   map[string]*sceneEntry
}

func (c *sceneCache) remove(id string) {
	if _, ok := c.index[id]; !ok {
		return
	}
	delete(c.index, id)
	for i, e := range c.entries {
		if e.sceneID == id {
			c.entries = append(c.entries[:i:i], c.entries[i+1:]...)
			return
		}
	}
}

func (s *Service) rememberSceneCoLocated(sceneID string, names []string) {
	if sceneID == "" || len(names) == 0 {
		return
	}
	c := &s.sceneCache
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.nowMs()
	if c.index == nil {
		c.index = map[string]*sceneEntry{}
	}
	stored := append([]string(nil), names...)
	if e, ok := c.index[sceneID]; ok {
		e.names, e.lastSeen = stored, now
	} else {
		e := &sceneEntry{sceneID: sceneID, names: stored, lastSeen: now}
		c.entries = append(c.entries, e)
		c.index[sceneID] = e
	}
	// Pruned after the set, so refreshing an entry at the cap evicts
	// nothing.
	cutoff := now - sceneCoLocatedIdleMs
	kept := c.entries[:0]
	for _, e := range c.entries {
		if e.lastSeen < cutoff {
			delete(c.index, e.sceneID)
			continue
		}
		kept = append(kept, e)
	}
	c.entries = kept
	if len(c.entries) > sceneCoLocatedMax {
		sorted := append([]*sceneEntry(nil), c.entries...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].lastSeen < sorted[j].lastSeen })
		drop := len(c.entries) - sceneCoLocatedMax
		for _, e := range sorted[:drop] {
			c.remove(e.sceneID)
		}
	}
}

func (s *Service) recallSceneCoLocated(sceneID string) []string {
	if sceneID == "" {
		return []string{}
	}
	c := &s.sceneCache
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.index[sceneID]
	if !ok {
		return []string{}
	}
	now := s.nowMs()
	if now-e.lastSeen > sceneCoLocatedIdleMs {
		c.remove(sceneID)
		return []string{}
	}
	e.lastSeen = now
	return append([]string{}, e.names...)
}

func (s *Service) forgetSceneCoLocated(sceneID string) {
	if sceneID == "" {
		return
	}
	c := &s.sceneCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remove(sceneID)
}
