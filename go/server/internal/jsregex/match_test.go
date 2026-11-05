package jsregex

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// (a|aa)+$ passes safe-regex (as in Node) but backtracks exponentially on a
// run of "a" that fails at the end. The budget stops it.
func TestBudgetStopsCatastrophicBacktracking(t *testing.T) {
	p, err := Parse("(a|aa)+$")
	if err != nil || !p.Safe() {
		t.Fatalf("parse %v, safe %v: the pattern must reach the matcher", err, p != nil && p.Safe())
	}
	re, err := CompileIgnoreCase("(a|aa)+$")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = re.MatchString(strings.Repeat("a", 60)+"!", NewBudget(context.Background(), 2_000_000))
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("stopping took %v", d)
	}
}

func TestBudgetIsSharedAcrossLines(t *testing.T) {
	re, err := CompileIgnoreCase("b")
	if err != nil {
		t.Fatal(err)
	}
	b := NewBudget(context.Background(), 50)
	var err2 error
	for i := 0; i < 100 && err2 == nil; i++ {
		_, err2 = re.MatchString("aaaaaaaaaa", b)
	}
	if !errors.Is(err2, ErrBudget) {
		t.Errorf("err = %v, want ErrBudget once the shared budget is spent", err2)
	}
}

func TestCanceledContextStopsTheMatch(t *testing.T) {
	re, err := CompileIgnoreCase("(a|aa)+$")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = re.MatchString(strings.Repeat("a", 60)+"!", NewBudget(ctx, 1<<40))
	if !errors.Is(err, ErrBudget) {
		t.Errorf("err = %v, want ErrBudget after cancel", err)
	}
}

func TestOrdinaryMatches(t *testing.T) {
	cases := []struct {
		pattern, line string
		want          bool
	}{
		{`^(a|(b))+\2$`, "aba", true},
		{`^(k)\1$`, "kK", false},
		{`^(Σ)\1$`, "Σς", true},
		{`(?<=\$)\d+`, "costs $25", true},
		{`(?<!\$)\b\d+`, "costs $25", false},
		{`^\s*$`, " 　", true},
		{`^.$`, "\U0001F600", false},
		{`^..$`, "\U0001F600", true},
	}
	for _, c := range cases {
		re, err := CompileIgnoreCase(c.pattern)
		if err != nil {
			t.Errorf("%q: %v", c.pattern, err)
			continue
		}
		got, err := re.MatchString(c.line, NewBudget(context.Background(), 1_000_000))
		if err != nil || got != c.want {
			t.Errorf("%q on %q = %v (%v), want %v", c.pattern, c.line, got, err, c.want)
		}
	}
}

// A greedy loop over a line longer than the depth cap fails cleanly
// instead of growing the stack without limit.
func TestDepthCapOnAVeryLongLine(t *testing.T) {
	re, err := CompileIgnoreCase("^a.*z$")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := re.MatchString("a"+strings.Repeat("b", 50_000)+"z", NewBudget(context.Background(), 100_000_000))
	if err != nil || !ok {
		t.Errorf("50k line: %v %v", ok, err)
	}
	_, err = re.MatchString("a"+strings.Repeat("b", maxRepeatDepth+10)+"z", NewBudget(context.Background(), 100_000_000))
	if !errors.Is(err, ErrBudget) {
		t.Errorf("past the depth cap: err = %v, want ErrBudget", err)
	}
}

// Required empty iterations and long consuming loops stop at the depth cap,
// before the Go stack grows without limit.
func TestDepthCapOnRequiredEmptyIterations(t *testing.T) {
	for _, c := range []struct{ pattern, line string }{
		{"(?:){10000000}", ""},
		{"^a+$", strings.Repeat("a", maxRepeatDepth+10)},
	} {
		re, err := CompileIgnoreCase(c.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := re.MatchString(c.line, NewBudget(context.Background(), 1_000_000_000)); !errors.Is(err, ErrBudget) {
			t.Errorf("%q: err = %v, want ErrBudget", c.pattern, err)
		}
	}
}

// A backreference charges each compared unit, so ^(a+)\1$ on a long run
// that fails at the end exhausts a small budget.
func TestBackreferenceComparisonsSpendTheBudget(t *testing.T) {
	re, err := CompileIgnoreCase(`^(a+)\1$`)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("a", 20_000) + "!"
	if _, err := re.MatchString(line, NewBudget(context.Background(), 500_000)); !errors.Is(err, ErrBudget) {
		t.Errorf("err = %v, want ErrBudget", err)
	}
}
