package jsutil

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

// The expected values in this file were produced with Node 24.

func TestDateISO(t *testing.T) {
	cases := map[int]string{ // microseconds → milliseconds Node shows
		0:      "000",
		100000: "100",
		123000: "123",
		123456: "123",
		1:      "000",
		999999: "999",
		35000:  "035",
		570000: "570",
		290000: "290",
		1000:   "001",
		500:    "000",
		7000:   "007",
	}
	for us, ms := range cases {
		at := time.Date(2026, 11, 5, 10, 20, 30, us*1000, time.UTC)
		if got, want := DateISO(at), "2026-11-05T10:20:30."+ms+"Z"; got != want {
			t.Errorf("DateISO(.%06d) = %s, want %s", us, got, want)
		}
	}
	east := time.Date(2026, 11, 5, 12, 0, 0, 0, time.FixedZone("x", 2*3600))
	if got := DateISO(east); got != "2026-11-05T10:00:00.000Z" {
		t.Errorf("DateISO in +02:00 = %s", got)
	}
}

func TestStringToNumber(t *testing.T) {
	nan := math.NaN()
	cases := map[string]float64{
		"5": 5, "  7 ": 7, "0x1A": 26, "0b101": 5, "0o17": 15, "-0x5": nan,
		"1e3": 1000, ".5": 0.5, "5.": 5, "+.5e1": 5, "": 0,
		"Infinity": math.Inf(1), "-Infinity": math.Inf(-1), "inf": nan,
		"1_000": nan, "12abc": nan, "0x1fffffffffffff1": 144115188075855860, "900719925474099267": 900719925474099300, "\u00a03\u2003": 3, ".": nan, "e5": nan,
	}
	for in, want := range cases {
		got := StringToNumber(in)
		if !(got == want || math.IsNaN(got) && math.IsNaN(want)) {
			t.Errorf("Number(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestToNumber(t *testing.T) {
	cases := map[string]string{
		`[7]`: "7", `[]`: "0", `[1,2]`: "NaN", `[[3]]`: "3", `[null]`: "0", `{}`: "NaN",
		`true`: "1", `null`: "0", `"0x10"`: "16", `1e400`: "Infinity", `-1e400`: "-Infinity",
		`["4"]`: "4", `[1.0]`: "1", `[-0]`: "0", `5.5`: "5.5",
	}
	for in, want := range cases {
		if got := NumberString(ToNumber(json.RawMessage(in))); got != want {
			t.Errorf("Number(%s) = %s, want %s", in, got, want)
		}
	}
	if !math.IsNaN(ToNumber(nil)) {
		t.Error("Number(undefined) is not NaN")
	}
}

func TestParseInt(t *testing.T) {
	cases := map[string]string{
		"5": "5", "  42abc": "42", "0x1F": "31", "-0x10": "-16", "+7": "7",
		"abc": "NaN", "": "NaN", "1e3": "1", "900719925474099267": "900719925474099300", "0x1fffffffffffff1": "144115188075855860", "\u00a09": "9", "-0": "0",
	}
	for in, want := range cases {
		n, ok := ParseInt(in)
		got := "NaN"
		if ok {
			got = NumberString(n)
		}
		if got != want {
			t.Errorf("parseInt(%q) = %s, want %s", in, got, want)
		}
	}
	if IntOr("0", 5) != 5 || IntOr("x", 5) != 5 || IntOr("3", 5) != 3 || IntOr("-2", 5) != -2 {
		t.Error("IntOr does not follow parseInt(s) || fallback")
	}
}

func TestToLower(t *testing.T) {
	cases := map[string][]rune{
		"\u039f\u0394\u039f\u03a3":   {0x3bf, 0x3b4, 0x3bf, 0x3c2},
		"\u039f\u03a3 \u039f\u03a3.": {0x3bf, 0x3c2, 0x20, 0x3bf, 0x3c2, 0x2e},
		"\u03a3":                     {0x3c3},
		"\u0391\u03a3\u0301":         {0x3b1, 0x3c2, 0x301},
		"\u0391\u03a3\u0391":         {0x3b1, 0x3c3, 0x3b1},
		"\u0130S":                    {'i', 0x307, 's'},
		"\u00aa\u03a3":               {0xaa, 0x3c2},
		"KELVIN \u212a":              []rune("kelvin k"),
		"Plain ASCII":                []rune("plain ascii"),
	}
	for in, want := range cases {
		if got := []rune(ToLower(in)); !reflect.DeepEqual(got, want) {
			t.Errorf("ToLower(%q) = %U, want %U", in, got, want)
		}
	}
}

func TestSplitSpace(t *testing.T) {
	cases := map[string][]string{
		"":                  {""},
		" a":                {"", "a"},
		"a  b ":             {"a", "b", ""},
		"\ta\u00a0b\u0085c": {"", "a", "b\u0085c"},
		"x":                 {"x"},
	}
	for in, want := range cases {
		if got := SplitSpace(in); !reflect.DeepEqual(got, want) {
			t.Errorf("SplitSpace(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrimIsJavaScriptTrim(t *testing.T) {
	if got := Trim("\ufeff\u00a0x\u3000"); got != "x" {
		t.Errorf("Trim kept JS whitespace: %q", got)
	}
	if got := Trim("\u0085x\u0085"); got != "\u0085x\u0085" {
		t.Errorf("Trim removed U+0085, which JS keeps: %q", got)
	}
}

func TestTruncateUTF16(t *testing.T) {
	if got := TruncateUTF16("ab\U0001F600c", 3); got != "ab" {
		t.Errorf("a cut inside a surrogate pair kept half: %q", got)
	}
	if got := TruncateUTF16("ab\U0001F600c", 4); got != "ab\U0001F600" {
		t.Errorf("TruncateUTF16 to 4 = %q", got)
	}
	if UTF16Len("a\U0001F600") != 3 {
		t.Error("UTF16Len does not count surrogate pairs as two")
	}
}

func TestRoundAndFloor(t *testing.T) {
	for in, want := range map[float64]float64{2.5: 3, -2.5: -2, 0.49999999999999994: 0, 1.4: 1} {
		if got := Round(in); got != want {
			t.Errorf("Math.round(%v) = %v, want %v", in, got, want)
		}
	}
	if FloorOr(0.15, 5) != 5 || FloorOr(math.NaN(), 5) != 5 || FloorOr(-0.5, 5) != -1 || FloorOr(7.9, 5) != 7 {
		t.Error("FloorOr does not follow Math.floor(x) || fallback")
	}
}

func TestNumberString(t *testing.T) {
	cases := map[float64]string{0.15: "0.15", 1e-7: "1e-7", 1e21: "1e+21", 1e20: "100000000000000000000",
		math.Copysign(0, -1): "0", 0.30000000000000004: "0.30000000000000004", 1209600: "1209600"}
	for in, want := range cases {
		if got := NumberString(in); got != want {
			t.Errorf("String(%v) = %s, want %s", in, got, want)
		}
	}
	if fmt.Sprint(Num(1.5)) != "1.5" {
		t.Error("Num")
	}
}

func TestHasPrefixFold(t *testing.T) {
	if !HasPrefixFold("CAN YOU x", "can you ") || HasPrefixFold("\u017fhow", "show") || HasPrefixFold("ca", "can") {
		t.Error("HasPrefixFold is not ASCII-only case folding")
	}
}
