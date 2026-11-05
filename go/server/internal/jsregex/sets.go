package jsregex

import (
	"math/bits"
	"sync"
	"unicode"
)

// unitSet is a set of UTF-16 code units.
type unitSet [1024]uint64

func (s *unitSet) add(u uint16)      { s[u>>6] |= 1 << (u & 63) }
func (s *unitSet) has(u uint16) bool { return s[u>>6]&(1<<(u&63)) != 0 }

func (s *unitSet) addRange(lo, hi uint16) {
	for u := int(lo); u <= int(hi); u++ {
		s.add(uint16(u))
	}
}

func (s *unitSet) union(o *unitSet) {
	for i := range s {
		s[i] |= o[i]
	}
}

func (s *unitSet) complement() *unitSet {
	out := &unitSet{}
	for i := range s {
		out[i] = ^s[i]
	}
	return out
}

func (s *unitSet) count() int {
	n := 0
	for _, w := range s {
		n += bits.OnesCount64(w)
	}
	return n
}

func digitSet() *unitSet {
	s := &unitSet{}
	s.addRange('0', '9')
	return s
}

func wordSet() *unitSet {
	s := &unitSet{}
	s.addRange('a', 'z')
	s.addRange('A', 'Z')
	s.addRange('0', '9')
	s.add('_')
	return s
}

// spaceSet is JavaScript's \s: WhiteSpace and LineTerminator.
func spaceSet() *unitSet {
	s := &unitSet{}
	for _, u := range []uint16{0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x20, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff} {
		s.add(u)
	}
	s.addRange(0x2000, 0x200a)
	return s
}

// lineTerminators is what "." does not match without the s flag.
func lineTerminators() *unitSet {
	s := &unitSet{}
	for _, u := range []uint16{0x0a, 0x0d, 0x2028, 0x2029} {
		s.add(u)
	}
	return s
}

// multiUnitUpper lists the code units whose full uppercase mapping
// (SpecialCasing.txt, unconditional) is more than one character. For them
// toUpperCase gives a longer string, and Canonicalize keeps the unit as it
// is; Go's simple mapping would give a single character instead.
var multiUnitUpper = [][2]uint16{
	{0x00DF, 0x00DF}, {0x0149, 0x0149}, {0x01F0, 0x01F0}, {0x0390, 0x0390}, {0x03B0, 0x03B0},
	{0x0587, 0x0587}, {0x1E96, 0x1E9A}, {0x1F50, 0x1F50}, {0x1F52, 0x1F52}, {0x1F54, 0x1F54},
	{0x1F56, 0x1F56}, {0x1F80, 0x1FAF}, {0x1FB2, 0x1FB4}, {0x1FB6, 0x1FB7}, {0x1FBC, 0x1FBC},
	{0x1FC2, 0x1FC4}, {0x1FC6, 0x1FC7}, {0x1FCC, 0x1FCC}, {0x1FD2, 0x1FD3}, {0x1FD6, 0x1FD7},
	{0x1FE2, 0x1FE4}, {0x1FE6, 0x1FE7}, {0x1FF2, 0x1FF4}, {0x1FF6, 0x1FF7}, {0x1FFC, 0x1FFC},
	{0xFB00, 0xFB06}, {0xFB13, 0xFB17},
}

var (
	foldOnce sync.Once
	canon    [65536]uint16
)

// canonicalize is the ECMAScript Canonicalize for the i flag without u: the
// unit's uppercase when that is one code unit, except that a non-ASCII unit
// never maps into ASCII ("ſ" stays "ſ", not "S").
func buildCanon() {
	multi := map[uint16]bool{}
	for _, r := range multiUnitUpper {
		for u := int(r[0]); u <= int(r[1]); u++ {
			multi[uint16(u)] = true
		}
	}
	for u := 0; u < 65536; u++ {
		cu := uint16(u)
		canon[u] = cu
		if u >= 0xD800 && u <= 0xDFFF || multi[cu] {
			continue
		}
		up := unicode.ToUpper(rune(u))
		if up > 0xFFFF || u >= 128 && up < 128 {
			continue
		}
		canon[u] = uint16(up)
	}
}

// fold is every code unit that matches a member of s under the i flag: the
// units whose canonical form is the canonical form of some member.
func (s *unitSet) fold() *unitSet {
	foldOnce.Do(buildCanon)
	targets := &unitSet{}
	for u := 0; u < 65536; u++ {
		if s.has(uint16(u)) {
			targets.add(canon[u])
		}
	}
	out := &unitSet{}
	for u := 0; u < 65536; u++ {
		if targets.has(canon[u]) {
			out.add(uint16(u))
		}
	}
	return out
}
