package virtualagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/system"
)

// A trigger for an agent whose generation is finishing is never lost: it
// either marks a rerun the finishing generation then checks, or it finds the
// slot free and runs its own generation. Node could not interleave the two
// (its rerun check and its slot release had no await between them); Go
// can, so the check and the release are one locked step.
func TestDiscussionTriggerAtGenerationEndIsNotLost(t *testing.T) {
	var base *discussionScenario
	for _, c := range loadDiscussionGoldens(t).Scenarios {
		if c.Name == "message addressed" {
			base = &c
			break
		}
	}
	if base == nil {
		t.Fatal("scenario missing")
	}
	payload := json.RawMessage(base.Payloads[0])
	// The second trigger lands before the check (waited for), or at about
	// the same time (not waited for, after a short pause): both orders run.
	for _, delay := range []time.Duration{-1, 0, 50 * time.Microsecond, 200 * time.Microsecond, time.Millisecond} {
		for i := 0; i < 8; i++ {
			sc := *base
			s, r := newDiscussionService(t, &sc)
			var once sync.Once
			var wg sync.WaitGroup
			s.beforeRerunCheck = func(string) {
				once.Do(func() {
					wg.Add(1)
					run := func() {
						defer wg.Done()
						if err := s.HandleVirtualAgent(context.Background(), payload, system.Meta{}); err != nil {
							t.Error(err)
						}
					}
					if delay < 0 {
						run()
						return
					}
					go func() {
						time.Sleep(delay)
						run()
					}()
				})
			}
			if err := s.HandleVirtualAgent(context.Background(), payload, system.Meta{}); err != nil {
				t.Fatal(err)
			}
			wg.Wait()

			r.mu.Lock()
			coalesced, checked := 0, 0
			for _, l := range r.logs {
				switch l.Action {
				case "coalesce-trigger":
					coalesced++
				case "rerun", "rerun-skip-no-new-message":
					checked++
				}
			}
			generations := len(r.calls)
			r.mu.Unlock()
			s.inFlightMu.Lock()
			left := len(s.inFlight)
			s.inFlightMu.Unlock()

			// Coalesced: the first generation's check took it. Not
			// coalesced: the second trigger ran a generation of its own.
			if coalesced == 1 && checked != 1 || coalesced == 0 && generations != 2 || coalesced > 1 {
				t.Fatalf("delay %v: coalesced %d, rerun checks %d, generations %d", delay, coalesced, checked, generations)
			}
			if left != 0 {
				t.Fatalf("delay %v: %d slots left taken", delay, left)
			}
		}
	}
}

// Every way out of a generation frees the slot: after a failure, after a
// stop before the call, and after a reply, the next trigger is served.
func TestDiscussionSlotFreedOnEveryExit(t *testing.T) {
	names := []string{"message addressed", "provider failure single agent", "over budget", "post error fails", "reply send fails"}
	for _, name := range names {
		var sc *discussionScenario
		for _, c := range loadDiscussionGoldens(t).Scenarios {
			if c.Name == name {
				sc = &c
				break
			}
		}
		if sc == nil {
			t.Fatalf("scenario %q missing", name)
		}
		s, r := newDiscussionService(t, sc)
		for i := 0; i < 2; i++ {
			s.HandleVirtualAgent(context.Background(), json.RawMessage(sc.Payloads[0]), system.Meta{})
		}
		s.inFlightMu.Lock()
		left := len(s.inFlight)
		s.inFlightMu.Unlock()
		r.mu.Lock()
		var actions []string
		for _, l := range r.logs {
			actions = append(actions, l.Action)
		}
		r.mu.Unlock()
		if left != 0 || strings.Contains(strings.Join(actions, " "), "coalesce-trigger") {
			t.Errorf("%s: %d slots left, logs %v", name, left, actions)
		}
	}
}
