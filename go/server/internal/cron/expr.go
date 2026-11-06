// Package cron is node-cron 4.6.0's schedule handling, ported so the Go
// server accepts, refuses and fires the same `*_cron_schedule` values as the
// Node server did: validate(), the expression conversion, the time matcher
// with its next-match walk, and the runner's timing rules.
//
// It is ported rather than taken from a module because the common Go cron
// libraries read expressions differently (day-of-month OR day-of-week where
// node-cron ANDs them, no L / W / # tokens, other refusals), which would
// change which schedules run and when.
package cron

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// token is one comma-separated value of a converted field, as node-cron's
// normalizeIntegers left it: a number (from a run of digits) or a string
// (L, LW, 15W, L-2, 5L, 1#2, or anything it could not read).
type token struct {
	isNum bool
	num   float64
	str   string
}

func (t token) String() string {
	if t.isNum {
		return jsutil.NumberString(t.num)
	}
	return t.str
}

var nicknames = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// resolveNickname is node-cron's: the trimmed, lower-cased expression looked
// up as a nickname, else the expression unchanged (untrimmed).
func resolveNickname(expression string) string {
	if resolved, ok := nicknames[strings.ToLower(jsutil.Trim(expression))]; ok {
		return resolved
	}
	return expression
}

var spaceRuns = regexp.MustCompile(`\s{2,}`)

// splitFields collapses runs of two or more whitespace characters to one
// space, trims, and splits on single spaces, as node-cron does in both its
// validator and its converter.
func splitFields(s string) []string {
	return strings.Split(jsutil.Trim(spaceRuns.ReplaceAllString(s, " ")), " ")
}

var (
	monthNames      = []string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}
	shortMonthNames = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	weekdayNames    = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
	shortDayNames   = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
)

// replaceNames is node-cron's `expression.replace(new RegExp(name, 'gi'), n)`
// for each name in turn, n being the name's index plus offset. The names are
// ASCII letters, and a field that reaches this point is ASCII (the validator
// refuses anything else first), so a lower-cased copy has the same offsets.
func replaceNames(expression string, names []string, offset int) string {
	for i, name := range names {
		lower := strings.ToLower(expression)
		if !strings.Contains(lower, name) {
			continue
		}
		var b strings.Builder
		pos := 0
		for {
			j := strings.Index(lower[pos:], name)
			if j < 0 {
				break
			}
			b.WriteString(expression[pos : pos+j])
			b.WriteString(strconv.Itoa(i + offset))
			pos += j + len(name)
		}
		b.WriteString(expression[pos:])
		expression = b.String()
	}
	return expression
}

var fieldBounds = [6]struct{ min, max float64 }{
	{0, 59}, {0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6},
}

var asteriskRanges = [6]string{"0-59", "0-59", "0-23", "1-31", "1-12", "0-6"}

var rangeRe = regexp.MustCompile(`^(\d+)-(\d+)(?:/(\d+))?$`)

// expandRange is node-cron's: "a-b/s" becomes the listed values, wrapping
// past the field's maximum when a > b ("22-2" hours is 22,23,0,1,2). A step
// below 1 leaves the text as it was, which the validator then refuses.
//
// node-cron builds the whole list, so a range like "0-99999999999" made it
// loop for minutes. A value the validator will refuse makes the field invalid
// whatever else it holds, so the loop stops at the first one and keeps it,
// which gives the same verdict and the same accepted values. That is a value
// above the field's maximum, except in the weekday field, where 7 is Sunday
// and only a value above 7 is refused.
func expandRange(initTxt, endTxt, stepTxt string, field int) []string {
	step, _ := jsutil.ParseInt(stepTxt)
	if !(step >= 1) {
		return []string{initTxt + "-" + endTxt + "/" + stepTxt}
	}
	first, _ := jsutil.ParseInt(initTxt)
	last, _ := jsutil.ParseInt(endTxt)
	bounds := fieldBounds[field]
	refusedAbove := bounds.max
	if field == 5 {
		refusedAbove = 7
	}
	var out []string
	if first <= last {
		for i := first; i <= last; i += step {
			out = append(out, jsutil.NumberString(i))
			if i > refusedAbove {
				break
			}
		}
		return out
	}
	size := bounds.max - bounds.min + 1
	span := math.Mod(math.Mod(last-first, size)+size, size)
	for offset := 0.0; offset <= span; offset += step {
		value := first + offset
		if value > bounds.max {
			value -= size
		}
		out = append(out, jsutil.NumberString(value))
	}
	return out
}

var (
	lastToken        = regexp.MustCompile(`(?i)^l$`)
	lastOffsetLoose  = regexp.MustCompile(`(?i)^l-\d{1,2}$`)
	lastWeekdayLoose = regexp.MustCompile(`(?i)^[0-7]l$`)
	digitsOnly       = regexp.MustCompile(`^\d+$`)
)

// normalizeToken is one value of node-cron's normalizeIntegers.
func normalizeToken(raw string) token {
	t := jsutil.Trim(raw)
	switch {
	case lastToken.MatchString(t):
		return token{str: "L"}
	case lastOffsetLoose.MatchString(t), lastWeekdayLoose.MatchString(t), strings.ContainsAny(t, "wW"):
		return token{str: strings.ToUpper(t)}
	case strings.Contains(t, "#"):
		return token{str: t}
	case digitsOnly.MatchString(t):
		n, _ := jsutil.ParseInt(t)
		return token{isNum: true, num: n}
	}
	return token{str: t}
}

// convert is node-cron's convertExpression: six fields of tokens. The
// caller has checked that the expression has five or six fields.
func convert(expression string) [6][]token {
	fields := splitFields(resolveNickname(expression))
	if len(fields) == 5 {
		fields = append([]string{"0"}, fields...)
	}
	if fields[3] == "?" {
		fields[3] = "*"
	}
	if fields[5] == "?" {
		fields[5] = "*"
	}
	fields[4] = replaceNames(replaceNames(fields[4], monthNames, 1), shortMonthNames, 1)
	fields[5] = replaceNames(replaceNames(fields[5], weekdayNames, 0), shortDayNames, 0)

	var out [6][]token
	for i, field := range fields {
		var values []string
		for _, part := range strings.Split(field, ",") {
			if strings.Contains(part, "*") {
				part = strings.Replace(part, "*", asteriskRanges[i], 1)
			}
			if m := rangeRe.FindStringSubmatch(jsutil.Trim(part)); m != nil {
				stepTxt := m[3]
				if stepTxt == "" {
					stepTxt = "1"
				}
				values = append(values, expandRange(m[1], m[2], stepTxt, i)...)
				continue
			}
			values = append(values, part)
		}
		for _, v := range values {
			out[i] = append(out[i], normalizeToken(v))
		}
	}

	// Weekday 7 is Sunday. node-cron then drops repeats with a Set, which
	// keeps the first of each.
	var weekdays []token
	seen := map[string]bool{}
	for _, t := range out[5] {
		if t.isNum && t.num == 7 {
			t.num = 0
		} else if !t.isNum && strings.HasPrefix(t.str, "7") {
			t.str = "0" + t.str[1:]
		}
		key := "s" + t.str
		if t.isNum {
			key = "n" + jsutil.NumberString(t.num)
		}
		if !seen[key] {
			seen[key] = true
			weekdays = append(weekdays, t)
		}
	}
	out[5] = weekdays
	return out
}

var allowedChars = regexp.MustCompile(`^[a-zA-Z0-9\-*/,#? ]+$`)

// validValues is node-cron's isValidExpression: every value must be a number
// (from digits) within [min, max]. A string value never passes its
// `^(?:\d+|\*|\*\/\d+)$` test, because the converter has already turned
// every `*` it accepts into a range.
func validValues(values []token, min, max float64) bool {
	for _, v := range values {
		if !v.isNum || v.num < min || v.num > max {
			return false
		}
	}
	return true
}

var (
	domWeekdayToken = regexp.MustCompile(`(?i)^(\d{1,2}|L)W$`)
	domOffsetToken  = regexp.MustCompile(`(?i)^L-(\d{1,2})$`)
	nthWeekdayToken = regexp.MustCompile(`^([0-7])#([1-5])$`)
	lastWeekdayStr  = regexp.MustCompile(`^[0-7]L$`)
)

func invalidDayOfMonth(values []token) bool {
	var plain []token
	for _, v := range values {
		if !v.isNum {
			if v.str == "L" {
				continue
			}
			if m := domWeekdayToken.FindStringSubmatch(v.str); m != nil {
				if m[1] == "L" {
					continue
				}
				target, _ := jsutil.ParseInt(m[1])
				if target >= 1 && target <= 31 {
					continue
				}
			} else if m := domOffsetToken.FindStringSubmatch(v.str); m != nil {
				n, _ := jsutil.ParseInt(m[1])
				if n >= 1 && n <= 30 {
					continue
				}
			}
		}
		plain = append(plain, v)
	}
	return !validValues(plain, 1, 31)
}

// invalidWModifier checks the raw day-of-month field: a value with a W must
// be "<day>W" or "LW".
func invalidWModifier(raw string) bool {
	if !strings.ContainsAny(raw, "wW") {
		return false
	}
	for _, part := range strings.Split(raw, ",") {
		value := jsutil.Trim(part)
		if strings.ContainsAny(value, "wW") && !domWeekdayToken.MatchString(value) {
			return true
		}
	}
	return false
}

func invalidWeekday(values []token) bool {
	var plain []token
	for _, v := range values {
		if !v.isNum && (nthWeekdayToken.MatchString(v.str) || lastWeekdayStr.MatchString(v.str)) {
			continue
		}
		plain = append(plain, v)
	}
	return !validValues(plain, 0, 7)
}

var maxDaysInMonth = map[float64]float64{1: 31, 2: 29, 3: 31, 4: 30, 5: 31, 6: 30, 7: 31, 8: 31, 9: 30, 10: 31, 11: 30, 12: 31}

func impossibleDayOfMonth(days, months []token) bool {
	for _, d := range days {
		if !d.isNum {
			return false
		}
	}
	for _, m := range months {
		for _, d := range days {
			if d.num <= maxDaysInMonth[m.num] {
				return false
			}
		}
	}
	return true
}

var fieldLabels = [6]string{"second", "minute", "hour", "day of month", "month", "week day"}

// check is node-cron's validateDetailed reduced to its first error, which
// is the message schedule() and parse() throw. It returns the converted
// fields of a valid expression.
func check(pattern string) ([6][]token, error) {
	resolved := resolveNickname(pattern)
	if !allowedChars.MatchString(resolved) {
		return [6][]token{}, fmt.Errorf("pattern includes illegal characters")
	}
	raw := splitFields(resolved)
	if len(raw) != 5 && len(raw) != 6 {
		return [6][]token{}, fmt.Errorf("expected 5 or 6 fields but got %d", len(raw))
	}
	patterns := raw
	if len(raw) == 5 {
		patterns = append([]string{"0"}, raw...)
	}
	fields := convert(pattern)
	invalid := [6]bool{
		!validValues(fields[0], 0, 59),
		!validValues(fields[1], 0, 59),
		!validValues(fields[2], 0, 23),
		invalidDayOfMonth(fields[3]) || invalidWModifier(patterns[3]),
		!validValues(fields[4], 1, 12),
		invalidWeekday(fields[5]),
	}
	for i, bad := range invalid {
		if bad {
			return [6][]token{}, fmt.Errorf("%s is a invalid expression for %s", patterns[i], fieldLabels[i])
		}
	}
	if impossibleDayOfMonth(fields[3], fields[4]) {
		return [6][]token{}, fmt.Errorf("%s %s is an impossible day of month for the given month", patterns[3], patterns[4])
	}
	return fields, nil
}

// Validate is node-cron's validate(): whether schedule() would accept the
// expression.
func Validate(pattern string) bool {
	_, err := check(pattern)
	return err == nil
}
