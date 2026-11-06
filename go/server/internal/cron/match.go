package cron

import (
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Schedule is a parsed expression: node-cron's TimeMatcher.
type Schedule struct {
	loc                              *time.Location
	seconds, minutes, hours          []int
	months                           []int
	days, weekdays                   []token
	sortedSec, sortedMin, sortedHour []int
}

// Parse reads an expression in the process's local time zone, which is what
// node-cron used when no timezone option was given. The error is the message
// node-cron's schedule() threw.
func Parse(pattern string) (*Schedule, error) {
	return ParseIn(pattern, time.Local)
}

// ParseIn reads an expression in the given time zone.
func ParseIn(pattern string, loc *time.Location) (*Schedule, error) {
	fields, err := check(pattern)
	if err != nil {
		return nil, err
	}
	// A valid expression's second, minute, hour and month fields hold only
	// numbers within range.
	ints := func(values []token) []int {
		out := make([]int, len(values))
		for i, v := range values {
			out[i] = int(v.num)
		}
		return out
	}
	s := &Schedule{
		loc:      loc,
		seconds:  ints(fields[0]),
		minutes:  ints(fields[1]),
		hours:    ints(fields[2]),
		days:     fields[3],
		months:   ints(fields[4]),
		weekdays: fields[5],
	}
	s.sortedSec = sorted(s.seconds)
	s.sortedMin = sorted(s.minutes)
	s.sortedHour = sorted(s.hours)
	return s, nil
}

func sorted(values []int) []int {
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}

type dateParts struct {
	year, month, day, hour, minute, second int
	weekday                                int
}

func (s *Schedule) parts(ms int64) dateParts {
	t := time.UnixMilli(ms).In(s.loc)
	return dateParts{
		year: t.Year(), month: int(t.Month()), day: t.Day(),
		hour: t.Hour(), minute: t.Minute(), second: t.Second(),
		weekday: int(t.Weekday()),
	}
}

// Match is node-cron's TimeMatcher.match: every field matches the instant's
// local time. Day of month and day of week must both match.
func (s *Schedule) Match(t time.Time) bool {
	p := s.parts(t.UnixMilli())
	return slices.Contains(s.seconds, p.second) &&
		slices.Contains(s.minutes, p.minute) &&
		slices.Contains(s.hours, p.hour) &&
		matchesDayOfMonth(s.days, p.year, p.month, p.day) &&
		slices.Contains(s.months, p.month) &&
		matchesDayOfWeek(s.weekdays, p.year, p.month, p.day, p.weekday)
}

func lastDayOfMonth(year, month int) int {
	return time.Date(year, time.Month(month+1), 0, 0, 0, 0, 0, time.UTC).Day()
}

func weekdayOf(year, month, day int) int {
	return int(time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC).Weekday())
}

// nearestWeekday is the "15W" rule: the weekday nearest the target day
// without leaving the month; -1 when the month has no such day.
func nearestWeekday(year, month, target int) int {
	last := lastDayOfMonth(year, month)
	if target < 1 || target > last {
		return -1
	}
	switch weekdayOf(year, month, target) {
	case 6:
		if target == 1 {
			return target + 2
		}
		return target - 1
	case 0:
		if target == last {
			return target - 2
		}
		return target + 1
	}
	return target
}

var (
	weekdayDayToken = regexp.MustCompile(`^(\d{1,2}|L)W$`)
	lastOffsetToken = regexp.MustCompile(`^L-(\d{1,2})$`)
	lastWeekdayAny  = regexp.MustCompile(`(?i)^([0-7])L$`)
)

func matchesDayOfMonth(field []token, year, month, day int) bool {
	for _, v := range field {
		if v.isNum {
			if v.num == float64(day) {
				return true
			}
			continue
		}
		if v.str == "L" && day == lastDayOfMonth(year, month) {
			return true
		}
		if m := weekdayDayToken.FindStringSubmatch(v.str); m != nil {
			target := lastDayOfMonth(year, month)
			if m[1] != "L" {
				n, _ := jsutil.ParseInt(m[1])
				target = int(n)
			}
			if nearestWeekday(year, month, target) == day {
				return true
			}
		}
		if m := lastOffsetToken.FindStringSubmatch(v.str); m != nil {
			n, _ := jsutil.ParseInt(m[1])
			target := lastDayOfMonth(year, month) - int(n)
			if target >= 1 && target == day {
				return true
			}
		}
	}
	return false
}

func matchesDayOfWeek(field []token, year, month, day, weekday int) bool {
	for _, v := range field {
		if v.isNum {
			if v.num == float64(weekday) {
				return true
			}
			continue
		}
		if m := nthWeekdayToken.FindStringSubmatch(v.str); m != nil {
			wd := int(m[1][0]-'0') % 7
			nth := int(m[2][0] - '0')
			if weekdayOf(year, month, day) == wd && (day-1)/7+1 == nth {
				return true
			}
			continue
		}
		if m := lastWeekdayAny.FindStringSubmatch(v.str); m != nil {
			wd := int(m[1][0]-'0') % 7
			// The last such weekday: a week later is in another month.
			if wd == weekday && int(time.Date(year, time.Month(month), day+7, 0, 0, 0, 0, time.UTC).Month()) != month {
				return true
			}
		}
	}
	return false
}

// maxDays is how far ahead node-cron's walk looks for a matching day.
const maxDays = 366 * 100

// ErrNoMatch is node-cron's error for an expression that matches no time in
// the next hundred years.
var ErrNoMatch = errors.New("Could not find next matching date within reasonable time range")

// Next is node-cron's getNextMatch: the first matching time strictly after
// t (taken to the whole second below it).
func (s *Schedule) Next(t time.Time) (time.Time, error) {
	baseMs := floorDiv(t.UnixMilli(), 1000) * 1000
	base := s.parts(baseMs)
	year, month, day := base.year, base.month, base.day
	for i := range maxDays {
		if slices.Contains(s.months, month) && matchesDayOfMonth(s.days, year, month, day) &&
			matchesDayOfWeek(s.weekdays, year, month, day, weekdayOf(year, month, day)) {
			var lower *dateParts
			if i == 0 {
				lower = &base
			}
			if ts, ok := s.firstTimeOnDay(year, month, day, lower, baseMs); ok {
				return time.UnixMilli(ts), nil
			}
		}
		next := time.Date(year, time.Month(month), day+1, 0, 0, 0, 0, time.UTC)
		year, month, day = next.Year(), int(next.Month()), next.Day()
	}
	return time.Time{}, ErrNoMatch
}

func (s *Schedule) firstTimeOnDay(year, month, day int, lower *dateParts, baseMs int64) (int64, bool) {
	for _, hour := range s.sortedHour {
		if lower != nil && hour < lower.hour {
			continue
		}
		for _, minute := range s.sortedMin {
			for _, second := range s.sortedSec {
				if lower != nil && hour*3600+minute*60+second <= lower.hour*3600+lower.minute*60+lower.second {
					continue
				}
				ts := s.localToTimestamp(dateParts{year: year, month: month, day: day, hour: hour, minute: minute, second: second})
				if ts > baseMs && s.Match(time.UnixMilli(ts)) {
					return ts, true
				}
			}
		}
	}
	return 0, false
}

// offsetMinutes is the zone's offset at ms as node-cron read it from Intl's
// "shortOffset" name. A name with seconds (a local mean time before
// standard zones) does not match its pattern, which it then read as 0.
func (s *Schedule) offsetMinutes(ms int64) int64 {
	_, secs := time.UnixMilli(ms).In(s.loc).Zone()
	if secs%60 != 0 {
		return 0
	}
	return int64(secs / 60)
}

func (s *Schedule) readsBackTo(ms int64, p dateParts) bool {
	q := s.parts(ms)
	return q.year == p.year && q.month == p.month && q.day == p.day &&
		q.hour == p.hour && q.minute == p.minute && q.second == p.second
}

// localToTimestamp is node-cron's localTimeToTimestamp: the instant whose
// local time is p. In a gap (spring forward) it lands on a later local time,
// which the caller's Match then refuses; in an overlap it takes the earlier
// instant that reads back as p.
func (s *Schedule) localToTimestamp(p dateParts) int64 {
	guess := time.Date(p.year, time.Month(p.month), p.day, p.hour, p.minute, p.second, 0, time.UTC).UnixMilli()
	first := s.offsetMinutes(guess)
	candidate1 := guess - first*60000
	second := s.offsetMinutes(candidate1)
	if second == first {
		return candidate1
	}
	candidate2 := guess - second*60000
	if s.readsBackTo(candidate1, p) {
		return candidate1
	}
	if s.readsBackTo(candidate2, p) {
		return candidate2
	}
	return max(candidate1, candidate2)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}
