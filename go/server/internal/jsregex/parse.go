// Package jsregex runs JavaScript regular expressions in Go with
// JavaScript's semantics, for the notes grep (Node's `new RegExp(pattern,
// 'i')`). Go's regexp is RE2, which refuses lookbehind and backreferences
// and differs from JavaScript in many details, so this package parses a
// pattern with V8's rules (non-unicode mode, Annex B, V8's error messages)
// and runs it with the ECMAScript specification's own matching algorithm: a
// backtracking matcher over UTF-16 code units, with the spec's capture reset
// at each repetition, its Canonicalize case folding for the i flag (in sets
// and backreferences alike), and right-to-left lookbehind. "." matches one
// half of a surrogate pair, as it does in JavaScript without the u flag.
package jsregex

import (
	"strconv"
	"unicode"
	"unicode/utf16"
)

// SyntaxError is a pattern V8 refuses. Error() is V8's message for
// `new RegExp(pattern, 'i')`.
type SyntaxError struct {
	Source string
	Reason string
}

func (e *SyntaxError) Error() string {
	return "Invalid regular expression: /" + e.Source + "/i: " + e.Reason
}

type kind int

const (
	kSet kind = iota
	kStart
	kEnd
	kWordBoundary
	kNotWordBoundary
	kLookahead
	kNegLookahead
	kLookbehind
	kNegLookbehind
	kGroup
	kModifiers
	kBackref
	kRepeat
	kDisjunction
)

type node struct {
	kind kind

	// kSet: the members, or every code unit but the line terminators for
	// ".". negate is a [^...] class, applied after case folding.
	set    *unitSet
	negate bool
	dot    bool

	// kGroup: capture is false for (?:...). index is the JavaScript group
	// number.
	capture bool
	index   int

	// kModifiers: flags turned on and off for the contents.
	add, remove flags

	// kBackref: the groups it may refer to (several for a name used by
	// groups in different alternatives).
	refs []int
	// refName is the unresolved \k<name>.
	refName string

	// kRepeat
	min, max int64 // max -1: no limit
	lazy     bool

	child *node     // groups, lookarounds, modifiers, repeat
	alts  [][]*node // kDisjunction
}

type flags struct {
	i, m, s bool
}

type pathElem struct {
	disj, alt int
}

type namedGroup struct {
	index int
	path  []pathElem
}

type parser struct {
	src        string
	u          []uint16
	pos        int
	groupCount int
	hasNames   bool

	nextGroup int
	nextDisj  int
	path      []pathElem
	names     map[string][]namedGroup
	refs      []*node

	// regexpTreeFails marks syntax the regexp-tree parser that safe-regex
	// uses cannot read (modifier groups, a quantified lookahead, duplicate
	// group names); safe-regex then reports the pattern as safe.
	regexpTreeFails bool
}

// Pattern is a parsed JavaScript regular expression.
type Pattern struct {
	Source          string
	root            *node
	groups          int
	regexpTreeFails bool
}

// Parse reads a JavaScript pattern (non-unicode mode) and reports V8's
// syntax error for one it refuses.
func Parse(src string) (*Pattern, error) {
	p := &parser{src: src, u: utf16.Encode([]rune(src)), names: map[string][]namedGroup{}}
	p.scanCaptures()
	root, err := p.disjunction()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.u) {
		// disjunction stops only at the end or at ')'.
		return nil, p.fail("Unmatched ')'")
	}
	for _, ref := range p.refs {
		groups, ok := p.names[ref.refName]
		if !ok {
			return nil, p.fail("Invalid named capture referenced")
		}
		for _, g := range groups {
			ref.refs = append(ref.refs, g.index)
		}
	}
	return &Pattern{Source: src, root: root, groups: p.nextGroup, regexpTreeFails: p.regexpTreeFails}, nil
}

func (p *parser) fail(reason string) error {
	return &SyntaxError{Source: p.src, Reason: reason}
}

// scanCaptures counts the capture groups and notes whether any is named,
// before parsing, as V8 does: a decimal escape is a backreference only up
// to the number of groups in the whole pattern, and \k is a named reference
// only when the pattern has a named group.
func (p *parser) scanCaptures() {
	u := p.u
	for i := 0; i < len(u); i++ {
		switch u[i] {
		case '\\':
			i++
		case '[':
			for i++; i < len(u) && u[i] != ']'; i++ {
				if u[i] == '\\' {
					i++
				}
			}
		case '(':
			if i+1 < len(u) && u[i+1] == '?' {
				if i+2 < len(u) && u[i+2] == '<' && i+3 < len(u) && u[i+3] != '=' && u[i+3] != '!' {
					p.groupCount++
					p.hasNames = true
				}
			} else {
				p.groupCount++
			}
		}
	}
}

func (p *parser) peek(off int) int {
	if p.pos+off < len(p.u) {
		return int(p.u[p.pos+off])
	}
	return -1
}

func (p *parser) disjunction() (*node, error) {
	id := p.nextDisj
	p.nextDisj++
	n := &node{kind: kDisjunction}
	for alt := 0; ; alt++ {
		p.path = append(p.path, pathElem{id, alt})
		terms, err := p.alternative()
		p.path = p.path[:len(p.path)-1]
		if err != nil {
			return nil, err
		}
		n.alts = append(n.alts, terms)
		if p.peek(0) != '|' {
			return n, nil
		}
		p.pos++
	}
}

func (p *parser) alternative() ([]*node, error) {
	var terms []*node
	for {
		c := p.peek(0)
		if c == -1 || c == '|' || c == ')' {
			return terms, nil
		}
		t, err := p.term()
		if err != nil {
			return nil, err
		}
		terms = append(terms, t)
	}
}

// quantifierAt reports whether a quantifier starts at offset off from pos,
// and its length. "{" that does not form {n}, {n,} or {n,m} is a literal
// (Annex B).
func (p *parser) quantifierAt(off int) (min, max int64, length int, ok bool) {
	i := p.pos + off
	if i >= len(p.u) {
		return 0, 0, 0, false
	}
	switch p.u[i] {
	case '*':
		return 0, -1, 1, true
	case '+':
		return 1, -1, 1, true
	case '?':
		return 0, 1, 1, true
	case '{':
	default:
		return 0, 0, 0, false
	}
	j := i + 1
	min, j, ok = p.number(j)
	if !ok {
		return 0, 0, 0, false
	}
	max = min
	if j < len(p.u) && p.u[j] == ',' {
		j++
		if j < len(p.u) && p.u[j] == '}' {
			max = -1
		} else {
			max, j, ok = p.number(j)
			if !ok {
				return 0, 0, 0, false
			}
		}
	}
	if j >= len(p.u) || p.u[j] != '}' {
		return 0, 0, 0, false
	}
	return min, max, j + 1 - i, true
}

// number reads decimal digits at i, saturating far above any useful count.
func (p *parser) number(i int) (int64, int, bool) {
	start := i
	var n int64
	for i < len(p.u) && p.u[i] >= '0' && p.u[i] <= '9' {
		if n < 1<<53 {
			n = n*10 + int64(p.u[i]-'0')
		}
		i++
	}
	return n, i, i > start
}

func (p *parser) quantifierFollows() bool {
	_, _, _, ok := p.quantifierAt(0)
	return ok
}

// quantify applies a quantifier after an atom, if one follows.
func (p *parser) quantify(atom *node) (*node, error) {
	min, max, length, ok := p.quantifierAt(0)
	if !ok {
		return atom, nil
	}
	if max != -1 && min > max {
		return nil, p.fail("numbers out of order in {} quantifier")
	}
	p.pos += length
	lazy := false
	if p.peek(0) == '?' {
		lazy = true
		p.pos++
	}
	return &node{kind: kRepeat, child: atom, min: min, max: max, lazy: lazy}, nil
}

func (p *parser) term() (*node, error) {
	c := p.peek(0)
	switch c {
	case '^', '$':
		p.pos++
		if p.quantifierFollows() {
			return nil, p.fail("Nothing to repeat")
		}
		if c == '^' {
			return &node{kind: kStart}, nil
		}
		return &node{kind: kEnd}, nil
	case '\\':
		if b := p.peek(1); b == 'b' || b == 'B' {
			p.pos += 2
			if p.quantifierFollows() {
				return nil, p.fail("Nothing to repeat")
			}
			if b == 'b' {
				return &node{kind: kWordBoundary}, nil
			}
			return &node{kind: kNotWordBoundary}, nil
		}
		atom, err := p.atomEscape()
		if err != nil {
			return nil, err
		}
		return p.quantify(atom)
	case '(':
		return p.group()
	case '[':
		atom, err := p.class()
		if err != nil {
			return nil, err
		}
		return p.quantify(atom)
	case '.':
		p.pos++
		return p.quantify(&node{kind: kSet, dot: true})
	case '*', '+', '?':
		return nil, p.fail("Nothing to repeat")
	case '{':
		if p.quantifierFollows() {
			return nil, p.fail("Nothing to repeat")
		}
	}
	p.pos++
	return p.quantify(unitNode(uint16(c)))
}

func unitNode(u uint16) *node {
	s := &unitSet{}
	s.add(u)
	return &node{kind: kSet, set: s}
}

func (p *parser) group() (*node, error) {
	start := p.pos
	p.pos++ // (
	n := &node{}
	quantifiable := true
	if p.peek(0) == '?' {
		switch a, b := p.peek(1), p.peek(2); {
		case a == '=' || a == '!':
			n.kind = kLookahead
			if a == '!' {
				n.kind = kNegLookahead
			}
			p.pos += 2
		case a == '<' && (b == '=' || b == '!'):
			n.kind = kLookbehind
			if b == '!' {
				n.kind = kNegLookbehind
			}
			quantifiable = false
			p.pos += 3
		case a == ':':
			n.kind = kGroup
			p.pos += 2
		case a == '<':
			p.pos += 2
			name, err := p.groupName()
			if err != nil {
				return nil, err
			}
			n.kind, n.capture = kGroup, true
			p.nextGroup++
			n.index = p.nextGroup
			if err := p.register(name, n.index); err != nil {
				return nil, err
			}
		case a == 'i' || a == 'm' || a == 's' || a == '-':
			p.pos++
			if err := p.modifiers(n); err != nil {
				return nil, err
			}
			p.regexpTreeFails = true
		default:
			return nil, p.fail("Invalid group")
		}
	} else {
		n.kind, n.capture = kGroup, true
		p.nextGroup++
		n.index = p.nextGroup
	}
	child, err := p.disjunction()
	if err != nil {
		return nil, err
	}
	if p.peek(0) != ')' {
		return nil, p.fail("Unterminated group")
	}
	p.pos++
	n.child = child
	_ = start

	if !quantifiable {
		if p.quantifierFollows() {
			return nil, p.fail("Invalid quantifier")
		}
		return n, nil
	}
	q, err := p.quantify(n)
	if err != nil {
		return nil, err
	}
	if q != n && (n.kind == kLookahead || n.kind == kNegLookahead) {
		p.regexpTreeFails = true
	}
	return q, nil
}

// modifiers reads "ims-ims:" after "(?", for an ES2025 modifier group.
func (p *parser) modifiers(n *node) error {
	n.kind = kModifiers
	seen := map[int]bool{}
	read := func(set *flags) (int, error) {
		count := 0
		for {
			c := p.peek(0)
			if c != 'i' && c != 'm' && c != 's' {
				return count, nil
			}
			if seen[c] {
				return 0, p.fail("Repeated flag in flag group")
			}
			seen[c] = true
			switch c {
			case 'i':
				set.i = true
			case 'm':
				set.m = true
			case 's':
				set.s = true
			}
			count++
			p.pos++
		}
	}
	added, err := read(&n.add)
	if err != nil {
		return err
	}
	removed := 0
	dash := false
	if p.peek(0) == '-' {
		dash = true
		p.pos++
		if removed, err = read(&n.remove); err != nil {
			return err
		}
	}
	if p.peek(0) != ':' {
		return p.fail("Invalid group")
	}
	if dash && added == 0 && removed == 0 {
		return p.fail("Invalid flag group")
	}
	p.pos++
	return nil
}

// groupName reads "name>" for (?<name> and \k<name>. A name may spell a
// character as a \u escape: four hex digits (a surrogate pair as two such
// escapes) or hex digits in braces.
func (p *parser) groupName() (string, error) {
	var runes []rune
	for {
		c := p.peek(0)
		if c == -1 {
			return "", p.fail("Invalid capture group name")
		}
		if c == '>' {
			p.pos++
			break
		}
		if c == '\\' {
			r, reason := p.nameEscape()
			if reason != "" {
				return "", p.fail(reason)
			}
			runes = append(runes, r)
			continue
		}
		if utf16.IsSurrogate(rune(c)) {
			lo := p.peek(1)
			if c > 0xDBFF || lo < 0xDC00 || lo > 0xDFFF {
				return "", p.fail("Invalid capture group name")
			}
			runes = append(runes, utf16.DecodeRune(rune(c), rune(lo)))
			p.pos += 2
			continue
		}
		runes = append(runes, rune(c))
		p.pos++
	}
	if len(runes) == 0 {
		return "", p.fail("Invalid capture group name")
	}
	for i, r := range runes {
		if !(r == '$' || r == '_' || idStart(r) || i > 0 && (idContinue(r) || r == '\u200c' || r == '\u200d')) {
			return "", p.fail("Invalid capture group name")
		}
	}
	return string(runes), nil
}

// nameEscape reads one escape in a group name; pos is at the backslash. A
// malformed \u escape is V8's "Invalid Unicode escape"; a well-formed one
// for a lone surrogate, or any other escape, is "Invalid capture group
// name". It returns the reason, "" when the escape is good.
func (p *parser) nameEscape() (rune, string) {
	const badEscape, badName = "Invalid Unicode escape", "Invalid capture group name"
	if p.peek(1) != 'u' {
		return 0, badName
	}
	if p.peek(2) == '{' {
		i := 3
		var v rune
		for isHex(p.peek(i)) {
			d, _ := strconv.ParseUint(string(rune(p.peek(i))), 16, 8)
			v = v*16 + rune(d)
			if v > 0x10FFFF {
				return 0, badEscape
			}
			i++
		}
		if i == 3 || p.peek(i) != '}' {
			return 0, badEscape
		}
		p.pos += i + 1
		if utf16.IsSurrogate(v) {
			return 0, badName
		}
		return v, ""
	}
	v, ok := p.hex(2, 4)
	if !ok {
		return 0, badEscape
	}
	p.pos += 6
	if v >= 0xD800 && v <= 0xDBFF && p.peek(0) == '\\' && p.peek(1) == 'u' {
		if lo, ok := p.hex(2, 4); ok && lo >= 0xDC00 && lo <= 0xDFFF {
			p.pos += 6
			return utf16.DecodeRune(rune(v), rune(lo)), ""
		}
	}
	if utf16.IsSurrogate(rune(v)) {
		return 0, badName
	}
	return rune(v), ""
}

func idStart(r rune) bool {
	return unicode.IsLetter(r) || unicode.Is(unicode.Nl, r) || unicode.Is(unicode.Other_ID_Start, r)
}

func idContinue(r rune) bool {
	return idStart(r) || unicode.In(r, unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc) || unicode.Is(unicode.Other_ID_Continue, r)
}

// register records a named group. A name may repeat only in different
// alternatives, where both groups cannot match at once (ES2025).
func (p *parser) register(name string, index int) error {
	path := append([]pathElem(nil), p.path...)
	for _, g := range p.names[name] {
		if !exclusive(g.path, path) {
			return p.fail("Duplicate capture group name")
		}
		p.regexpTreeFails = true
	}
	p.names[name] = append(p.names[name], namedGroup{index: index, path: path})
	return nil
}

// exclusive reports whether two positions sit in different alternatives of
// a common disjunction.
func exclusive(a, b []pathElem) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i].disj != b[i].disj {
			return false
		}
		if a[i].alt != b[i].alt {
			return true
		}
	}
	return false
}

func isHex(c int) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func (p *parser) hex(off, n int) (uint16, bool) {
	var v uint16
	for k := 0; k < n; k++ {
		c := p.peek(off + k)
		if !isHex(c) {
			return 0, false
		}
		d, _ := strconv.ParseUint(string(rune(c)), 16, 8)
		v = v*16 + uint16(d)
	}
	return v, true
}

func isASCIILetter(c int) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// legacyOctal reads an Annex B octal escape at pos (the first digit, 0-7):
// up to three digits when it starts with 0-3, else up to two.
func (p *parser) legacyOctal() uint16 {
	first := p.peek(0) - '0'
	v := first
	p.pos++
	limit := 1
	if first <= 3 {
		limit = 2
	}
	for k := 0; k < limit; k++ {
		c := p.peek(0)
		if c < '0' || c > '7' {
			break
		}
		v = v*8 + (c - '0')
		p.pos++
	}
	return uint16(v)
}

// controlEscape is \f \n \r \t \v.
func controlEscape(c int) (uint16, bool) {
	switch c {
	case 'f':
		return 0x0c, true
	case 'n':
		return 0x0a, true
	case 'r':
		return 0x0d, true
	case 't':
		return 0x09, true
	case 'v':
		return 0x0b, true
	}
	return 0, false
}

// atomEscape reads an escape outside a class; pos is at the backslash.
func (p *parser) atomEscape() (*node, error) {
	c := p.peek(1)
	if c == -1 {
		return nil, p.fail("\\ at end of pattern")
	}
	if s := classEscape(c); s != nil {
		p.pos += 2
		return &node{kind: kSet, set: s}, nil
	}
	if u, ok := controlEscape(c); ok {
		p.pos += 2
		return unitNode(u), nil
	}
	switch {
	case c == 'c':
		if l := p.peek(2); isASCIILetter(l) {
			p.pos += 3
			return unitNode(uint16(l % 32)), nil
		}
		// "\c" without a letter is a literal backslash; the c follows.
		p.pos++
		return unitNode('\\'), nil
	case c == '0' && !(p.peek(2) >= '0' && p.peek(2) <= '9'):
		p.pos += 2
		return unitNode(0), nil
	case c >= '0' && c <= '9':
		save := p.pos
		n, end, _ := p.number(p.pos + 1)
		if n >= 1 && n <= int64(p.groupCount) {
			p.pos = end
			return &node{kind: kBackref, refs: []int{int(n)}}, nil
		}
		p.pos = save + 1
		if c == '8' || c == '9' {
			p.pos++
			return unitNode(uint16(c)), nil
		}
		return unitNode(p.legacyOctal()), nil
	case c == 'k' && p.hasNames:
		if p.peek(2) != '<' {
			return nil, p.fail("Invalid named reference")
		}
		p.pos += 3
		name, err := p.groupName()
		if err != nil {
			return nil, err
		}
		ref := &node{kind: kBackref, refName: name}
		p.refs = append(p.refs, ref)
		return ref, nil
	case c == 'x':
		if v, ok := p.hex(2, 2); ok {
			p.pos += 4
			return unitNode(v), nil
		}
	case c == 'u':
		if v, ok := p.hex(2, 4); ok {
			p.pos += 6
			return unitNode(v), nil
		}
	}
	// Identity escape: the character itself.
	p.pos += 2
	return unitNode(uint16(c)), nil
}

// classEscape is \d \D \s \S \w \W, or nil.
func classEscape(c int) *unitSet {
	switch c {
	case 'd':
		return digitSet()
	case 'D':
		return digitSet().complement()
	case 's':
		return spaceSet()
	case 'S':
		return spaceSet().complement()
	case 'w':
		return wordSet()
	case 'W':
		return wordSet().complement()
	}
	return nil
}

// class reads [...]; pos is at the "[".
func (p *parser) class() (*node, error) {
	p.pos++
	n := &node{kind: kSet, set: &unitSet{}}
	if p.peek(0) == '^' {
		n.negate = true
		p.pos++
	}
	for {
		c := p.peek(0)
		if c == -1 {
			return nil, p.fail("Unterminated character class")
		}
		if c == ']' {
			p.pos++
			return n, nil
		}
		a, aSet, err := p.classAtom()
		if err != nil {
			return nil, err
		}
		if p.peek(0) == '-' && p.peek(1) != -1 && p.peek(1) != ']' {
			p.pos++
			b, bSet, err := p.classAtom()
			if err != nil {
				return nil, err
			}
			if aSet != nil || bSet != nil {
				// A class escape in a range is no range (Annex B): both
				// ends and the "-" are members.
				addAtom(n.set, a, aSet)
				n.set.add('-')
				addAtom(n.set, b, bSet)
				continue
			}
			if a > b {
				return nil, p.fail("Range out of order in character class")
			}
			n.set.addRange(a, b)
			continue
		}
		addAtom(n.set, a, aSet)
	}
}

func addAtom(s *unitSet, u uint16, set *unitSet) {
	if set != nil {
		s.union(set)
	} else {
		s.add(u)
	}
}

// classAtom reads one class member: a code unit, or a class escape set.
func (p *parser) classAtom() (uint16, *unitSet, error) {
	c := p.peek(0)
	if c != '\\' {
		p.pos++
		return uint16(c), nil, nil
	}
	c = p.peek(1)
	if c == -1 {
		return 0, nil, p.fail("\\ at end of pattern")
	}
	if s := classEscape(c); s != nil {
		p.pos += 2
		return 0, s, nil
	}
	if u, ok := controlEscape(c); ok {
		p.pos += 2
		return u, nil, nil
	}
	switch {
	case c == 'b':
		p.pos += 2
		return 0x08, nil, nil
	case c == 'c':
		// In a class, \c also takes a digit or "_" (Annex B).
		if l := p.peek(2); isASCIILetter(l) || l >= '0' && l <= '9' || l == '_' {
			p.pos += 3
			return uint16(l % 32), nil, nil
		}
		p.pos++
		return '\\', nil, nil
	case c >= '0' && c <= '7':
		p.pos++
		return p.legacyOctal(), nil, nil
	case c == 'x':
		if v, ok := p.hex(2, 2); ok {
			p.pos += 4
			return v, nil, nil
		}
	case c == 'u':
		if v, ok := p.hex(2, 4); ok {
			p.pos += 6
			return v, nil, nil
		}
	}
	p.pos += 2
	return uint16(c), nil, nil
}
