package sim

import (
	"errors"
	"math"
	"strings"
	"sync"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// prop is v[key] on a parsed JSON value: a member of an object, else
// undefined (no field the distiller reads is a property of a primitive or
// of Object.prototype).
func prop(v any, key string) any {
	if o, ok := v.(*jsutil.Object); ok {
		return o.Value(key)
	}
	return jsutil.Undefined
}

// or is `a || b || …`: the first truthy value, else the last.
func or(vals ...any) any {
	for _, v := range vals[:len(vals)-1] {
		if jsutil.Truthy(v) {
			return v
		}
	}
	return vals[len(vals)-1]
}

// text is String(v || ”): "" for a falsy value, else String(v), with
// V8's TypeError for an object that cannot become a primitive.
func text(v any) (string, error) {
	if !jsutil.Truthy(v) {
		return "", nil
	}
	return jsutil.ValueString(v)
}

// collapse is .replace(/\s+/g, ' ').trim().
func collapse(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if jsutil.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return jsutil.Trim(b.String())
}

// SanitizeLabel is sanitizeLabel: a speaker, place or recipient with no
// brackets, double quotes or line breaks, so it cannot break the
// "[Day HH:MM Speaker]" line shape.
func SanitizeLabel(v any) (string, error) {
	s, err := text(v)
	if err != nil {
		return "", err
	}
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '[', ']', '"':
			return ' '
		}
		return r
	}, s)
	return collapse(s), nil
}

// sanitizeSpeech escapes backslashes and double quotes and keeps the
// speech on one line, so the closing quote of the wrapper cannot be
// pre-empted.
func sanitizeSpeech(v any) (string, error) {
	s, err := text(v)
	if err != nil {
		return "", err
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return collapse(s), nil
}

// sanitizeNarration keeps prose on one line and leaves its punctuation.
func sanitizeNarration(v any) (string, error) {
	s, err := text(v)
	if err != nil {
		return "", err
	}
	return collapse(s), nil
}

func number(v any) (float64, error) { return jsutil.ValueNumber(v) }

// formatCoins is "1 coin" or "<n> coins", n the floor of a positive
// finite amount, else 0.
func formatCoins(amount any) (string, error) {
	raw, err := number(amount)
	if err != nil {
		return "", err
	}
	n := 0.0
	if !math.IsNaN(raw) && !math.IsInf(raw, 0) && raw > 0 {
		n = math.Floor(raw)
	}
	if n == 1 {
		return "1 coin", nil
	}
	return jsutil.NumberString(n) + " coins", nil
}

// formatItemQty is "ale" (qty <= 1) or "3x bread".
func formatItemQty(item, qty any) (string, error) {
	name, err := SanitizeLabel(or(item, "something"))
	if err != nil {
		return "", err
	}
	n, err := number(qty)
	if err != nil {
		return "", err
	}
	if !math.IsNaN(n) && !math.IsInf(n, 0) && n > 1 {
		return jsutil.NumberString(math.Floor(n)) + "x " + name, nil
	}
	return name, nil
}

func isObject(v any) bool {
	switch v.(type) {
	case *jsutil.Object, []any:
		return true
	}
	return false
}

// formatLaborReward is a labor reward's terms: the in-kind goods
// (reward_items, each line a positive integer qty and a name that survives
// sanitizing) and the coins when positive, or the coins alone when there
// are no goods ("0 coins" for an empty reward), joined with commas and
// "and".
func formatLaborReward(p any) (string, error) {
	var parts []string
	if items, ok := prop(p, "reward_items").([]any); ok {
		for _, line := range items {
			if !jsutil.Truthy(line) || !isObject(line) {
				continue
			}
			item, err := SanitizeLabel(or(prop(line, "item"), ""))
			if err != nil {
				return "", err
			}
			qty, err := number(prop(line, "qty"))
			if err != nil {
				return "", err
			}
			if item == "" || qty != math.Trunc(qty) || math.IsInf(qty, 0) || math.IsNaN(qty) || qty < 1 {
				continue
			}
			s, err := formatItemQty(prop(line, "item"), prop(line, "qty"))
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
	}
	coins, err := number(prop(p, "amount"))
	if err != nil {
		return "", err
	}
	if (!math.IsNaN(coins) && !math.IsInf(coins, 0) && coins > 0) || len(parts) == 0 {
		s, err := formatCoins(prop(p, "amount"))
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	return joinAnd(parts), nil
}

// joinAnd is "a", "a and b", "a, b and c".
func joinAnd(parts []string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// chorePhrasing is the object literal Node looked chore types up in. A
// type named like a member of Object.prototype found that member, whose
// String() is its source text.
var chorePhrasing = map[string]string{
	"well":                 "fetched water from the well",
	"outhouse":             "visited the outhouse",
	"shop":                 "ran an errand at the shop",
	"constructor":          "function Object() { [native code] }",
	"hasOwnProperty":       "function hasOwnProperty() { [native code] }",
	"isPrototypeOf":        "function isPrototypeOf() { [native code] }",
	"propertyIsEnumerable": "function propertyIsEnumerable() { [native code] }",
	"toLocaleString":       "function toLocaleString() { [native code] }",
	"toString":             "function toString() { [native code] }",
	"valueOf":              "function valueOf() { [native code] }",
	"__defineGetter__":     "function __defineGetter__() { [native code] }",
	"__defineSetter__":     "function __defineSetter__() { [native code] }",
	"__lookupGetter__":     "function __lookupGetter__() { [native code] }",
	"__lookupSetter__":     "function __lookupSetter__() { [native code] }",
	"__proto__":            "[object Object]",
}

// UnmappedKinds remembers the action types already logged as having no
// narration, so each is logged once per process. Log is set by the
// caller.
type UnmappedKinds struct {
	mu   sync.Mutex
	seen map[string]bool
	Log  func(kind string)
}

func (u *UnmappedKinds) note(kind string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	first := !u.seen[kind]
	if u.seen == nil {
		u.seen = map[string]bool{}
	}
	u.seen[kind] = true
	u.mu.Unlock()
	if first && u.Log != nil {
		u.Log(kind)
	}
}

// NarrateEvent is narrateEvent: the transcript text of one engine event
// (speech in quotes, actions in parentheses), or "" for an event with no
// narrative (look_around, done, enter_huddle, an empty speech, and any
// action type without a mapping, which is logged once per kind).
func NarrateEvent(event *jsutil.Object, unmapped *UnmappedKinds) (string, error) {
	p := or(event.Value("payload"), jsutil.NewObject())
	kind, _ := event.Value("kind").(string)
	switch kind {
	case "move_to", "walked":
		dest, err := SanitizeLabel(or(prop(p, "destination"), prop(p, "structure_name"), "somewhere"))
		if err != nil {
			return "", err
		}
		return "(walked to " + dest + ")", nil
	case "chore":
		t, err := SanitizeLabel(or(prop(p, "type"), "an errand"))
		if err != nil {
			return "", err
		}
		if phrase, ok := chorePhrasing[t]; ok {
			return "(" + phrase + ")", nil
		}
		return "(ran an errand: " + t + ")", nil
	case "pay", "paid":
		recipient, err := SanitizeLabel(or(prop(p, "recipient"), prop(p, "recipient_name"), "someone"))
		if err != nil {
			return "", err
		}
		// LLM-607: a payment that discharged the town rate is narrated as
		// the due it was; the payer's own words for it are dropped.
		rate, err := number(prop(p, "rate_settled"))
		if err != nil {
			return "", err
		}
		coins, err := formatCoins(prop(p, "amount"))
		if err != nil {
			return "", err
		}
		if !math.IsNaN(rate) && !math.IsInf(rate, 0) && rate > 0 {
			return "(paid " + recipient + " " + coins + " — the town rate, a due settled, with no goods owed in return)", nil
		}
		forText, err := SanitizeLabel(or(prop(p, "for"), prop(p, "for_text"), ""))
		if err != nil {
			return "", err
		}
		reason := ""
		if forText != "" {
			reason = " for " + forText
		}
		return "(paid " + recipient + " " + coins + reason + ")", nil
	case "object_refresh":
		refreshes, _ := prop(p, "refreshes").([]any)
		objectName, err := SanitizeLabel(or(prop(p, "object_name"), "something"))
		if err != nil {
			return "", err
		}
		var verbs []string
		for _, r := range refreshes {
			if r == nil {
				return "", errors.New("Cannot read properties of null (reading 'attribute')")
			}
			attr := prop(r, "attribute")
			switch attr {
			case "thirst":
				verbs = append(verbs, "drank")
			case "hunger":
				verbs = append(verbs, "ate")
			case "tiredness":
				verbs = append(verbs, "rested")
			default:
				s, err := SanitizeLabel(attr)
				if err != nil {
					return "", err
				}
				verbs = append(verbs, "drew on "+s)
			}
		}
		if len(verbs) == 0 {
			return "(arrived at " + objectName + ")", nil
		}
		return "(" + joinAnd(verbs) + " at " + objectName + ")", nil
	case "speak", "spoke":
		s, err := sanitizeSpeech(or(prop(p, "text"), ""))
		if err != nil || s == "" {
			return "", err
		}
		return `"` + s + `"`, nil
	case "act":
		verb, err := sanitizeNarration(or(prop(p, "verb_phrase"), ""))
		if err != nil || verb == "" {
			return "", err
		}
		return "(" + verb + ")", nil
	case "delivered":
		recipient, err := SanitizeLabel(or(prop(p, "recipient"), "someone"))
		if err != nil {
			return "", err
		}
		goods, err := formatItemQty(prop(p, "item"), prop(p, "qty"))
		if err != nil {
			return "", err
		}
		amount, err := number(prop(p, "amount"))
		if err != nil {
			return "", err
		}
		coins := ""
		if amount > 0 {
			c, err := formatCoins(prop(p, "amount"))
			if err != nil {
				return "", err
			}
			coins = " for " + c
		}
		return "(delivered " + goods + " to " + recipient + coins + ")", nil
	case "consumed":
		goods, err := formatItemQty(prop(p, "item"), prop(p, "qty"))
		if err != nil {
			return "", err
		}
		return "(had " + goods + ")", nil
	case "took_break":
		reason, err := sanitizeNarration(or(prop(p, "reason"), ""))
		if err != nil {
			return "", err
		}
		if reason != "" {
			return "(stepped away, " + reason + ")", nil
		}
		return "(stepped away)", nil
	case "labored", "solicited_work":
		employer, err := SanitizeLabel(or(prop(p, "employer"), prop(p, "recipient"), ""))
		if err != nil {
			return "", err
		}
		if employer == "" {
			employer = "someone"
		}
		reward, err := formatLaborReward(p)
		if err != nil {
			return "", err
		}
		if kind == "labored" {
			return "(earned " + reward + " working for " + employer + ")", nil
		}
		return "(offered to work for " + employer + " for " + reward + ")", nil
	case "offered_work", "hired":
		worker, err := SanitizeLabel(or(prop(p, "worker"), prop(p, "recipient"), ""))
		if err != nil {
			return "", err
		}
		if worker == "" {
			worker = "someone"
		}
		reward, err := formatLaborReward(p)
		if err != nil {
			return "", err
		}
		if kind == "offered_work" {
			return "(offered " + worker + " a job for " + reward + ")", nil
		}
		return "(hired " + worker + " for " + reward + ")", nil
	case "brought_case":
		complaint, err := sanitizeNarration(or(prop(p, "complaint"), ""))
		if err != nil {
			return "", err
		}
		var parties []string
		if list, ok := prop(p, "parties").([]any); ok {
			for _, party := range list {
				s, err := SanitizeLabel(party)
				if err != nil {
					return "", err
				}
				if s != "" {
					parties = append(parties, s)
				}
			}
		}
		concerning := ""
		if len(parties) > 0 {
			concerning = ", concerning " + strings.Join(parties, ", ")
		}
		if complaint != "" {
			complaint = ": " + complaint
		}
		return "(brought a matter before the magistrates in Salem Town" + concerning + complaint + ")", nil
	case "ruled", "placed_ward":
		s, err := sanitizeNarration(or(prop(p, "text"), ""))
		if err != nil || s == "" {
			return "", err
		}
		return "(" + s + ")", nil
	case "noticed_omen":
		s, err := sanitizeNarration(or(prop(p, "text"), ""))
		if err != nil || s == "" {
			return "", err
		}
		subject, err := SanitizeLabel(or(prop(p, "subject"), ""))
		if err != nil {
			return "", err
		}
		if subject != "" {
			s += ", at " + subject + "'s"
		}
		return "(" + s + ")", nil
	case "enter_huddle", "look_around", "done":
		return "", nil
	}
	unmapped.note(kind)
	return "", nil
}

// soulMessage is the /sim/soul writer's user message.
type SoulMessage struct {
	CharacterDescription, CurrentSoul, DaySnapshot, Day string
}

// BuildSoulUserMessage is sim-soul.js's buildSoulUserMessage: the same
// sections the nightly soul pass uses, with the first-run framing when the
// current soul is empty, and the length directive.
func BuildSoulUserMessage(m SoulMessage) string {
	empty := jsutil.Trim(m.CurrentSoul) == ""
	header := "## Dream snapshot"
	if m.Day != "" {
		header = "## Dream snapshot for " + m.Day
	}
	current := "(empty — first run)"
	if !empty {
		current = jsutil.Trim(m.CurrentSoul)
	}
	msg := "## Character description\n\n" + jsutil.Trim(m.CharacterDescription) + "\n\n" +
		"## Current soul document\n\n" + current + "\n\n" + header + "\n\n"
	if empty {
		msg += "The current soul document is empty. Synthesize an initial soul " +
			"from the day's material below; do not treat this as a single-day " +
			"incremental update.\n\n"
	}
	return msg + jsutil.Trim(m.DaySnapshot) + "\n\n" + SoulLengthDirective
}
