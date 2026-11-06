package dream

import (
	"context"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsregex"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/peopleslug"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sim"
)

// signalSources are SIGNAL_PATTERNS: the markers of memory-worthy talk.
// The prefilter keeps the lines around them. All carry the i flag.
var signalSources = []string{
	// Explicit memory requests
	`\bremember\b`,
	`\bdon'?t forget\b`,
	`\bnote that\b`,
	`\bkeep in mind\b`,
	// Corrections and feedback
	`\bdon'?t do that\b`,
	`\bstop doing\b`,
	`\bnot like that\b`,
	`\bwrong\b`,
	`\binstead\b`,
	`\bactually\b`,
	`\bno,?\s`,
	// Preferences and decisions
	`\bfrom now on\b`,
	`\balways\b`,
	`\bnever\b`,
	`\bprefer\b`,
	`\bI like\b`,
	`\bI hate\b`,
	`\bI want\b`,
	// Temporal / deadline signals
	`\bdeadline\b`,
	`\bby\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday|tomorrow|next week)\b`,
	`\bdue\s+(date|by)\b`,
	// Reasons and context
	`\bthe reason\b`,
	`\bbecause\b`,
	`\bimportant\b`,
	// Emotional signals (for companion mode)
	`\bfeeling\b`,
	`\bworried\b`,
	`\bexcited\b`,
	`\bfrustrat`,
	`\bhappy\b`,
	`\bsad\b`,
	`\bstress`,
	`\banxious`,
	`\bthank you\b`,
	`\blove\b`,
	`\bmiss\b`,
}

// signalPatterns run on jsregex, the JavaScript matcher: under the i flag
// without u, JavaScript folds only ASCII letters for these patterns, and
// its \s is wider than RE2's.
var signalPatterns = func() []*jsregex.Regexp {
	out := make([]*jsregex.Regexp, len(signalSources))
	for i, src := range signalSources {
		re, err := jsregex.CompileIgnoreCase(src)
		if err != nil {
			panic("dream: signal pattern " + src + ": " + err.Error())
		}
		out[i] = re
	}
	return out
}()

// signalBudget bounds one line's matching. The patterns are linear, so it
// is never reached in practice; a line that exhausts it counts as having
// no signal (Known differences).
const signalBudget = 1 << 26

func hasSignal(line string) bool {
	for _, re := range signalPatterns {
		ok, err := re.MatchString(line, jsregex.NewBudget(context.Background(), signalBudget))
		if err == nil && ok {
			return true
		}
	}
	return false
}

// contextLines are the lines kept before and after a signal.
const contextLines = 5

// reasoningPreambleMarkers are the chain-of-thought openers that keep a
// soul from being saved (detectReasoningPreamble). Lowercase substrings.
var reasoningPreambleMarkers = []string{
	"thinking process",
	"<thinking",
	"<think",
	"1. **analyze",
	"step 1:",
	"let me analyze",
	"let me think",
	"i'll analyze",
	"first, i'll",
	"first, let me",
	"## analysis",
}

// detectReasoningPreamble returns the first marker found in the first 200
// characters of the trimmed text, or "" when they look like clean output.
func detectReasoningPreamble(text string) string {
	if text == "" {
		return ""
	}
	lead := jsutil.ToLower(jsutil.TruncateUTF16(jsutil.Trim(text), 200))
	for _, m := range reasoningPreambleMarkers {
		if strings.Contains(lead, m) {
			return m
		}
	}
	return ""
}

// soulNeedsRebuild: the soul is empty, or shorter than minChars (a
// truncated write). minChars <= 0 turns the length check off.
func soulNeedsRebuild(existingSoul string, minChars float64) bool {
	trimmed := jsutil.Trim(existingSoul)
	if trimmed == "" {
		return true
	}
	return minChars > 0 && float64(jsutil.UTF16Len(trimmed)) < minChars
}

type soulMessage struct {
	agentName, startupInstructions, existingSoul string
	needsRebuild                                 bool
	backloadDreams                               string
	chunkDate, dreamContent                      string
}

// buildSoulUserMessage is the soul writer's user message. A rebuild
// withholds the prior soul, so a degraded stub never reaches the writer;
// with a dream backload it also asks for a from-scratch synthesis.
func buildSoulUserMessage(m soulMessage) string {
	var b strings.Builder
	b.WriteString("## Agent: " + m.agentName + "\n\n")
	if m.startupInstructions != "" {
		b.WriteString("## Character description\n\n" + m.startupInstructions + "\n\n")
	}
	b.WriteString("## Current soul document\n\n")
	if m.needsRebuild {
		b.WriteString("(none on file — rebuilding from recent dreams)")
	} else {
		b.WriteString(m.existingSoul)
	}
	if m.backloadDreams != "" {
		b.WriteString("\n\n## Dream snapshot for initial soul rebuild\n\n" +
			"There is no usable prior soul document. Synthesize an initial soul from the recent dream history below; do not treat this as a single-day incremental update.\n\n" +
			m.backloadDreams)
	} else {
		b.WriteString("\n\n## Dream snapshot for " + m.chunkDate + "\n\n" + m.dreamContent)
	}
	b.WriteString("\n\n" + sim.SoulLengthDirective)
	return b.String()
}

func isLineTerminator(r rune) bool {
	return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029
}

func isASCIIWord(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

// spaceEnd is the end of the run of JavaScript whitespace starting at i.
func spaceEnd(s string, i int) int {
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !jsutil.IsSpace(r) {
			break
		}
		i += size
	}
	return i
}

// nonSpaceEnd is the end of the run of non-whitespace starting at i.
func nonSpaceEnd(s string, i int) int {
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if jsutil.IsSpace(r) {
			break
		}
		i += size
	}
	return i
}

func wordEnd(s string, i int) int {
	for i < len(s) && isASCIIWord(s[i]) {
		i++
	}
	return i
}

func allSpace(s string) bool { return spaceEnd(s, 0) == len(s) }

// jsonArrayHead is /^\s*\[\s*\{\s*"sender"\s*:/: the typed-context
// conversation record, one JSON array on one line.
func jsonArrayHead(line string) bool {
	i := spaceEnd(line, 0)
	for _, want := range []string{"[", "{", `"sender"`, ":"} {
		if !strings.HasPrefix(line[i:], want) {
			return false
		}
		i += len(want)
		if want != ":" {
			i = spaceEnd(line, i)
		}
	}
	return true
}

// ledgerLine is LEDGER_LINE, /^\[[^\]]+\]\s+\(.*\)\s*$/: an engine-written
// record of something that happened, "[Friday 18:26 Constance Scott] (paid
// Josiah Thorne 1 coin for milk)". Speech is quoted; only the distiller
// writes the parentheses, so they are the reliable mark.
func ledgerLine(line string) bool {
	if !strings.HasPrefix(line, "[") {
		return false
	}
	closeAt := strings.IndexByte(line[1:], ']')
	if closeAt < 1 {
		return false
	}
	p := 1 + closeAt + 1
	q := spaceEnd(line, p)
	if q == p || !strings.HasPrefix(line[q:], "(") {
		return false
	}
	rest := line[q+1:]
	paren := strings.LastIndexByte(rest, ')')
	if paren < 0 || !allSpace(rest[paren+1:]) {
		return false
	}
	return !strings.ContainsFunc(rest[:paren], isLineTerminator)
}

// prefilterLog keeps the signal lines of a log with contextLines around
// each, the typed-context JSON arrays and the ledger lines, joining
// skipped stretches with "  [...]". ok is false when the log has neither a
// signal nor a ledger line.
func prefilterLog(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	signal := map[int]bool{}
	var signalOrder []int
	var ledger []int
	for i, line := range lines {
		if hasSignal(line) {
			signal[i] = true
			signalOrder = append(signalOrder, i)
		}
	}
	for i, line := range lines {
		if ledgerLine(line) {
			ledger = append(ledger, i)
		}
	}
	if len(signal) == 0 && len(ledger) == 0 {
		return "", false
	}
	included := make([]bool, len(lines))
	for _, idx := range signalOrder {
		start := max(0, idx-contextLines)
		end := min(len(lines)-1, idx+contextLines)
		for i := start; i <= end; i++ {
			included[i] = true
		}
	}
	for i, line := range lines {
		if jsonArrayHead(line) {
			included[i] = true
		}
	}
	for _, idx := range ledger {
		included[idx] = true
	}
	var out []string
	last := -2
	for i, line := range lines {
		if !included[i] {
			continue
		}
		if i > last+1 && last >= 0 {
			out = append(out, "  [...]")
		}
		out = append(out, line)
		last = i
	}
	return strings.Join(out, "\n"), true
}

// noteRow is one row of the notes-mode source query.
type noteRow struct {
	slug, content string
	updatedAt     time.Time
}

// buildNotesLog is the notes-mode dream source: each curated note under a
// "## Note: <slug> (updated <date>)" header, in query order.
func buildNotesLog(rows []noteRow) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = "## Note: " + r.slug + " (updated " + utcDate(r.updatedAt) + ")\n\n" + r.content
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// utcDate is toISOString().slice(0, 10).
func utcDate(t time.Time) string {
	return jsutil.DateISO(t)[:10]
}

// speaker is one person in a day's log: the display name for prompts and
// the lines grouped under them.
type speaker struct {
	slug, display string
	lines         []string
}

// speakers keeps a Map's insertion order.
type speakers struct {
	order []string
	bySlg map[string]*speaker
}

func newSpeakers() *speakers { return &speakers{bySlg: map[string]*speaker{}} }

func (s *speakers) get(slug string) *speaker { return s.bySlg[slug] }

func (s *speakers) all() []*speaker {
	out := make([]*speaker, len(s.order))
	for i, slug := range s.order {
		out[i] = s.bySlg[slug]
	}
	return out
}

// simHeader is /^\[\w+day\s+\d{2}:\d{2}\s+([^\]]+?)\]/, the sim-day
// distiller's "[Weekday HH:MM Display Name]", returning the name.
func simHeader(line string) (string, bool) {
	if !strings.HasPrefix(line, "[") {
		return "", false
	}
	w := wordEnd(line, 1)
	if w-1 < 4 || line[w-3:w] != "day" {
		return "", false
	}
	p := spaceEnd(line, w)
	if p == w || !clock(line, p, 2) {
		return "", false
	}
	nameAt := spaceEnd(line, p+5)
	if nameAt == p+5 {
		return "", false
	}
	q := strings.IndexByte(line[nameAt:], ']')
	if q < 0 {
		return "", false
	}
	if q > 0 {
		return line[nameAt : nameAt+q], true
	}
	// The lazy name must take at least one character: with the run of
	// whitespace followed at once by "]", \s+ gives its last character back.
	_, size := utf8.DecodeLastRuneInString(line[:nameAt])
	if nameAt-size == p+5 {
		return "", false
	}
	return line[nameAt-size : nameAt], true
}

// clock is \d{n}:\d{2} at i (n is 2, or 1 for one digit).
func clock(s string, i, n int) bool {
	if len(s) < i+n+3 {
		return false
	}
	for k := 0; k < n; k++ {
		if !isASCIIDigit(s[i+k]) {
			return false
		}
	}
	return s[i+n] == ':' && isASCIIDigit(s[i+n+1]) && isASCIIDigit(s[i+n+2])
}

// chatHeader is /^\[(\d{2}:\d{2})\s+(\S+)\]/, memory-sync's "[HH:MM
// speaker]", returning the speaker. \S+ backtracks to the last "]" of the
// run that leaves it a character.
func chatHeader(line string) (string, bool) {
	if !strings.HasPrefix(line, "[") || !clock(line, 1, 2) {
		return "", false
	}
	p := spaceEnd(line, 6)
	if p == 6 {
		return "", false
	}
	run := line[p:nonSpaceEnd(line, p)]
	j := strings.LastIndexByte(run, ']')
	if j < 1 {
		return "", false
	}
	return run[:j], true
}

// fromHeader is /^-\s+\*\*From:\*\*\s+(\S+)/, a VA transcript's sender.
func fromHeader(line string) (string, bool) {
	if !strings.HasPrefix(line, "-") {
		return "", false
	}
	p := spaceEnd(line, 1)
	if p == 1 || !strings.HasPrefix(line[p:], "**From:**") {
		return "", false
	}
	p += len("**From:**")
	q := spaceEnd(line, p)
	if q == p {
		return "", false
	}
	end := nonSpaceEnd(line, q)
	if end == q {
		return "", false
	}
	return line[q:end], true
}

// agentLabel is ([a-z][a-z0-9-]*):(?:\s|$) at i.
func agentLabel(s string, i int) (string, bool) {
	if i >= len(s) || s[i] < 'a' || s[i] > 'z' {
		return "", false
	}
	j := i + 1
	for j < len(s) && (s[j] >= 'a' && s[j] <= 'z' || isASCIIDigit(s[j]) || s[j] == '-') {
		j++
	}
	if j >= len(s) || s[j] != ':' {
		return "", false
	}
	if j+1 < len(s) {
		r, _ := utf8.DecodeRuneInString(s[j+1:])
		if !jsutil.IsSpace(r) {
			return "", false
		}
	}
	return s[i:j], true
}

// discussionHeader is /^(?:\[.*?\]\s+)?([a-z][a-z0-9-]*):(?:\s|$)/:
// "speaker: text" with an optional "[timestamp] " before it.
func discussionHeader(line string) (string, bool) {
	if strings.HasPrefix(line, "[") {
		for i := 1; i < len(line); {
			r, size := utf8.DecodeRuneInString(line[i:])
			if isLineTerminator(r) {
				break
			}
			if r == ']' {
				p := spaceEnd(line, i+1)
				if p > i+1 {
					if name, ok := agentLabel(line, p); ok {
						return name, true
					}
				}
			}
			i += size
		}
	}
	return agentLabel(line, 0)
}

// extractSpeakers groups a day's lines by who said them, in first-seen
// order, from the five formats the logs carry: memory-sync "[HH:MM who]",
// VA transcript "- **From:** who", discussion "who: text", the
// typed-context JSON array, and the sim distiller's "[Weekday HH:MM Name]".
// Lines without a header continue the current speaker. The agent's own
// lines are dropped, so it never gets a people note about itself; a salem
// "zbbs-" agent name is stripped first, so "zbbs-john-ellis" matches "John
// Ellis".
func extractSpeakers(content, agentName string) *speakers {
	out := newSpeakers()
	self := peopleslug.Slug(strings.TrimPrefix(agentName, "zbbs-"))
	current := ""

	add := func(display string, line *string) {
		slug := peopleslug.Slug(display)
		if slug == "" || slug == self {
			current = ""
			return
		}
		current = slug
		if out.get(slug) == nil {
			out.order = append(out.order, slug)
			out.bySlg[slug] = &speaker{slug: slug, display: display}
		}
		if line != nil {
			sp := out.get(slug)
			sp.lines = append(sp.lines, *line)
		}
	}

	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "##") || strings.HasPrefix(line, "---") {
			continue
		}
		if jsonArrayHead(line) {
			if messages, ok := parseJSONArray(jsutil.Trim(line)); ok {
				for _, msg := range messages {
					obj, isObj := msg.(*jsutil.Object)
					if !isObj {
						continue
					}
					sender, isString := obj.Value("sender").(string)
					if !isString {
						continue
					}
					text, _ := obj.Value("content").(string)
					l := "[" + sender + "] " + text
					add(sender, &l)
				}
				current = ""
				continue
			}
		}
		if name, ok := simHeader(line); ok {
			add(jsutil.Trim(name), &line)
			continue
		}
		if name, ok := chatHeader(line); ok {
			add(name, &line)
			continue
		}
		if name, ok := fromHeader(line); ok {
			add(name, nil)
			continue
		}
		if name, ok := discussionHeader(line); ok {
			add(name, &line)
			continue
		}
		if current != "" && jsutil.Trim(line) != "" {
			if sp := out.get(current); sp != nil {
				sp.lines = append(sp.lines, line)
			}
		}
	}
	return out
}

// parseJSONArray is JSON.parse(text) when it gives an array.
func parseJSONArray(text string) ([]any, bool) {
	v, err := jsutil.Parse([]byte(text))
	if err != nil {
		return nil, false
	}
	a, ok := v.([]any)
	return a, ok
}

// nameMatcher is nameMatcher(fragment).test: the fragment as a whole word,
// case-insensitive under the u flag (simple case folding), with letters,
// numbers and "_" as the word characters on either side. A display name
// like "Zoë Marsh" therefore does not match inside a longer word.
type nameMatcher struct{ frag []rune }

func newNameMatcher(fragment string) nameMatcher {
	return nameMatcher{frag: []rune(fragment)}
}

func isNameRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func foldEqual(a, b rune) bool {
	if a == b {
		return true
	}
	for r := unicode.SimpleFold(a); r != a; r = unicode.SimpleFold(r) {
		if r == b {
			return true
		}
	}
	return false
}

func (m nameMatcher) test(text string) bool {
	runes := []rune(text)
	for start := 0; start <= len(runes); start++ {
		if start > 0 && isNameRune(runes[start-1]) {
			continue
		}
		end := start + len(m.frag)
		if end > len(runes) {
			break
		}
		ok := true
		for k, r := range m.frag {
			if !foldEqual(runes[start+k], r) {
				ok = false
				break
			}
		}
		if ok && (end == len(runes) || !isNameRune(runes[end])) {
			return true
		}
	}
	return false
}

// firstWord is text.split(/\s+/)[0].
func firstWord(text string) string {
	for i, r := range text {
		if jsutil.IsSpace(r) {
			return text[:i]
		}
	}
	return text
}

// ledgerLineSpeaker reads the speaker out of a line's bracket header,
// dropping a leading weekday and clock time: "[Friday 18:26 Constance
// Scott]" and "[18:26 home]" both work. "" when there is no header. It is
// a best-effort read; a header it cannot reduce falls back to the names in
// the line.
func ledgerLineSpeaker(line string) string {
	if !strings.HasPrefix(line, "[") {
		return ""
	}
	closeAt := strings.IndexByte(line[1:], ']')
	if closeAt < 1 {
		return ""
	}
	h := line[1 : 1+closeAt]
	// .replace(/^\w+day\s+/i, '')
	if w := wordEnd(h, 0); w >= 4 && asciiHasPrefixFold(h[w-3:w], "day") {
		if p := spaceEnd(h, w); p > w {
			h = h[p:]
		}
	}
	// .replace(/^\d{1,2}:\d{2}\s+/, '')
	for _, n := range []int{2, 1} {
		if clock(h, 0, n) {
			if p := spaceEnd(h, n+3); p > n+3 {
				h = h[p:]
			}
			break
		}
	}
	return jsutil.Trim(h)
}

// personSection is one person's day: the ledger lines (what happened) and
// the speech (what was said).
type personSection struct {
	slug, display string
	ledger, said  []string
}

type person struct {
	slug, display string
	full          nameMatcher
	first         *nameMatcher
}

// buildPersonExcerptSections splits each person's day into ledger and
// speech for the people writer. The villager's own ledger lines, which
// extractSpeakers drops, are filed under each person they name; a ledger
// line with another speaker's header is filed under that speaker. Speech
// that names another of the day's people (by full name, or by a first name
// no one else shares) and not the villager is marked as overheard.
func buildPersonExcerptSections(filtered, selfName string, sp *speakers) []*personSection {
	all := sp.all()
	sections := make([]*personSection, len(all))
	bySlug := map[string]*personSection{}
	for i, s := range all {
		sections[i] = &personSection{slug: s.slug, display: s.display}
		bySlug[s.slug] = sections[i]
	}

	selfDisplay := sim.SlugToDisplay(selfName)
	selfSlug := peopleslug.Slug(strings.TrimPrefix(selfName, "zbbs-"))

	firstCounts := map[string]int{}
	for _, s := range all {
		firstCounts[firstWord(s.display)]++
	}
	people := make([]person, len(all))
	for i, s := range all {
		p := person{slug: s.slug, display: s.display, full: newNameMatcher(s.display)}
		if first := firstWord(s.display); firstCounts[first] == 1 {
			m := newNameMatcher(first)
			p.first = &m
		}
		people[i] = p
	}

	var selfFull, selfFirst *nameMatcher
	if selfDisplay != "" {
		m := newNameMatcher(selfDisplay)
		selfFull = &m
		if first := firstWord(selfDisplay); !hasKey(firstCounts, first) {
			f := newNameMatcher(first)
			selfFirst = &f
		}
	}
	namesPerson := func(text string, p person) bool {
		if p.full.test(text) {
			return true
		}
		return p.first != nil && p.first.test(text)
	}
	namesSelf := func(text string) bool {
		if selfFull != nil && selfFull.test(text) {
			return true
		}
		return selfFirst != nil && selfFirst.test(text)
	}

	for _, line := range strings.Split(filtered, "\n") {
		if !ledgerLine(line) {
			continue
		}
		speakerSlug := peopleslug.Slug(ledgerLineSpeaker(line))
		if speakerSlug != "" && speakerSlug != selfSlug && bySlug[speakerSlug] != nil {
			bySlug[speakerSlug].ledger = append(bySlug[speakerSlug].ledger, line)
			continue
		}
		for _, p := range people {
			if p.full.test(line) {
				bySlug[p.slug].ledger = append(bySlug[p.slug].ledger, line)
			}
		}
	}

	for _, s := range all {
		section := bySlug[s.slug]
		for _, line := range s.lines {
			if ledgerLine(line) {
				continue
			}
			addressee := ""
			for _, p := range people {
				if p.slug != s.slug && namesPerson(line, p) {
					addressee = p.display
					break
				}
			}
			if addressee != "" && !namesSelf(line) {
				section.said = append(section.said, line+"  (overheard — addressed to "+addressee+")")
				continue
			}
			section.said = append(section.said, line)
		}
	}
	return sections
}

// slugify is the dream note's title slug: lower-cased, runs of anything
// but [a-z0-9] turned into "-", edge hyphens dropped, 60 characters.
func slugify(text string) string {
	var b strings.Builder
	dash := false
	for _, r := range jsutil.ToLower(text) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
		}
		dash = true
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// dreamTitle is the title Node took from the dream text: the first
// /^#\s+(.+)$/m match, else the first /^title:\s*(.+)$/im match, trimmed;
// "Dream consolidation" when neither matches.
func dreamTitle(text string) string {
	if t, ok := lineMatch(text, "#", false, 1); ok {
		return jsutil.Trim(t)
	}
	if t, ok := lineMatch(text, "title:", true, 0); ok {
		return jsutil.Trim(t)
	}
	return "Dream consolidation"
}

// lineMatch finds the first line start (under the m flag: the start, or
// after \n, \r, U+2028, U+2029) where prefix is followed by at least
// minSpace JavaScript whitespace characters and then .+ to the end of the
// line, and returns the captured .+. \s can cross line ends, so after the
// greedy whitespace run it gives characters back until .+ can take one.
// fold matches the prefix ASCII case-insensitively (the i flag without u
// folds only ASCII here).
func lineMatch(text, prefix string, fold bool, minSpace int) (string, bool) {
	for start := 0; start <= len(text); {
		if strings.HasPrefix(text[start:], prefix) ||
			fold && asciiHasPrefixFold(text[start:], prefix) {
			if capture, ok := afterPrefix(text, start+len(prefix), minSpace); ok {
				return capture, true
			}
		}
		next := indexLineStart(text, start)
		if next < 0 {
			break
		}
		start = next
	}
	return "", false
}

func hasKey(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
}

// asciiHasPrefixFold reports whether s starts with prefix, ASCII letters
// compared case-insensitively and nothing else folded.
func asciiHasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		a, b := s[i], prefix[i]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

// indexLineStart is the next line start after the one at start, or -1.
func indexLineStart(text string, start int) int {
	for i := start; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size
		if isLineTerminator(r) {
			return i
		}
	}
	return -1
}

func afterPrefix(text string, p, minSpace int) (string, bool) {
	var runStarts []int
	end := p
	for end < len(text) {
		r, size := utf8.DecodeRuneInString(text[end:])
		if !jsutil.IsSpace(r) {
			break
		}
		runStarts = append(runStarts, end)
		end += size
	}
	if len(runStarts) < minSpace {
		return "", false
	}
	// Try the longest whitespace prefix first, as the greedy \s does.
	for k := len(runStarts); k >= minSpace; k-- {
		at := end
		if k < len(runStarts) {
			at = runStarts[k]
		}
		if at >= len(text) {
			continue
		}
		r, _ := utf8.DecodeRuneInString(text[at:])
		if isLineTerminator(r) {
			continue
		}
		stop := at
		for stop < len(text) {
			r, size := utf8.DecodeRuneInString(text[stop:])
			if isLineTerminator(r) {
				break
			}
			stop += size
		}
		return text[at:stop], true
	}
	return "", false
}

// chunk is one (from, to] window of a dream run.
type chunk struct{ from, to time.Time }

const dayMs = 24 * 60 * 60 * 1000

// computeDailyChunks splits (since, now] at UTC midnights. The first chunk
// starts at since, not at its day's start, so a run never rescans logs an
// earlier run consumed; the last ends at now.
func computeDailyChunks(since, now time.Time) []chunk {
	sinceMs, nowMs := since.UnixMilli(), now.UnixMilli()
	if sinceMs >= nowMs {
		return nil
	}
	s := since.UTC()
	dayEnd := time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, time.UTC).UnixMilli() + dayMs
	var out []chunk
	for cursor := sinceMs; cursor < nowMs; dayEnd += dayMs {
		end := min(dayEnd, nowMs)
		out = append(out, chunk{time.UnixMilli(cursor).UTC(), time.UnixMilli(end).UTC()})
		cursor = end
	}
	return out
}

// jsDate is a timestamptz as node-postgres built its Date: the fraction
// times 1000 in float64, truncated to whole milliseconds.
func jsDate(t time.Time) time.Time {
	t = t.UTC()
	us := t.Nanosecond() / 1000
	ms := int(math.Trunc(1000 * (float64(us) / 1e6)))
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), ms*int(time.Millisecond), time.UTC)
}
