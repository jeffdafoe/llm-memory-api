package virtualagent

import (
	"context"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsregex"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Speaker labels (Node's virtual-agent-labels.js) and the guards the
// discussion path runs on a generated reply. Agent names are untrusted:
// rows predate the current name rules, so a name is escaped before it goes
// into a pattern.

// canonicalSpeakerID is the comparison form of a speaker name: NFKC, so
// Unicode equivalents compare equal.
func canonicalSpeakerID(name string) string {
	return norm.NFKC.String(name)
}

// escapeRegExp escapes the characters JavaScript's escapeRegExp does.
func escapeRegExp(s string) string {
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune(`.*+?^${}()|[]\`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// collectDistinctSpeakers is the set of speaker labels in a discussion's
// history, plus the agent's own (so a self-prefixed reply is caught too),
// in first-seen order, without "".
func collectDistinctSpeakers(fromAgents []string, selfAgent string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		id := canonicalSpeakerID(name)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, name := range fromAgents {
		add(name)
	}
	if selfAgent != "" {
		add(selfAgent)
	}
	return out
}

// ImpersonationScan is scanForImpersonation's answer. Rule is
// "generic-json-continuation" or "legacy-name-prefix" when Truncated.
// DroppedChars counts UTF-16 units.
type ImpersonationScan struct {
	Text         string
	Truncated    bool
	Rule         string
	DroppedChars int
}

// impersonationBudget bounds one label match. The pattern is a literal
// alternation, so a real line needs a few steps per character.
const impersonationBudget = 1 << 26

// trimEnd is String.prototype.trimEnd.
func trimEnd(s string) string {
	return strings.TrimRightFunc(s, jsutil.IsSpace)
}

// startsJSONSender is /^\s*\{"sender"\s*:/.
func startsJSONSender(line string) bool {
	rest := strings.TrimLeftFunc(line, jsutil.IsSpace)
	if !strings.HasPrefix(rest, `{"sender"`) {
		return false
	}
	rest = strings.TrimLeftFunc(rest[len(`{"sender"`):], jsutil.IsSpace)
	return strings.HasPrefix(rest, ":")
}

var lineBreak = regexp.MustCompile(`\r?\n`)

// scanForImpersonation cuts a reply at the first line where the model
// starts another history entry: a line starting with {"sender": (any
// sender, even an invented one), or with a known speaker's label and a
// colon (the older "wendy: ..." continuation). Lines are compared in NFKC
// form; the cut is at the original line's offset.
func scanForImpersonation(response string, speakers []string) ImpersonationScan {
	if response == "" {
		return ImpersonationScan{Text: response}
	}
	var labels []string
	for _, s := range speakers {
		if e := escapeRegExp(s); e != "" {
			labels = append(labels, e)
		}
	}
	var legacy *jsregex.Regexp
	if len(labels) > 0 {
		legacy, _ = jsregex.CompileIgnoreCase(`^\s*(?:` + strings.Join(labels, "|") + `)\s*:`)
	}
	cut := func(cursor int, rule string) ImpersonationScan {
		truncated := trimEnd(response[:cursor])
		return ImpersonationScan{Text: truncated, Truncated: true, Rule: rule,
			DroppedChars: jsutil.UTF16Len(response) - jsutil.UTF16Len(truncated)}
	}
	cursor := 0
	breaks := lineBreak.FindAllStringIndex(response, -1)
	for i := 0; i <= len(breaks); i++ {
		end := len(response)
		next := end
		if i < len(breaks) {
			end, next = breaks[i][0], breaks[i][1]
		}
		normalized := norm.NFKC.String(response[cursor:end])
		if startsJSONSender(normalized) {
			return cut(cursor, "generic-json-continuation")
		}
		if legacy != nil {
			if ok, _ := legacy.MatchString(normalized, jsregex.NewBudget(context.Background(), impersonationBudget)); ok {
				return cut(cursor, "legacy-name-prefix")
			}
		}
		cursor = next
	}
	return ImpersonationScan{Text: response}
}

var clientStatus = regexp.MustCompile(` 4[0-9][0-9]`)

// isStopUnsupportedError is a provider 4xx that names the stop field: the
// one failure the discussion path retries without stop sequences. Any
// other error is a real failure, not masked by a retry.
func isStopUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !clientStatus.MatchString(msg) {
		return false
	}
	lower := jsutil.ToLower(msg)
	for _, marker := range []string{"stop_sequences", "stopsequences", `"stop"`, "invalid stop", "unsupported parameter"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
