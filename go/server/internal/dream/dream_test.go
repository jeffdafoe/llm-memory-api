package dream

// The tests of Node's dream.test.js, ported.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sim"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestBuildNotesLog(t *testing.T) {
	got := buildNotesLog([]noteRow{{slug: "journal/2026-05-california-trip", content: "We drove the coast road.", updatedAt: mustTime(t, "2026-05-18T14:22:00Z")}})
	if want := "## Note: journal/2026-05-california-trip (updated 2026-05-18)\n\nWe drove the coast road."; got != want {
		t.Errorf("one note: %q", got)
	}
	got = buildNotesLog([]noteRow{
		{slug: "core-identity", content: "A", updatedAt: mustTime(t, "2026-05-16T08:00:00Z")},
		{slug: "thea-core", content: "B", updatedAt: mustTime(t, "2026-05-16T19:30:00Z")},
	})
	if want := "## Note: core-identity (updated 2026-05-16)\n\nA\n\n---\n\n## Note: thea-core (updated 2026-05-16)\n\nB"; got != want {
		t.Errorf("two notes: %q", got)
	}
}

func TestSoulNeedsRebuild(t *testing.T) {
	healthy := strings.Repeat("x", 5000)
	cases := []struct {
		soul     string
		minChars float64
		want     bool
	}{
		{"", 800, true},
		{"   \n\t  ", 800, true},
		{strings.Repeat("x", 708), 800, true},
		{healthy, 800, false},
		{"   " + strings.Repeat("x", 700) + strings.Repeat("\n", 200), 800, true},
		{strings.Repeat("x", 800), 800, false},
		{strings.Repeat("x", 10), 0, false},
		{strings.Repeat("x", 10), -1, false},
		{"", 0, true},
	}
	for _, c := range cases {
		if got := soulNeedsRebuild(c.soul, c.minChars); got != c.want {
			t.Errorf("soulNeedsRebuild(%.20q, %v) = %v", c.soul, c.minChars, got)
		}
	}
}

const stub = "a degraded truncated stub soul"

func TestBuildSoulUserMessage(t *testing.T) {
	msg := buildSoulUserMessage(soulMessage{agentName: "work", existingSoul: stub, needsRebuild: true,
		backloadDreams: "### dreams/2026-07-14-x\n\nyesterday material", chunkDate: "2026-07-15", dreamContent: "today chunk"})
	for _, want := range []string{"(none on file — rebuilding from recent dreams)", "## Dream snapshot for initial soul rebuild", "There is no usable prior soul document.", "yesterday material"} {
		if !strings.Contains(msg, want) {
			t.Errorf("rebuild with backload: no %q", want)
		}
	}
	if strings.Contains(msg, stub) {
		t.Error("rebuild with backload: the stub leaked into the prompt")
	}

	msg = buildSoulUserMessage(soulMessage{agentName: "work", existingSoul: stub, needsRebuild: true, chunkDate: "2026-07-15", dreamContent: "today chunk"})
	if strings.Contains(msg, stub) || !strings.Contains(msg, "(none on file — rebuilding from recent dreams)") ||
		!strings.Contains(msg, "## Dream snapshot for 2026-07-15") || !strings.Contains(msg, "today chunk") || strings.Contains(msg, "initial soul rebuild") {
		t.Errorf("rebuild without backload: %q", msg)
	}

	soul := "a healthy multi-paragraph soul"
	msg = buildSoulUserMessage(soulMessage{agentName: "work", startupInstructions: "You are Work.", existingSoul: soul, chunkDate: "2026-07-15", dreamContent: "today chunk"})
	if !strings.Contains(msg, soul) || !strings.Contains(msg, "## Character description\n\nYou are Work.") || !strings.Contains(msg, "## Dream snapshot for 2026-07-15") ||
		strings.Contains(msg, "rebuilding from recent dreams") || strings.Contains(msg, "initial soul rebuild") || !strings.HasSuffix(msg, sim.SoulLengthDirective) {
		t.Errorf("evolve: %q", msg)
	}

	msg = buildSoulUserMessage(soulMessage{agentName: "work", existingSoul: "soul", chunkDate: "2026-07-15", dreamContent: "today"})
	if strings.Contains(msg, "## Character description") {
		t.Error("empty startup instructions gave a character section")
	}
}

const simDayLog = "[Wednesday 14:30 Constance Scott] (earned 6 coins working for Josiah Thorne)\n" +
	"[Wednesday 14:35 Josiah Thorne] Fair work, fairly paid."

func TestExtractSpeakersSelfSkip(t *testing.T) {
	sp := extractSpeakers(simDayLog, "Constance Scott")
	if sp.get("constance-scott") != nil {
		t.Error("the villager herself was not self-skipped")
	}
	if j := sp.get("josiah-thorne"); j == nil || j.display != "Josiah Thorne" {
		t.Errorf("counterparty: %+v", j)
	}
	// The pooled agent's name does not skip the villager: why selfName is
	// threaded through.
	if extractSpeakers(simDayLog, "salem-vendor").get("constance-scott") == nil {
		t.Error("the pooled agent name skipped the villager")
	}
}

var ledgerDay = strings.Join([]string{
	"[Wednesday 17:48 Constance Scott] (offered to work for Josiah Thorne for 4 coins)",
	`[Wednesday 17:48 Josiah Thorne] "I can offer four coins for a couple of hours' work, if that suits you."`,
	"[Wednesday 19:49 Constance Scott] (earned 4 coins working for Josiah Thorne)",
	"[Wednesday 19:49 Constance Scott] (offered to work for Josiah Thorne for 6 coins)",
	`[Wednesday 20:23 Josiah Thorne] "Fifteen coins for three cuts — that's a fair trade, John."`,
	`[Wednesday 20:24 John Ellis] "Good trades, Josiah. Appreciate you keeping the shelves stocked."`,
	"[Wednesday 21:50 Constance Scott] (earned 6 coins working for Josiah Thorne)",
	"[Wednesday 22:00 Constance Scott] (had stew)",
	`[Wednesday 22:01 Josiah Thorne] "Off you go, then — safe to you, Constance Scott."`,
}, "\n")

func sectionsFor(log, self string) map[string]*personSection {
	out := map[string]*personSection{}
	for _, s := range buildPersonExcerptSections(log, self, extractSpeakers(log, self)) {
		out[s.slug] = s
	}
	return out
}

func equalLines(t *testing.T, label string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") || len(got) != len(want) {
		t.Errorf("%s:\n got %q\nwant %q", label, got, want)
	}
}

func TestLedgerSections(t *testing.T) {
	josiah := sectionsFor(ledgerDay, "Constance Scott")["josiah-thorne"]
	equalLines(t, "own ledger lines filed under the person named", josiah.ledger, []string{
		"[Wednesday 17:48 Constance Scott] (offered to work for Josiah Thorne for 4 coins)",
		"[Wednesday 19:49 Constance Scott] (earned 4 coins working for Josiah Thorne)",
		"[Wednesday 19:49 Constance Scott] (offered to work for Josiah Thorne for 6 coins)",
		"[Wednesday 21:50 Constance Scott] (earned 6 coins working for Josiah Thorne)",
	})
	text := strings.Join(josiah.ledger, "\n")
	if !strings.Contains(text, "earned 4 coins") || !strings.Contains(text, "earned 6 coins") {
		t.Error("an engagement was lost")
	}
	for slug, s := range sectionsFor(ledgerDay, "Constance Scott") {
		for _, l := range s.ledger {
			if strings.Contains(l, "(had stew)") {
				t.Errorf("a solitary act was filed under %s", slug)
			}
		}
	}
	if sectionsFor(ledgerDay, "Constance Scott")["constance-scott"] != nil {
		t.Error("the villager got a section about herself")
	}

	var carrots, farewell string
	for _, l := range josiah.said {
		if strings.Contains(l, "Fifteen coins for three cuts") {
			carrots = l
		}
		if strings.Contains(l, "Off you go") {
			farewell = l
		}
	}
	if !strings.HasSuffix(carrots, "(overheard — addressed to John Ellis)") {
		t.Errorf("third-party speech not labeled: %q", carrots)
	}
	if strings.Contains(farewell, "overheard") {
		t.Errorf("speech naming the villager labeled: %q", farewell)
	}
}

func TestPromiseDay(t *testing.T) {
	log := strings.Join([]string{
		`[Friday 18:25 Josiah Thorne] "I paid you ten coins for your labor and added a cut of meat besides, as I recollect."`,
		`[Friday 18:25 Josiah Thorne] "There we are — six coins to square us up, and a fair bit of gratitude besides."`,
		"[Friday 18:26 Constance Scott] (paid Josiah Thorne 1 coin for milk)",
	}, "\n")
	josiah := sectionsFor(log, "Constance Scott")["josiah-thorne"]
	equalLines(t, "unbacked promise", josiah.ledger, []string{"[Friday 18:26 Constance Scott] (paid Josiah Thorne 1 coin for milk)"})
	if !strings.Contains(strings.Join(josiah.said, "\n"), "six coins to square us up") {
		t.Error("the promise is not in the speech")
	}
}

func TestAddresseeHeuristics(t *testing.T) {
	log := `[Friday 12:00 Josiah Thorne] "Fair trade, John."` + "\n" +
		`[Friday 12:01 John Ellis] "Aye."` + "\n" + `[Friday 12:02 John Proctor] "Aye."`
	if s := sectionsFor(log, "Constance Scott")["josiah-thorne"]; strings.Contains(s.said[0], "overheard") {
		t.Error("an ambiguous first name inferred an addressee")
	}

	log = `[Friday 12:00 Josiah Thorne] "The annexed field is Anne's no longer."` + "\n" + `[Friday 12:01 Anne Walker] "Aye."`
	if s := sectionsFor(log, "Constance Scott")["josiah-thorne"]; !strings.Contains(s.said[0], "(overheard — addressed to Anne Walker)") {
		t.Errorf("whole-word match: %q", s.said[0])
	}

	log = `[Friday 12:00 Josiah Thorne] "Constance, John Ellis still owes me two coins."` + "\n" + `[Friday 12:01 John Ellis] "I do not."`
	if s := sectionsFor(log, "Constance Scott")["josiah-thorne"]; strings.Contains(s.said[0], "overheard") {
		t.Error("a line naming the villager was labeled overheard")
	}

	log = "[Friday 12:00 Josiah Thorne] (paid John Ellis 3 coins for meat)\n" + `[Friday 12:01 John Ellis] "Fair trade, Josiah."`
	ellis := sectionsFor(log, "zbbs-josiah-thorne")["john-ellis"]
	equalLines(t, "dedicated NPC slug", ellis.ledger, []string{"[Friday 12:00 Josiah Thorne] (paid John Ellis 3 coins for meat)"})
	if strings.Contains(ellis.said[0], "overheard") {
		t.Error("a line naming Josiah is not addressed to Josiah")
	}
}

func TestLedgerAttribution(t *testing.T) {
	log := "[Friday 12:00 Josiah Thorne] (delivered 3x flour to Abraham Warren for 6 coins)\n" + `[Friday 12:01 Constance Scott] "A fair price."`
	josiah := sectionsFor(log, "Constance Scott")["josiah-thorne"]
	equalLines(t, "another speaker's ledger line", josiah.ledger, []string{"[Friday 12:00 Josiah Thorne] (delivered 3x flour to Abraham Warren for 6 coins)"})
	if len(josiah.said) != 0 {
		t.Error("a ledger line was repeated as speech")
	}

	log = strings.Join([]string{
		"[Friday 18:26 Constance Scott] (paid Josiah Thorne 1 coin for milk)",
		"[Friday 9:05 Constance Scott] (paid Josiah Thorne 2 coins for bread)",
		"[18:26 Constance Scott] (paid Josiah Thorne 3 coins for cheese)",
		`[Friday 12:01 Josiah Thorne] "Good day."`,
	}, "\n")
	if n := len(sectionsFor(log, "Constance Scott")["josiah-thorne"].ledger); n != 3 {
		t.Errorf("header forms: %d ledger lines", n)
	}

	log = "[2026-07-15T18:26Z Constance Scott] (paid Josiah Thorne 1 coin for milk)\n" + `[Friday 12:01 Josiah Thorne] "Good day."`
	if n := len(sectionsFor(log, "Constance Scott")["josiah-thorne"].ledger); n != 1 {
		t.Errorf("unreducible header: %d ledger lines", n)
	}

	log = "[Friday 12:00 Constance Scott] (watched Josiah Thorne pay John Ellis 2 coins)\n" + `[Friday 12:01 Josiah Thorne] "Settled."` + "\n" + `[Friday 12:02 John Ellis] "Aye."`
	s := sectionsFor(log, "Constance Scott")
	if len(s["josiah-thorne"].ledger) != 1 || len(s["john-ellis"].ledger) != 1 {
		t.Error("a line naming two people is not filed under both")
	}
}

func TestPrefilterLedger(t *testing.T) {
	log := "[Friday 18:26 Constance Scott] (paid Josiah Thorne 1 coin for milk)\n[Friday 19:49 Constance Scott] (earned 4 coins working for Josiah Thorne)"
	filtered, ok := prefilterLog(log)
	if !ok || !strings.Contains(filtered, "(paid Josiah Thorne 1 coin for milk)") || !strings.Contains(filtered, "(earned 4 coins working for Josiah Thorne)") {
		t.Errorf("ledger-only day: %q %v", filtered, ok)
	}
	if _, ok := prefilterLog("the weather held\nthe road was dry"); ok {
		t.Error("a day with neither signal nor ledger was kept")
	}
	log = "I want you to remember this.\nfiller\nfiller\nfiller\nfiller\nfiller\n[Friday 20:03 Constance Scott] (delivered meat to John Ellis for 4 coins)"
	if filtered, _ := prefilterLog(log); !strings.Contains(filtered, "(delivered meat to John Ellis for 4 coins)") {
		t.Errorf("ledger line outside the context window: %q", filtered)
	}
}

func TestBuildPersonUserMessage(t *testing.T) {
	msg := buildPersonUserMessage(personMessage{selfLabel: "Constance Scott", display: "Josiah Thorne", today: "2026-07-24",
		ledger: []string{"[Friday 18:26 Constance Scott] (paid Josiah Thorne 1 coin for milk)"},
		said:   `[Friday 18:25 Josiah Thorne] "six coins to square us up"`})
	ledgerAt := strings.Index(msg, "## What the ledger records")
	saidAt := strings.Index(msg, "## What was said in your hearing")
	if !strings.Contains(msg, "## What the ledger records\n\n[Friday 18:26 Constance Scott] (paid Josiah Thorne 1 coin for milk)") ||
		!strings.Contains(msg, "## What was said in your hearing\n\n[Friday 18:25 Josiah Thorne] \"six coins to square us up\"") ||
		ledgerAt >= saidAt || !strings.Contains(msg, "Where the two disagree, the ledger wins.") {
		t.Errorf("sections: %q", msg)
	}

	msg = buildPersonUserMessage(personMessage{selfLabel: "Moses James", display: "Constable Gideon Marsh", today: "2026-08-06",
		ledger: []string{"[Thursday 12:11 Moses James] (paid Constable Gideon Marsh 1 coin — the town rate, a due settled, with no goods owed in return)"}})
	if !strings.Contains(msg, "Not everything that moves one way is owed back.") || !strings.Contains(msg, "never delivered") ||
		strings.Index(msg, "Where the two disagree, the ledger wins.") >= strings.Index(msg, "Not everything that moves one way") {
		t.Errorf("one-way transfer clause: %q", msg)
	}

	msg = buildPersonUserMessage(personMessage{selfLabel: "Constance Scott", display: "Josiah Thorne", today: "2026-07-24", existingFile: "prior file", said: "he said he would make it right"})
	if !strings.Contains(msg, "(nothing — no coin or goods changed hands between you and Josiah Thorne today)") {
		t.Error("empty ledger rendered blank")
	}

	msg = buildPersonUserMessage(personMessage{selfLabel: "salem-vendor", display: "Josiah Thorne", today: "2026-07-24", existingFile: "bloated file"})
	if !strings.Contains(msg, "(no new excerpts since last update — please consolidate any redundant bullets if present, or return file unchanged if already tight)") {
		t.Error("consolidation sentinel changed")
	}
}

func TestRunPersonContextUpdateRefusesBadPrefix(t *testing.T) {
	j := &Job{}
	for _, prefix := range []string{"cons%tance/", "../secrets/"} {
		_, err := j.RunPersonContextUpdate(context.Background(), "salem-vendor", "dream-sim-people", "josiah-thorne", "Josiah Thorne", "x", "2026-07-15", PersonOptions{SlugPrefix: prefix})
		if err == nil || !strings.Contains(err.Error(), "invalid slug prefix") {
			t.Errorf("prefix %q: %v", prefix, err)
		}
	}
}

func chunkList(chunks ...logger.Fields) []logger.Fields { return chunks }

func TestCountFailedActors(t *testing.T) {
	f := func(kv ...any) logger.Fields {
		var out logger.Fields
		for i := 0; i < len(kv); i += 2 {
			out = append(out, logger.Field{Key: kv[i].(string), Value: kv[i+1]})
		}
		return out
	}
	if n := countFailedActors([]logger.Fields{f("prefix", "bad/", "error", "invalid slug prefix")}); n != 1 {
		t.Errorf("actor-level error: %d", n)
	}
	if n := countFailedActors([]logger.Fields{f("prefix", "constance-scott/", "plannedChunks", 2, "completedChunks", 1,
		"chunks", chunkList(f("processed", true, "chunkDate", "2026-07-15"), f("chunkDate", "2026-07-16", "error", "model timeout")))}); n != 1 {
		t.Errorf("failed chunk: %d", n)
	}
	if n := countFailedActors([]logger.Fields{
		f("prefix", "a/", "plannedChunks", 1, "completedChunks", 1, "chunks", chunkList(f("processed", true))),
		f("prefix", "b/", "skipped", true, "reason", "no conversation notes"),
		f("prefix", "c/", "plannedChunks", 1, "completedChunks", 1, "chunks", chunkList(f("skipped", true, "reason", "no signals"))),
	}); n != 0 {
		t.Errorf("clean and skipped: %d", n)
	}
	if n := countFailedActors([]logger.Fields{
		f("prefix", "a/", "error", "invalid slug prefix"),
		f("prefix", "b/", "chunks", chunkList(f("processed", true), f("error", "x"))),
		f("prefix", "c/", "chunks", chunkList(f("processed", true))),
	}); n != 2 {
		t.Errorf("roster: %d", n)
	}
}

func TestPeopleNotePath(t *testing.T) {
	ok := func(prefix scopePrefix, slug, want string) {
		t.Helper()
		if got, err := peopleNotePath(prefix, slug); err != nil || got != want {
			t.Errorf("peopleNotePath(%+v, %q) = %q, %v", prefix, slug, got, err)
		}
	}
	fails := func(prefix scopePrefix, slug, want string) {
		t.Helper()
		if _, err := peopleNotePath(prefix, slug); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("peopleNotePath(%+v, %q): %v, want %q", prefix, slug, err, want)
		}
	}
	ok(scopePrefix{}, "jeff", "context/people/jeff")
	ok(scopePrefix{value: "constance-scott/"}, "josiah-thorne", "constance-scott/context/people/josiah-thorne")
	fails(scopePrefix{notString: true}, "jeff", "not a string")
	fails(scopePrefix{value: "../evil/"}, "jeff", "invalid slug prefix")
	fails(scopePrefix{value: "a%b/"}, "jeff", "invalid slug prefix")
	fails(scopePrefix{}, "../secrets", "invalid person slug")
	fails(scopePrefix{value: "constance-scott/"}, "../secrets", "invalid person slug")
	fails(scopePrefix{}, "Not A Slug", "invalid person slug")
	ok(scopePrefix{}, "josiah-thorne", "context/people/josiah-thorne")
}

func TestResolveScopePrefix(t *testing.T) {
	for _, p := range []scopePrefix{{}, {value: ""}} {
		if got, err := resolveScopePrefix(p); err != nil || got != "" {
			t.Errorf("absent: %q %v", got, err)
		}
	}
	for _, v := range []string{"constance-scott/", "agent-7/", strings.Repeat("a", 99) + "/"} {
		if got, err := resolveScopePrefix(scopePrefix{value: v}); err != nil || got != v {
			t.Errorf("canonical %q: %q %v", v, got, err)
		}
	}
	for _, v := range []string{"john-ellis", "constance-scott//", "  constance-scott/  ", strings.Repeat("a", 100) + "/",
		"a%b/", "a_b/", "../secrets/", "foo/../bar/", "Constance-Scott/"} {
		if _, err := resolveScopePrefix(scopePrefix{value: v}); err == nil || !strings.Contains(err.Error(), "invalid slug prefix") {
			t.Errorf("%q: %v", v, err)
		}
	}
	if _, err := resolveScopePrefix(scopePrefix{notString: true}); err == nil || !strings.Contains(err.Error(), "not a string") {
		t.Errorf("non-string: %v", err)
	}
}

func TestPlanCronReport(t *testing.T) {
	for _, result := range []logger.Fields{{{Key: "failedSharedActorCount", Value: 0}}, nil} {
		events := planCronReport(result)
		if len(events) != 1 || events[0] != (cronEvent{kind: "complete", status: "ok"}) {
			t.Errorf("clean run: %+v", events)
		}
	}
	events := planCronReport(logger.Fields{{Key: "failedSharedActorCount", Value: 3}})
	if len(events) != 2 || events[0] != (cronEvent{kind: "shared-failures", count: 3}) || events[1] != (cronEvent{kind: "complete", status: "completed-with-errors"}) {
		t.Errorf("failed run: %+v", events)
	}
}

func TestValidateRosterPrefix(t *testing.T) {
	if got := validateRosterPrefix(nil); got != "" {
		t.Errorf("NULL: %q", got)
	}
	for _, v := range []string{"cons%tance/", "a_b/", "../secrets/", "foo/../bar/", "john-ellis", "constance-scott//", "  constance-scott/  "} {
		if got := validateRosterPrefix(&v); got != "" {
			t.Errorf("%q accepted", v)
		}
	}
	v := "constance-scott/"
	if got := validateRosterPrefix(&v); got != v {
		t.Errorf("canonical: %q", got)
	}
}

// The undefined row of Node's test has no Go counterpart: the config table
// holds text.
func TestDreamDelayMs(t *testing.T) {
	cases := []struct {
		value string
		want  float64
	}{
		{"", 2000}, {"soon", 2000},
		{"2147483648", 3600000}, {"999999999999", 3600000}, {"3600000", 3600000},
		{"5000", 5000}, {"1500.9", 1500}, {"-5", -5},
		{"0", 0}, {" 0 ", 0},
	}
	for _, c := range cases {
		store := config.NewStore()
		store.Set("dream_test_delay", c.value)
		j := &Job{Config: store}
		if got, err := j.dreamDelayMs("dream_test_delay", 2000); err != nil || got != c.want {
			t.Errorf("dreamDelayMs(%q) = %v, %v; want %v", c.value, got, err, c.want)
		}
	}
}

// A prefixed (shared-villager) scope never writes a soul, even when a soul
// agent is passed: the villager's soul lives in the engine, and one
// context/soul would be shared by every pooled villager. Run never passes
// one for such a scope; the guard is the second line.
func TestPrefixedScopeWritesNoSoul(t *testing.T) {
	s := &stream{}
	db := &routeDB{chunk: func(int) ([][]any, error) {
		return [][]any{{"a/conversations/x", "[Monday 09:00 B] \"I want bread\"", testNow}}, nil
	}}
	agents := &fakeAgents{t: t, s: s, answers: []answer{{Text: json.RawMessage(`"# Day"`)}}}
	j := &Job{DB: db, Config: storeOf(nil), Log: fakeLog{s}, Notes: &fakeNotes{s: s}, Agents: agents}
	agent := &agentRow{name: "pool", actorID: 1, dreamMode: "sim-shared", dreamSource: "conversation"}
	_, err := j.processChunk(context.Background(), agent, agentNames{dream: "d", soul: "soul-writer"},
		chunk{from: testNow.Add(-time.Hour), to: testNow}, scope{prefix: scopePrefix{value: "a/"}, selfName: "A"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range s.events {
		if e["kind"] == "invoke" && e["context"] == "soul" || e["kind"] == "read" && e["slug"] == "context/soul" {
			t.Errorf("soul pass ran under a prefixed scope: %v", e)
		}
	}
}
