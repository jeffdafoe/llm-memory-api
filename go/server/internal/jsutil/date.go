package jsutil

import (
	"math"
	"time"
)

// maxTime is the largest absolute time value a JavaScript Date holds, in
// milliseconds from the epoch (TimeClip).
const maxTime = 8.64e15

const msPerDay = 86400000

// ParseDate is Date.parse(s) for strings in the ECMAScript date time string
// format, as V8 reads them: YYYY, YYYY-MM or YYYY-MM-DD (or a sign and six
// year digits; -000000 is refused), optionally followed by THH:mm,
// THH:mm:ss or THH:mm:ss.f, then Z or an offset ±HH:mm. A fraction has one
// or more digits and is cut to milliseconds. The month is 01 to 12 and the
// day 01 to 31; a day past the month's end rolls into the next month, as V8
// does. Hour 24 is midnight at the end of the day and allows no minutes,
// seconds or fraction. A date alone is UTC; a date and time with no offset is
// local time in loc. ok is false for anything else, and for a result outside
// the Date range.
//
// V8 also reads other forms with its legacy parser ("2026-11-06 10:00",
// lowercase t and z, "-0500" offsets, "10/09/2026", "Oct 9 2026"); those are
// not ported and give ok false.
func ParseDate(s string, loc *time.Location) (ms float64, ok bool) {
	p := dateParser{s: s}
	year, ok := p.year()
	if !ok {
		return 0, false
	}
	month, day := 1, 1
	if p.accept('-') {
		if month, ok = p.digits(2); !ok || month < 1 || month > 12 {
			return 0, false
		}
		if p.accept('-') {
			if day, ok = p.digits(2); !ok || day < 1 || day > 31 {
				return 0, false
			}
		}
	}
	days := daysFromCivil(year, month) + float64(day-1)
	if p.done() {
		return timeClip(days * msPerDay)
	}

	if !p.accept('T') {
		return 0, false
	}
	hour, ok := p.digits(2)
	if !ok || !p.accept(':') {
		return 0, false
	}
	minute, ok := p.digits(2)
	if !ok {
		return 0, false
	}
	second, millis := 0, 0
	if p.accept(':') {
		if second, ok = p.digits(2); !ok {
			return 0, false
		}
		if p.accept('.') {
			if millis, ok = p.fraction(); !ok {
				return 0, false
			}
		}
	}
	if hour > 24 || minute > 59 || second > 59 {
		return 0, false
	}
	if hour == 24 && (minute != 0 || second != 0 || millis != 0) {
		return 0, false
	}
	wall := days*msPerDay + float64(((hour*60+minute)*60+second)*1000+millis)

	if p.done() {
		return timeClip(localToUTC(wall, loc))
	}
	if p.accept('Z') {
		if !p.done() {
			return 0, false
		}
		return timeClip(wall)
	}
	sign := 1.0
	switch {
	case p.accept('+'):
	case p.accept('-'):
		sign = -1
	default:
		return 0, false
	}
	offsetHour, ok := p.digits(2)
	if !ok || !p.accept(':') {
		return 0, false
	}
	offsetMinute, ok := p.digits(2)
	if !ok || !p.done() || offsetHour > 23 || offsetMinute > 59 {
		return 0, false
	}
	return timeClip(wall - sign*float64((offsetHour*60+offsetMinute)*60000))
}

type dateParser struct {
	s string
	i int
}

func (p *dateParser) done() bool {
	return p.i == len(p.s)
}

func (p *dateParser) accept(c byte) bool {
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

// digits reads exactly n ASCII digits.
func (p *dateParser) digits(n int) (int, bool) {
	if p.i+n > len(p.s) {
		return 0, false
	}
	v := 0
	for _, c := range []byte(p.s[p.i : p.i+n]) {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int(c-'0')
	}
	p.i += n
	return v, true
}

func (p *dateParser) year() (int, bool) {
	sign := 0
	if p.accept('+') {
		sign = 1
	} else if p.accept('-') {
		sign = -1
	}
	if sign == 0 {
		return p.digits(4)
	}
	y, ok := p.digits(6)
	if !ok || (sign < 0 && y == 0) {
		return 0, false
	}
	return sign * y, true
}

// fraction reads one or more digits and returns the first three as
// milliseconds.
func (p *dateParser) fraction() (int, bool) {
	start := p.i
	ms, scale := 0, 100
	for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		ms += int(p.s[p.i]-'0') * scale
		scale /= 10
		p.i++
	}
	return ms, p.i > start
}

// daysFromCivil is the day number (days from 1970-01-01) of the first day of
// a month in the proleptic Gregorian calendar.
func daysFromCivil(year, month int) float64 {
	y := year
	if month <= 2 {
		y--
	}
	era := y / 400
	if y < 0 && y%400 != 0 {
		era--
	}
	yoe := y - era*400
	mp := (month + 9) % 12
	doy := (153*mp + 2) / 5
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return float64(era*146097 + doe - 719468)
}

// timeClip is TimeClip: NaN for a value outside the Date range, else the
// value cut to an integer.
func timeClip(ms float64) (float64, bool) {
	if math.IsNaN(ms) || math.Abs(ms) > maxTime {
		return 0, false
	}
	return math.Trunc(ms) + 0, true
}

// offsetAt is the zone offset in milliseconds at an instant.
func offsetAt(ms float64, loc *time.Location) float64 {
	_, off := time.UnixMilli(int64(ms)).In(loc).Zone()
	return float64(off) * 1000
}

// localToUTC turns a wall-clock time in loc (as milliseconds, read as if it
// were UTC) into an instant, the way V8 does through ICU with the "former"
// rule: a time that occurs twice (the hour repeated when clocks go back)
// takes the earlier offset, and a time that does not occur (skipped when
// clocks go forward) takes the offset from before the change, so it lands
// past the change. Go's time.Date picks differently for skipped times.
func localToUTC(wall float64, loc *time.Location) float64 {
	if math.Abs(wall) > maxTime+msPerDay {
		return wall
	}
	before := offsetAt(wall-msPerDay, loc)
	instant := wall - before
	if offsetAt(instant, loc) == before {
		return instant
	}
	after := offsetAt(instant, loc)
	if offsetAt(wall-after, loc) == after {
		return wall - after
	}
	return instant
}

// DateValue is new Date(v).getTime() for a JavaScript value from Parse.
// ok is false where it gives NaN. A value that cannot become a primitive is
// V8's TypeError. A string goes through ParseDate, so a free-form date V8
// reads with its legacy parser gives ok false here.
func DateValue(v any, loc *time.Location) (ms float64, ok bool, err error) {
	if !primitiveOK(v) {
		return 0, false, ErrNoPrimitive
	}
	switch x := v.(type) {
	case string:
		ms, ok = ParseDate(x, loc)
		return ms, ok, nil
	case []any:
		ms, ok = ParseDate(valueJoin(x), loc)
		return ms, ok, nil
	case *Object:
		// "[object Object]" is not a date.
		return 0, false, nil
	}
	n, err := ValueNumber(v)
	if err != nil {
		return 0, false, err
	}
	ms, ok = timeClip(n)
	return ms, ok, nil
}
