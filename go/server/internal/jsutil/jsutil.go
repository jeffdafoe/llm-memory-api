// Package jsutil copies the JavaScript string, number and date rules the Node
// server's code relied on, so ported code gives the same results for the
// same input: what counts as whitespace, how a string lowercases, how
// Number(), parseInt() and Math.round() read a value, and how node-postgres
// turns a timestamptz into a JSON date.
package jsutil

import (
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
)

// IsSpace reports whether r is JavaScript whitespace: what \s matches and
// String.prototype.trim removes (WhiteSpace and LineTerminator). It differs
// from unicode.IsSpace: U+FEFF is whitespace here, U+0085 is not.
func IsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029',
		'\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return r >= '\u2000' && r <= '\u200a'
}

// Trim is String.prototype.trim.
func Trim(s string) string {
	return strings.TrimFunc(s, IsSpace)
}

// TrimStart is String.prototype.trimStart.
func TrimStart(s string) string {
	return strings.TrimLeftFunc(s, IsSpace)
}

// SplitSpace is s.split(/\s+/): leading or trailing whitespace gives an
// empty first or last element, and "" gives [""].
func SplitSpace(s string) []string {
	var out []string
	start := 0
	inSpace := false
	for i, r := range s {
		if IsSpace(r) {
			if !inSpace {
				out = append(out, s[start:i])
				inSpace = true
			}
			continue
		}
		if inSpace {
			start = i
			inSpace = false
		}
	}
	if inSpace {
		return append(out, "")
	}
	return append(out, s[start:])
}

// UTF16Len is a JavaScript string's length: UTF-16 code units.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// TruncateUTF16 cuts s to at most max UTF-16 code units, the way
// s.slice(0, max) does followed by dropping a trailing lone high surrogate:
// a character that would be split is left out whole.
func TruncateUTF16(s string, max int) string {
	n := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if n+w > max {
			return s[:i]
		}
		n += w
	}
	return s
}

// ToLower is String.prototype.toLowerCase. Go's simple case mapping misses
// the two special-casing rules JavaScript applies outside any locale: U+0130
// becomes "i" plus U+0307, and a capital sigma at the end of a word becomes
// the final form ς.
func ToLower(s string) string {
	if isASCII(s) {
		return strings.ToLower(s)
	}
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		switch {
		case r == '\u0130':
			b.WriteString("i\u0307")
		case r == '\u03a3' && finalSigma(runes, i):
			b.WriteRune('\u03c2')
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// finalSigma is the Unicode Final_Sigma condition: a cased letter before the
// sigma (skipping case-ignorable characters) and none after it.
func finalSigma(runes []rune, i int) bool {
	before := false
	for j := i - 1; j >= 0; j-- {
		if caseIgnorable(runes[j]) {
			continue
		}
		before = cased(runes[j])
		break
	}
	if !before {
		return false
	}
	for j := i + 1; j < len(runes); j++ {
		if caseIgnorable(runes[j]) {
			continue
		}
		return !cased(runes[j])
	}
	return true
}

// cased is the Unicode Cased property: the upper, lower and title case
// letters plus Other_Lowercase and Other_Uppercase (U+00AA, for example).
func cased(r rune) bool {
	return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) ||
		unicode.Is(unicode.Other_Lowercase, r) || unicode.Is(unicode.Other_Uppercase, r)
}

func caseIgnorable(r rune) bool {
	switch r {
	case '\'', '.', ':', '\u00b7', '\u2018', '\u2019', '\u2024', '\u2027':
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}

// HasPrefixFold reports whether s starts with the ASCII prefix, ignoring
// ASCII case only, as a JavaScript /i regex without the u flag does for an
// ASCII pattern: "ſ" does not match "s" there, though Go's (?i) would.
func HasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		a, b := s[i], prefix[i]
		if 'A' <= a && a <= 'Z' {
			a += 'a' - 'A'
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

// ParseInt is parseInt(s) with no radix: leading whitespace, an optional
// sign, "0x" for hexadecimal, then the longest run of digits. ok is false
// where JavaScript gives NaN.
func ParseInt(s string) (n float64, ok bool) {
	s = TrimStart(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	base := 10
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		base = 16
		s = s[2:]
	}
	end := 0
	for end < len(s) {
		d := digitValue(s[end])
		if d < 0 || d >= base {
			break
		}
		end++
	}
	if end == 0 {
		return math.NaN(), false
	}
	n = integerValue(s[:end], base)
	if neg {
		n = -n
	}
	return n, true
}

// integerValue converts a run of digits in base to the nearest float64,
// rounding once at the end as JavaScript does, not after every digit.
func integerValue(digits string, base int) float64 {
	b, ok := new(big.Int).SetString(digits, base)
	if !ok {
		return math.NaN()
	}
	f, _ := new(big.Float).SetInt(b).Float64()
	return f
}

func digitValue(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'z':
		return int(c-'a') + 10
	case 'A' <= c && c <= 'Z':
		return int(c-'A') + 10
	}
	return -1
}

// IntOr is `parseInt(s) || fallback` as an int: NaN and 0 give fallback, and
// a value past the int range saturates.
func IntOr(s string, fallback int) int {
	n, ok := ParseInt(s)
	if !ok || n == 0 {
		return fallback
	}
	return saturate(n)
}

func saturate(n float64) int {
	if n >= math.MaxInt64 {
		return math.MaxInt64
	}
	if n <= math.MinInt64 {
		return math.MinInt64
	}
	return int(n)
}

// Round is Math.round: halves go toward +Infinity.
func Round(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	r := math.Floor(x)
	if x-r >= 0.5 {
		r++
	}
	return r
}

// FloorOr is `Math.floor(x) || fallback`: NaN and 0 give fallback.
func FloorOr(x, fallback float64) float64 {
	f := math.Floor(x)
	if math.IsNaN(f) || f == 0 {
		return fallback
	}
	return f
}

// ToNumber is Number(v) for a value decoded from a JSON body. A missing
// value (nil raw) is undefined and gives NaN; JSON null gives 0.
func ToNumber(raw json.RawMessage) float64 {
	if raw == nil {
		return math.NaN()
	}
	v, err := decode(raw)
	if err != nil {
		return math.NaN()
	}
	return toNumber(v)
}

// decode keeps numbers as their JSON text, as JSON.parse reads them, so a
// number too large for a float64 becomes Infinity instead of an error.
func decode(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

func toNumber(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case json.Number:
		return jsonNumber(string(x))
	case string:
		return StringToNumber(x)
	case []any:
		return StringToNumber(arrayString(x))
	}
	// An object's string form is "[object Object]".
	return math.NaN()
}

// jsonNumber reads JSON number text as JSON.parse does: out of range is
// ±Infinity (or ±0), not an error.
func jsonNumber(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f
		}
		return math.NaN()
	}
	return f
}

// arrayString is String(array): elements joined by ",", null and undefined
// as "", nested arrays the same way.
func arrayString(a []any) string {
	parts := make([]string, len(a))
	for i, e := range a {
		switch x := e.(type) {
		case nil:
			parts[i] = ""
		case bool:
			parts[i] = strconv.FormatBool(x)
		case json.Number:
			parts[i] = NumberString(jsonNumber(string(x)))
		case string:
			parts[i] = x
		case []any:
			parts[i] = arrayString(x)
		default:
			parts[i] = "[object Object]"
		}
	}
	return strings.Join(parts, ",")
}

// StringToNumber is Number(s) for a string: whitespace is trimmed, "" is 0,
// "Infinity" with an optional sign, unsigned 0x / 0o / 0b integers, and
// decimal literals. Anything else is NaN.
func StringToNumber(s string) float64 {
	s = Trim(s)
	switch s {
	case "":
		return 0
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			for i := 2; i < len(s); i++ {
				d := digitValue(s[i])
				if d < 0 || d >= base {
					return math.NaN()
				}
			}
			return integerValue(s[2:], base)
		}
	}
	if !isDecimalLiteral(s) {
		return math.NaN()
	}
	return jsonNumber(s)
}

// isDecimalLiteral is StrDecimalLiteral without Infinity: an optional sign,
// digits with an optional point (at least one digit on some side), and an
// optional exponent.
func isDecimalLiteral(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		exp := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			exp++
		}
		if exp == 0 {
			return false
		}
	}
	return i == len(s)
}

// NumberString is String(x) for a finite number. encoding/json formats
// float64 values the way ES6 Number.prototype.toString does.
func NumberString(x float64) string {
	switch {
	case math.IsNaN(x):
		return "NaN"
	case math.IsInf(x, 1):
		return "Infinity"
	case math.IsInf(x, -1):
		return "-Infinity"
	case x == 0:
		// String(-0) is "0"; encoding/json writes "-0".
		return "0"
	}
	b, _ := json.Marshal(x)
	return string(b)
}

// Number is a float64 sent to Postgres as String(x), the text node-postgres
// sends for a JavaScript number. Sending a float64 instead would cast
// through float8, which keeps only 15 significant digits when Postgres
// converts it to numeric.
type Number string

// Num is Number(NumberString(x)).
func Num(x float64) Number {
	return Number(NumberString(x))
}

// DateISO is how a timestamptz reaches a JSON response in the Node server:
// node-postgres builds a Date with 1000 * parseFloat(fraction) milliseconds,
// which the Date truncates, and JSON.stringify writes toISOString. The
// float64 steps are kept, so a fraction whose product falls just under a
// whole millisecond loses that millisecond as it did in Node.
func DateISO(t time.Time) string {
	t = t.UTC()
	us := t.Nanosecond() / 1000
	ms := int(math.Trunc(1000 * (float64(us) / 1e6)))
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), ms*int(time.Millisecond), time.UTC)
	y := t.Year()
	if y >= 0 && y <= 9999 {
		return t.Format("2006-01-02T15:04:05.000Z")
	}
	sign := "+"
	if y < 0 {
		sign = "-"
		y = -y
	}
	return sign + leftPad(strconv.Itoa(y), 6) + t.Format("-01-02T15:04:05.000Z")
}

func leftPad(s string, n int) string {
	for len(s) < n {
		s = "0" + s
	}
	return s
}

// ToFixed is Number.prototype.toFixed for |x| below 1e21: the exact
// decimal value of x rounded to digits places, a tie going to the larger
// magnitude (toFixed(0.25, 1) is "0.3"; Go's 'f' format gives "0.2").
func ToFixed(x float64, digits int) string {
	if math.IsNaN(x) {
		return "NaN"
	}
	if math.Abs(x) >= 1e21 || math.IsInf(x, 0) {
		return NumberString(x)
	}
	neg := x < 0
	if neg {
		x = -x
	}
	exact := new(big.Float).SetPrec(2048).SetFloat64(x)
	scale := new(big.Float).SetPrec(2048).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil))
	exact.Mul(exact, scale)
	exact.Add(exact, big.NewFloat(0.5).SetPrec(2048))
	n, _ := exact.Int(nil) // truncates toward zero, so this is floor(x*10^d + 0.5)
	s := n.String()
	if digits > 0 {
		for len(s) <= digits {
			s = "0" + s
		}
		s = s[:len(s)-digits] + "." + s[len(s)-digits:]
	}
	// x < 0 above, so -0 has no sign, but a negative value that rounds to
	// zero keeps it: (-0.001).toFixed(2) is "-0.00".
	if neg {
		s = "-" + s
	}
	return s
}
