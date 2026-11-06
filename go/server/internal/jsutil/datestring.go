package jsutil

import (
	"fmt"
	"time"
)

// zoneNames are ICU's long names for the zone abbreviations Go reports, as
// V8 prints them in parentheses at the end of Date.prototype.toString. The
// server runs in UTC; the US zones cover a developer's machine.
var zoneNames = map[string]string{
	"UTC":  "Coordinated Universal Time",
	"GMT":  "Greenwich Mean Time",
	"EST":  "Eastern Standard Time",
	"EDT":  "Eastern Daylight Time",
	"CST":  "Central Standard Time",
	"CDT":  "Central Daylight Time",
	"MST":  "Mountain Standard Time",
	"MDT":  "Mountain Daylight Time",
	"PST":  "Pacific Standard Time",
	"PDT":  "Pacific Daylight Time",
	"AKST": "Alaska Standard Time",
	"AKDT": "Alaska Daylight Time",
	"HST":  "Hawaii-Aleutian Standard Time",
}

// DateString is String(date) for a Date holding t: Date.prototype.toString
// in loc, "Fri Nov 06 2026 14:31:30 GMT+0000 (Coordinated Universal Time)".
// The time is cut to milliseconds first, as a Date holds no more. A zone
// whose abbreviation has no entry in zoneNames prints the abbreviation.
func DateString(t time.Time, loc *time.Location) string {
	t = t.Truncate(time.Millisecond).In(loc)
	abbrev, offset := t.Zone()
	name, ok := zoneNames[abbrev]
	if !ok {
		name = abbrev
	}
	sign := '+'
	if offset < 0 {
		sign = '-'
		offset = -offset
	}
	year := fmt.Sprintf("%04d", t.Year())
	if t.Year() < 0 {
		year = fmt.Sprintf("-%04d", -t.Year())
	}
	return fmt.Sprintf("%s %s %02d %s %02d:%02d:%02d GMT%c%02d%02d (%s)",
		t.Weekday().String()[:3], t.Month().String()[:3], t.Day(), year,
		t.Hour(), t.Minute(), t.Second(), sign, offset/3600, offset/60%60, name)
}
