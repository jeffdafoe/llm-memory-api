package jsregex

import (
	"context"
	"errors"
	"unicode/utf16"
)

// ErrBudget is returned when a match would need more steps than its
// budget allows (catastrophic backtracking), or the request ended.
var ErrBudget = errors.New("regex match exceeded its work budget")

// Budget bounds the backtracking work of a series of matches, for example
// every line of one grep request. Node had no bound: a pathological pattern
// that passed safe-regex held its event loop until done.
type Budget struct {
	ctx   context.Context
	steps int64
}

// NewBudget allows at most steps matcher steps in total, and stops early
// once ctx is done.
func NewBudget(ctx context.Context, steps int64) *Budget {
	return &Budget{ctx: ctx, steps: steps}
}

// Regexp is a compiled pattern that matches the way `new RegExp(source,
// "i").test(line)` does.
type Regexp struct {
	m      matcher
	groups int
}

// CompileIgnoreCase parses source and compiles it with the i flag.
func CompileIgnoreCase(source string) (*Regexp, error) {
	p, err := Parse(source)
	if err != nil {
		return nil, err
	}
	c := &compiler{}
	m := c.disjunction(p.root, flags{i: true}, true)
	return &Regexp{m: m, groups: p.groups}, nil
}

// MatchString reports whether the pattern matches anywhere in s, matched as
// UTF-16 code units. It spends from b; past the budget it returns ErrBudget.
func (re *Regexp) MatchString(s string, b *Budget) (bool, error) {
	r := &run{input: utf16.Encode([]rune(s)), budget: b}
	for start := 0; start <= len(r.input); start++ {
		caps := make([]int, 2*(re.groups+1))
		for i := range caps {
			caps[i] = -1
		}
		if re.m(r, state{end: start, caps: caps}, func(*run, state) bool { return true }) {
			return true, nil
		}
		if r.err != nil {
			return false, r.err
		}
	}
	return false, r.err
}

// The matcher follows the ECMAScript specification's pattern semantics
// (22.2.2): a matcher takes a state and a continuation and succeeds when
// the continuation does, which gives backtracking in the spec's order.
type state struct {
	end  int
	caps []int // start, end per group; -1 when the group has not matched
}

type cont func(*run, state) bool

type matcher func(*run, state, cont) bool

type run struct {
	input  []uint16
	budget *Budget
	err    error
	// depth is the number of repetition iterations on the Go stack. Each
	// iteration nests a few calls, so a greedy loop over a very long line
	// would otherwise grow the stack without limit.
	depth int
}

// maxRepeatDepth caps nested repetition iterations per match (about a
// hundred megabytes of stack at most). A line that needs more fails like an
// exhausted budget.
const maxRepeatDepth = 200_000

// step spends one unit of work, and reports false once the run must stop.
func (r *run) step() bool {
	if r.err != nil {
		return false
	}
	b := r.budget
	b.steps--
	if b.steps < 0 {
		r.err = ErrBudget
		return false
	}
	if b.steps&0xffff == 0 && b.ctx != nil && b.ctx.Err() != nil {
		r.err = ErrBudget
		return false
	}
	return true
}

type compiler struct{}

func (c *compiler) disjunction(n *node, f flags, forward bool) matcher {
	alts := make([]matcher, len(n.alts))
	for i, alt := range n.alts {
		alts[i] = c.alternative(alt, f, forward)
	}
	if len(alts) == 1 {
		return alts[0]
	}
	return func(r *run, x state, k cont) bool {
		for _, m := range alts {
			if m(r, x, k) {
				return true
			}
			if r.err != nil {
				return false
			}
		}
		return false
	}
}

// alternative chains its terms; backward (inside a lookbehind) the last
// term matches first.
func (c *compiler) alternative(terms []*node, f flags, forward bool) matcher {
	m := func(r *run, x state, k cont) bool { return k(r, x) }
	ordered := terms
	if !forward {
		ordered = make([]*node, len(terms))
		for i, t := range terms {
			ordered[len(terms)-1-i] = t
		}
	}
	for i := len(ordered) - 1; i >= 0; i-- {
		head, rest := c.term(ordered[i], f, forward), m
		m = func(r *run, x state, k cont) bool {
			return head(r, x, func(r *run, y state) bool { return rest(r, y, k) })
		}
	}
	return m
}

func isLineTerminator(u uint16) bool {
	return u == 0x0a || u == 0x0d || u == 0x2028 || u == 0x2029
}

func isWordUnit(u uint16) bool {
	return u >= 'a' && u <= 'z' || u >= 'A' && u <= 'Z' || u >= '0' && u <= '9' || u == '_'
}

func (r *run) wordAt(i int) bool {
	return i >= 0 && i < len(r.input) && isWordUnit(r.input[i])
}

func (c *compiler) term(n *node, f flags, forward bool) matcher {
	switch n.kind {
	case kSet:
		return setMatcher(effectiveSet(n, f), forward)
	case kStart:
		multiline := f.m
		return func(r *run, x state, k cont) bool {
			if !r.step() {
				return false
			}
			e := x.end
			if e == 0 || multiline && isLineTerminator(r.input[e-1]) {
				return k(r, x)
			}
			return false
		}
	case kEnd:
		multiline := f.m
		return func(r *run, x state, k cont) bool {
			if !r.step() {
				return false
			}
			e := x.end
			if e == len(r.input) || multiline && isLineTerminator(r.input[e]) {
				return k(r, x)
			}
			return false
		}
	case kWordBoundary, kNotWordBoundary:
		want := n.kind == kWordBoundary
		return func(r *run, x state, k cont) bool {
			if !r.step() {
				return false
			}
			if (r.wordAt(x.end-1) != r.wordAt(x.end)) == want {
				return k(r, x)
			}
			return false
		}
	case kLookahead, kNegLookahead, kLookbehind, kNegLookbehind:
		inner := c.disjunction(n.child, f, n.kind == kLookahead || n.kind == kNegLookahead)
		negative := n.kind == kNegLookahead || n.kind == kNegLookbehind
		return func(r *run, x state, k cont) bool {
			if !r.step() {
				return false
			}
			// A lookaround is atomic: once it matches, its alternatives are
			// not retried when the rest fails.
			var y state
			ok := inner(r, x, func(_ *run, s state) bool { y = s; return true })
			if r.err != nil {
				return false
			}
			if negative {
				if ok {
					return false
				}
				return k(r, x)
			}
			if !ok {
				return false
			}
			return k(r, state{end: x.end, caps: y.caps})
		}
	case kGroup:
		inner := c.disjunction(n.child, f, forward)
		if !n.capture {
			return inner
		}
		idx := n.index
		return func(r *run, x state, k cont) bool {
			return inner(r, x, func(r *run, y state) bool {
				caps := append([]int(nil), y.caps...)
				if forward {
					caps[2*idx], caps[2*idx+1] = x.end, y.end
				} else {
					caps[2*idx], caps[2*idx+1] = y.end, x.end
				}
				return k(r, state{end: y.end, caps: caps})
			})
		}
	case kModifiers:
		inner := f
		if n.add.i {
			inner.i = true
		}
		if n.add.m {
			inner.m = true
		}
		if n.add.s {
			inner.s = true
		}
		if n.remove.i {
			inner.i = false
		}
		if n.remove.m {
			inner.m = false
		}
		if n.remove.s {
			inner.s = false
		}
		return c.disjunction(n.child, inner, forward)
	case kBackref:
		return backrefMatcher(n.refs, f.i, forward)
	case kRepeat:
		atom := c.term(n.child, f, forward)
		first, count := captureRange(n.child)
		return repeatMatcher(atom, n.min, n.max, !n.lazy, first, count)
	}
	panic("jsregex: unknown node")
}

// effectiveSet is the code units an atom matches under the flags.
func effectiveSet(n *node, f flags) *unitSet {
	if n.dot {
		s := (&unitSet{}).complement()
		if !f.s {
			lt := lineTerminators()
			for i := range s {
				s[i] &^= lt[i]
			}
		}
		return s
	}
	s := n.set
	if f.i {
		s = s.fold()
	}
	if n.negate {
		s = s.complement()
	}
	return s
}

func setMatcher(s *unitSet, forward bool) matcher {
	return func(r *run, x state, k cont) bool {
		if !r.step() {
			return false
		}
		e := x.end
		if forward {
			if e >= len(r.input) || !s.has(r.input[e]) {
				return false
			}
			return k(r, state{end: e + 1, caps: x.caps})
		}
		if e <= 0 || !s.has(r.input[e-1]) {
			return false
		}
		return k(r, state{end: e - 1, caps: x.caps})
	}
}

// backrefMatcher matches the text a group captured. A group that has not
// matched matches empty. Under i each code unit compares by Canonicalize.
// With a name used by several groups (in different alternatives) at most
// one has matched.
func backrefMatcher(refs []int, ignoreCase bool, forward bool) matcher {
	return func(r *run, x state, k cont) bool {
		if !r.step() {
			return false
		}
		start, end := -1, -1
		for _, idx := range refs {
			if x.caps[2*idx] >= 0 {
				start, end = x.caps[2*idx], x.caps[2*idx+1]
				break
			}
		}
		if start < 0 {
			return k(r, x)
		}
		n := end - start
		e := x.end
		from := e
		if !forward {
			from = e - n
		}
		if from < 0 || from+n > len(r.input) {
			return false
		}
		// Each compared code unit is a step: backtracking over shorter and
		// shorter captures (^(a+)\1$) would otherwise do quadratic work for
		// linear steps.
		for i := 0; i < n; i++ {
			if !r.step() {
				return false
			}
			a, b := r.input[start+i], r.input[from+i]
			if a != b && !(ignoreCase && canonical(a) == canonical(b)) {
				return false
			}
		}
		if forward {
			return k(r, state{end: e + n, caps: x.caps})
		}
		return k(r, state{end: e - n, caps: x.caps})
	}
}

func canonical(u uint16) uint16 {
	foldOnce.Do(buildCanon)
	return canon[u]
}

// captureRange is the capture groups inside n: the first index and how many.
func captureRange(n *node) (first, count int) {
	first = -1
	var walk func(*node)
	walk = func(n *node) {
		if n == nil {
			return
		}
		if n.kind == kGroup && n.capture {
			if first == -1 || n.index < first {
				first = n.index
			}
			count++
		}
		walk(n.child)
		for _, alt := range n.alts {
			for _, t := range alt {
				walk(t)
			}
		}
	}
	walk(n)
	return first, count
}

// repeatMatcher is the spec's RepeatMatcher: each iteration starts with the
// atom's captures cleared, an iteration past the minimum that matches empty
// fails, and greedy tries one more iteration before the continuation.
func repeatMatcher(atom matcher, min, max int64, greedy bool, firstCap, capCount int) matcher {
	var rep func(r *run, x state, k cont, min, max int64) bool
	rep = func(r *run, x state, k cont, min, max int64) bool {
		if !r.step() {
			return false
		}
		r.depth++
		defer func() { r.depth-- }()
		if r.depth > maxRepeatDepth {
			r.err = ErrBudget
			return false
		}
		if max == 0 {
			return k(r, x)
		}
		next := func(r *run, y state) bool {
			if min == 0 && y.end == x.end {
				return false
			}
			newMin, newMax := min, max
			if newMin > 0 {
				newMin--
			}
			if newMax > 0 {
				newMax--
			}
			return rep(r, y, k, newMin, newMax)
		}
		xr := x
		if capCount > 0 {
			caps := append([]int(nil), x.caps...)
			for i := firstCap; i < firstCap+capCount; i++ {
				caps[2*i], caps[2*i+1] = -1, -1
			}
			xr = state{end: x.end, caps: caps}
		}
		if min > 0 {
			return atom(r, xr, next)
		}
		if !greedy {
			if k(r, x) {
				return true
			}
			if r.err != nil {
				return false
			}
			return atom(r, xr, next)
		}
		if atom(r, xr, next) {
			return true
		}
		if r.err != nil {
			return false
		}
		return k(r, x)
	}
	return func(r *run, x state, k cont) bool {
		return rep(r, x, k, min, max)
	}
}
