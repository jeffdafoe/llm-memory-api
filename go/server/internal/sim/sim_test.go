package sim

// The tests of Node's sim-conversation-distiller.test.js and
// sim-soul.test.js (buildSoulUserMessage), ported.

import (
	"strings"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

func narrate(t *testing.T, event string) string {
	t.Helper()
	v, err := jsutil.Parse([]byte(event))
	if err != nil {
		t.Fatal(err)
	}
	got, err := NarrateEvent(v.(*jsutil.Object), &UnmappedKinds{})
	if err != nil {
		t.Fatalf("%s: %v", event, err)
	}
	return got
}

func TestNarrateEvent(t *testing.T) {
	cases := []struct{ event, want string }{
		{`{"kind":"spoke","payload":{"text":"Good morrow, neighbour."}}`, `"Good morrow, neighbour."`},
		{`{"kind":"speak","payload":{"text":"Good morrow, neighbour."}}`, `"Good morrow, neighbour."`},
		{`{"kind":"spoke","payload":{"text":"   "}}`, ""},
		{`{"kind":"spoke","payload":{}}`, ""},
		{`{"kind":"paid","payload":{"recipient":"Hannah","amount":3,"for":"bread"}}`, "(paid Hannah 3 coins for bread)"},
		{`{"kind":"pay","payload":{"recipient":"Hannah","amount":3,"for":"bread"}}`, "(paid Hannah 3 coins for bread)"},
		{`{"kind":"paid","payload":{"recipient":"Hannah","amount":1}}`, "(paid Hannah 1 coin)"},
		{`{"kind":"paid","payload":{"recipient":"Hannah","amount":0}}`, "(paid Hannah 0 coins)"},
		{`{"kind":"paid","payload":{"recipient":"Constable Gideon Marsh","amount":1,"for":"Day's rate on the James Farm","rate_settled":1}}`,
			"(paid Constable Gideon Marsh 1 coin — the town rate, a due settled, with no goods owed in return)"},
		{`{"kind":"paid","payload":{"recipient":"Hannah","amount":3,"for":"bread","rate_settled":0}}`, "(paid Hannah 3 coins for bread)"},
		{`{"kind":"paid","payload":{"recipient":"Hannah","amount":3,"for":"bread","rate_settled":"nonsense"}}`, "(paid Hannah 3 coins for bread)"},
		{`{"kind":"paid","payload":{"recipient":"Hannah","amount":3,"for":"bread","rate_settled":null}}`, "(paid Hannah 3 coins for bread)"},
		{`{"kind":"walked","payload":{"destination":"the Tavern"}}`, "(walked to the Tavern)"},
		{`{"kind":"move_to","payload":{"destination":"the Tavern"}}`, "(walked to the Tavern)"},
		{`{"kind":"delivered","payload":{"recipient":"Bram","item":"bread","qty":2,"amount":6}}`, "(delivered 2x bread to Bram for 6 coins)"},
		{`{"kind":"delivered","payload":{"recipient":"Bram","item":"ale","qty":1,"amount":0}}`, "(delivered ale to Bram)"},
		{`{"kind":"consumed","payload":{"item":"ale","qty":1}}`, "(had ale)"},
		{`{"kind":"consumed","payload":{"item":"bread","qty":3}}`, "(had 3x bread)"},
		{`{"kind":"took_break","payload":{"reason":"weary from the day"}}`, "(stepped away, weary from the day)"},
		{`{"kind":"took_break","payload":{}}`, "(stepped away)"},
		{`{"kind":"labored","payload":{"employer":"Hannah","amount":5,"duration_min":30}}`, "(earned 5 coins working for Hannah)"},
		{`{"kind":"labored","payload":{"amount":1}}`, "(earned 1 coin working for someone)"},
		{`{"kind":"offered_work","payload":{"worker":"Patience","amount":4,"duration_min":240}}`, "(offered Patience a job for 4 coins)"},
		{`{"kind":"offered_work","payload":{"amount":1}}`, "(offered someone a job for 1 coin)"},
		{`{"kind":"solicited_work","payload":{"employer":"Hannah","amount":4,"duration_min":240}}`, "(offered to work for Hannah for 4 coins)"},
		{`{"kind":"solicited_work","payload":{"amount":1}}`, "(offered to work for someone for 1 coin)"},
		{`{"kind":"hired","payload":{"worker":"Hannah","amount":4,"duration_min":240}}`, "(hired Hannah for 4 coins)"},
		{`{"kind":"hired","payload":{"amount":6}}`, "(hired someone for 6 coins)"},
		{`{"kind":"labored","payload":{"employer":"Hannah","amount":2,"reward_items":[{"item":"porridge","qty":1}]}}`, "(earned porridge and 2 coins working for Hannah)"},
		{`{"kind":"solicited_work","payload":{"employer":"Hannah","amount":0,"reward_items":[{"item":"porridge","qty":2}]}}`, "(offered to work for Hannah for 2x porridge)"},
		{`{"kind":"hired","payload":{"worker":"Anne","amount":1,"reward_items":[{"item":"porridge","qty":1},{"item":"bread","qty":2}]}}`, "(hired Anne for porridge, 2x bread and 1 coin)"},
		{`{"kind":"labored","payload":{"employer":"Hannah","amount":5,"reward_items":"porridge"}}`, "(earned 5 coins working for Hannah)"},
		{`{"kind":"labored","payload":{"employer":"Hannah","amount":5,"reward_items":[{"item":"","qty":1},{"item":"porridge","qty":0},{"item":"porridge","qty":"x"}]}}`, "(earned 5 coins working for Hannah)"},
		{`{"kind":"labored","payload":{"employer":"Hannah","amount":0,"reward_items":[{"item":"","qty":1},{"item":"porridge","qty":1}]}}`, "(earned porridge working for Hannah)"},
		{`{"kind":"offered","payload":{}}`, ""},
		{`{"kind":"declined","payload":{}}`, ""},
		{`{"kind":"countered","payload":{}}`, ""},
		{`{"kind":"gathered","payload":{}}`, ""},
		{`{"kind":"stayed_open","payload":{}}`, ""},
		{`{"kind":"summoned","payload":{}}`, ""},
		{`{"kind":"some_future_beat","payload":{}}`, ""},
		{`{"kind":"done","payload":{}}`, ""},
		{`{"kind":"look_around","payload":{}}`, ""},
		{`{"kind":"enter_huddle","payload":{}}`, ""},
		{`{"kind":"brought_case","payload":{"case_id":"case-0000ab01","parties":["Josiah Thorne","Constable Gideon Marsh"],"complaint":"The ledger was taken\nfrom the Store."}}`,
			"(brought a matter before the magistrates in Salem Town, concerning Josiah Thorne, Constable Gideon Marsh: The ledger was taken from the Store.)"},
		{`{"kind":"brought_case","payload":{"complaint":"x"}}`, "(brought a matter before the magistrates in Salem Town: x)"},
		{`{"kind":"ruled","payload":{"text":"Word has come. The matter is closed.","result":"no_case"}}`, "(Word has come. The matter is closed.)"},
		{`{"kind":"ruled","payload":{}}`, ""},
		{`{"kind":"noticed_omen","payload":{"kind":"sour_milk","text":"found the milk turned sour in the pail"}}`, "(found the milk turned sour in the pail)"},
		{`{"kind":"noticed_omen","payload":{"kind":"cold_hearth","text":"found the hearth fire would not stay lit","subject":"Hannah Boggs"}}`,
			"(found the hearth fire would not stay lit, at Hannah Boggs's)"},
		{`{"kind":"noticed_omen","payload":{"kind":"sour_milk"}}`, ""},
		{`{"kind":"placed_ward","payload":{"item":"horseshoe","text":"nailed a horseshoe over the door at home"}}`, "(nailed a horseshoe over the door at home)"},
		{`{"kind":"placed_ward","payload":{"item":"horseshoe"}}`, ""},
	}
	for _, c := range cases {
		if got := narrate(t, c.event); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.event, got, c.want)
		}
	}
}

func TestUnmappedKindLoggedOnce(t *testing.T) {
	var logged []string
	u := &UnmappedKinds{Log: func(kind string) { logged = append(logged, kind) }}
	for _, kind := range []string{"gathered", "gathered", "stayed_open"} {
		v, _ := jsutil.Parse([]byte(`{"kind":"` + kind + `","payload":{}}`))
		if got, err := NarrateEvent(v.(*jsutil.Object), u); got != "" || err != nil {
			t.Errorf("%s: %q %v", kind, got, err)
		}
	}
	if strings.Join(logged, ",") != "gathered,stayed_open" {
		t.Errorf("logged %v", logged)
	}
}

func TestNormalizeSlugPrefix(t *testing.T) {
	a99 := strings.Repeat("a", 99)
	cases := []struct{ in, want string }{
		{"constance-scott/", "constance-scott/"}, {"john-ellis/", "john-ellis/"}, {"agent-7/", "agent-7/"},
		{"constance-scott", "constance-scott/"}, {"constance-scott//", "constance-scott/"}, {"  constance-scott/  ", "constance-scott/"},
		{"", ""}, {"   ", ""}, {"/", ""},
		{a99 + "/", a99 + "/"}, {a99 + "a/", ""}, {a99 + "////   ", a99 + "/"}, {a99 + "a////", ""},
		{"../", ""}, {"foo/../bar/", ""}, {"/etc/passwd", ""}, {"a/b/", ""},
		{"Constance-Scott/", ""}, {"a.b/", ""}, {"a_b/", ""}, {"two words/", ""},
		{strings.Repeat("/", 5*1024*1024), ""}, {strings.Repeat("a", 1025), ""}, {a99 + strings.Repeat("/", 500), a99 + "/"},
	}
	for _, c := range cases {
		if got := NormalizeSlugPrefix(c.in); got != c.want {
			t.Errorf("NormalizeSlugPrefix(%.40q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCollapseTrailingSlashes(t *testing.T) {
	hostile := strings.Repeat("/", 200000) + "a"
	start := time.Now()
	if got := CollapseTrailingSlashes(hostile); got != hostile+"/" {
		t.Error("hostile input changed")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("not linear: %v", elapsed)
	}
	for in, want := range map[string]string{
		"constance-scott/": "constance-scott/", "constance-scott": "constance-scott/", "constance-scott////": "constance-scott/",
		"/": "/", "////": "/", "": "/", "a/b": "a/b/", "a/b//": "a/b/",
	} {
		if got := CollapseTrailingSlashes(in); got != want {
			t.Errorf("CollapseTrailingSlashes(%q) = %q, want %q", in, got, want)
		}
	}
}

const (
	seed     = "Lewis Walker, who lodges at the Wayfarer with Hannah Walker."
	snapshot = "- [Jun 30] spoke: \"Good morrow.\"\n- [Jun 30] sold bread to a stranger."
)

func TestBuildSoulUserMessage(t *testing.T) {
	msg := BuildSoulUserMessage(SoulMessage{CharacterDescription: seed, DaySnapshot: snapshot, Day: "2026-06-30"})
	for _, want := range []string{"## Character description\n\nLewis Walker", "## Current soul document\n\n(empty — first run)",
		"## Dream snapshot for 2026-06-30\n\nThe current soul document is empty. Synthesize an initial soul", snapshot} {
		if !strings.Contains(msg, want) {
			t.Errorf("first run: no %q", want)
		}
	}
	if !strings.HasSuffix(msg, SoulLengthDirective) {
		t.Error("first run: no length directive at the end")
	}
	if msg := BuildSoulUserMessage(SoulMessage{CharacterDescription: seed, CurrentSoul: "   \n  ", DaySnapshot: snapshot, Day: "2026-06-30"}); !strings.Contains(msg, "(empty — first run)") || !strings.Contains(msg, "Synthesize an initial soul") {
		t.Error("blank soul is not a first run")
	}
	prior := "I am Lewis, a careful keeper who measures every coin."
	msg = BuildSoulUserMessage(SoulMessage{CharacterDescription: seed, CurrentSoul: prior, DaySnapshot: snapshot, Day: "2026-06-30"})
	if !strings.Contains(msg, "## Current soul document\n\n"+prior) || strings.Contains(msg, "(empty — first run)") ||
		strings.Contains(msg, "Synthesize an initial soul") || !strings.Contains(msg, "## Dream snapshot for 2026-06-30\n\n- [Jun 30] spoke") {
		t.Errorf("incremental: %q", msg)
	}
	msg = BuildSoulUserMessage(SoulMessage{CharacterDescription: seed, CurrentSoul: "prior soul", DaySnapshot: snapshot})
	if !strings.Contains(msg, "## Dream snapshot\n\n") || strings.Contains(msg, "## Dream snapshot for") {
		t.Errorf("no day: %q", msg)
	}
	msg = BuildSoulUserMessage(SoulMessage{CharacterDescription: "  " + seed + "  ", CurrentSoul: "  prior soul  ", DaySnapshot: "  " + snapshot + "  ", Day: "2026-06-30"})
	if !strings.Contains(msg, "## Character description\n\nLewis Walker") || !strings.Contains(msg, "## Current soul document\n\nprior soul\n\n") ||
		!strings.Contains(msg, snapshot) || !strings.HasSuffix(msg, SoulLengthDirective) || strings.Contains(msg, "\n\n\n") {
		t.Errorf("trimming: %q", msg)
	}
	c, s, d := strings.Index(msg, "## Character description"), strings.Index(msg, "## Current soul document"), strings.Index(msg, "## Dream snapshot")
	if !(c >= 0 && s > c && d > s) {
		t.Error("sections out of order")
	}
}
