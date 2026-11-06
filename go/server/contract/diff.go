package contract

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ComparedHeaders are the response headers clients read. Only these are
// compared; Date, ETag, X-Powered-By and the like are not part of the
// contract. For set-cookie only the cookie names are kept.
var ComparedHeaders = []string{
	"content-type",
	"www-authenticate",
	"cache-control",
	"content-security-policy",
	"x-content-type-options",
	"location",
	"allow",
	"set-cookie",
	// MCP: HEAD's protocol version, the session id a stateless server never
	// sends, and the SSE stream headers clients read.
	"mcp-protocol-version",
	"mcp-session-id",
	"x-accel-buffering",
}

// Header is one compared response header with every value it had, already
// normalized. A header that was not sent has no Values.
type Header struct {
	Name   string
	Values []string
}

// Body is a response or outbound request body: a JSON value when it parsed
// as one, else the normalized text.
type Body struct {
	IsJSON bool
	JSON   Value
	Text   string
}

// Response is what one request step got back.
type Response struct {
	Status  int
	Headers []Header
	Body    Body
}

// StepResult is one request step of a scenario on one server. A step that
// could not be sent or read fails the run instead (Runner.Run), so every
// StepResult has a response.
type StepResult struct {
	Label    string
	Response Response
}

// Outbound is one request a server sent to the fake upstream.
type Outbound struct {
	Upstream string
	Method   string
	Path     string
	// Auth names the credential headers the request carried
	// (authorization, x-api-key, x-goog-api-key), not their values.
	Auth []string
	Body Body
}

// Label is how an outbound request is named in a diff.
func (o Outbound) Label() string {
	return o.Upstream + " " + o.Method + " " + o.Path
}

// Table is one table's rows after a scenario, each row a JSON object in
// column order. Multiset tables are compared without regard to row order.
type Table struct {
	Name     string
	Rows     []Value
	Multiset bool
}

// Result is everything one server produced for one scenario, normalized. It
// is plain data so it can be written to a file and read back.
type Result struct {
	Steps    []StepResult
	Outbound []Outbound
	Tables   []Table
}

// Difference is one place where Go did not do what Node did.
type Difference struct {
	Where string
	Node  string
	Go    string
}

// KnownDifference allows a difference on purpose. Where is the exact
// location from a diff, or a pattern in which each "*" matches any run of
// characters ("table request_log row *.response_length"; a trailing "*" is
// a prefix). Reason says why, and should match "Known differences" in the
// go-server codebase note.
type KnownDifference struct {
	Where  string
	Reason string
	// Volatile marks a value that differs between two correct runs by
	// design and is not shaped like anything the normalizer renames (the
	// length of a response that carries a random passphrase). Such an entry
	// may match nothing, and needs no go-server note item.
	Volatile bool
}

func (k KnownDifference) matches(where string) bool {
	parts := strings.Split(k.Where, "*")
	if len(parts) == 1 {
		return k.Where == where
	}
	if !strings.HasPrefix(where, parts[0]) {
		return false
	}
	rest := where[len(parts[0]):]
	last := len(parts) - 1
	for _, part := range parts[1:last] {
		i := strings.Index(rest, part)
		if i < 0 {
			return false
		}
		rest = rest[i+len(part):]
	}
	return strings.HasSuffix(rest, parts[last])
}

// Allowed is a difference a KnownDifference allowed.
type Allowed struct {
	Difference
	Reason string
}

// Report is the outcome of comparing two Results.
type Report struct {
	Differences []Difference
	Allowed     []Allowed
}

// OK reports whether there are no differences left after the allow-list.
func (r Report) OK() bool {
	return len(r.Differences) == 0
}

// String is the readable form for a failed test.
func (r Report) String() string {
	var b strings.Builder
	if r.OK() {
		b.WriteString("no differences")
	} else {
		fmt.Fprintf(&b, "%d difference(s), node is expected, go is actual:\n", len(r.Differences))
		for _, d := range r.Differences {
			fmt.Fprintf(&b, "  %s\n    node: %s\n    go:   %s\n", d.Where, d.Node, d.Go)
		}
	}
	if len(r.Allowed) > 0 {
		fmt.Fprintf(&b, "\n%d allowed difference(s):\n", len(r.Allowed))
		for _, a := range r.Allowed {
			fmt.Fprintf(&b, "  %s (%s)\n", a.Where, a.Reason)
		}
	}
	return b.String()
}

// Compare diffs Go's result against Node's. Each known difference must have a
// reason and must match at least one difference; an entry that matches
// nothing is reported, so the allow-list cannot go stale.
func Compare(node, goRes Result, known []KnownDifference) (Report, error) {
	for _, k := range known {
		if strings.TrimSpace(k.Reason) == "" {
			return Report{}, fmt.Errorf("known difference %q has no reason", k.Where)
		}
		if k.Where == "" || k.Where == "*" {
			return Report{}, errors.New("a known difference must name a location")
		}
	}
	var c comparer
	c.steps(node.Steps, goRes.Steps)
	c.outbound(node.Outbound, goRes.Outbound)
	c.tables(node.Tables, goRes.Tables)

	var rep Report
	used := make([]bool, len(known))
	for _, d := range c.diffs {
		allowed := false
		for i, k := range known {
			if k.matches(d.Where) {
				rep.Allowed = append(rep.Allowed, Allowed{Difference: d, Reason: k.Reason})
				used[i] = true
				allowed = true
				break
			}
		}
		if !allowed {
			rep.Differences = append(rep.Differences, d)
		}
	}
	for i, k := range known {
		if !used[i] && !k.Volatile {
			rep.Differences = append(rep.Differences, Difference{
				Where: "known difference " + strconv.Quote(k.Where),
				Node:  "(matched nothing)",
				Go:    "(remove it, or fix the location)",
			})
		}
	}
	return rep, nil
}

type comparer struct {
	diffs []Difference
}

func (c *comparer) add(where, node, goVal string) {
	c.diffs = append(c.diffs, Difference{Where: where, Node: node, Go: goVal})
}

const maxShown = 400

func show(s string) string {
	if len(s) > maxShown {
		return s[:maxShown] + fmt.Sprintf("... (%d bytes)", len(s))
	}
	return s
}

func showValue(v Value) string {
	return show(v.Compact())
}

func (c *comparer) steps(node, goSteps []StepResult) {
	n := max(len(node), len(goSteps))
	for i := 0; i < n; i++ {
		if i >= len(node) || i >= len(goSteps) {
			c.add("steps", fmt.Sprintf("%d steps", len(node)), fmt.Sprintf("%d steps", len(goSteps)))
			return
		}
		ns, gs := node[i], goSteps[i]
		c.response(fmt.Sprintf("step %d %s", i+1, ns.Label), ns.Response, gs.Response)
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func (c *comparer) response(where string, node, goResp Response) {
	if node.Status != goResp.Status {
		c.add(where+": status", strconv.Itoa(node.Status), strconv.Itoa(goResp.Status))
	}
	c.headers(where, node.Headers, goResp.Headers)
	c.body(where+": body", node.Body, goResp.Body)
}

func (c *comparer) headers(where string, node, goHeaders []Header) {
	byName := func(hs []Header) map[string][]string {
		m := map[string][]string{}
		for _, h := range hs {
			m[h.Name] = h.Values
		}
		return m
	}
	nm, gm := byName(node), byName(goHeaders)
	var names []string
	seen := map[string]bool{}
	for _, hs := range [][]Header{node, goHeaders} {
		for _, h := range hs {
			if !seen[h.Name] {
				seen[h.Name] = true
				names = append(names, h.Name)
			}
		}
	}
	for _, name := range names {
		nv, gv := nm[name], gm[name]
		if !sameStrings(nv, gv) {
			c.add(where+": header "+name, showHeader(nv), showHeader(gv))
		}
	}
}

func showHeader(values []string) string {
	if len(values) == 0 {
		return "(not sent)"
	}
	return show(strings.Join(values, " | "))
}

func (c *comparer) body(where string, node, goBody Body) {
	switch {
	case node.IsJSON && goBody.IsJSON:
		c.value(where, node.JSON, goBody.JSON)
	case !node.IsJSON && !goBody.IsJSON:
		if node.Text != goBody.Text {
			c.add(where, show(strconv.Quote(node.Text)), show(strconv.Quote(goBody.Text)))
		}
	default:
		c.add(where, showBody(node), showBody(goBody))
	}
}

func showBody(b Body) string {
	if b.IsJSON {
		return "JSON " + showValue(b.JSON)
	}
	return "text " + show(strconv.Quote(b.Text))
}

var kindNames = map[Kind]string{Null: "null", Bool: "boolean", Number: "number", String: "string", Array: "array", Object: "object"}

// value walks two JSON values. Key order is part of the contract, so a
// different key sequence is reported once at the object. Then each key both
// sides have is compared occurrence by occurrence: the first "x" with the
// first "x", the second with the second, so a duplicate key's earlier value
// is compared too.
func (c *comparer) value(where string, node, goVal Value) {
	if node.Kind != goVal.Kind {
		c.add(where, kindNames[node.Kind]+" "+showValue(node), kindNames[goVal.Kind]+" "+showValue(goVal))
		return
	}
	switch node.Kind {
	case Null:
	case Bool, Number, String:
		if !Equal(node, goVal) {
			c.add(where, showValue(node), showValue(goVal))
		}
	case Array:
		if len(node.Items) != len(goVal.Items) {
			c.add(where+": length", strconv.Itoa(len(node.Items)), strconv.Itoa(len(goVal.Items)))
		}
		for i := 0; i < min(len(node.Items), len(goVal.Items)); i++ {
			c.value(where+"."+strconv.Itoa(i), node.Items[i], goVal.Items[i])
		}
	case Object:
		nk, gk := keys(node), keys(goVal)
		if !sameStrings(nk, gk) {
			c.add(where+": keys", showKeys(nk), showKeys(gk))
		}
		nOcc, gOcc := occurrences(node), occurrences(goVal)
		for _, k := range uniqueKeys(nk) {
			nvs, gvs := nOcc[k], gOcc[k]
			for j := 0; j < min(len(nvs), len(gvs)); j++ {
				w := where + "." + k
				if len(nvs) > 1 || len(gvs) > 1 {
					w += fmt.Sprintf("#%d", j+1)
				}
				c.value(w, nvs[j], gvs[j])
			}
		}
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// showKeys quotes each key, so an empty key or one with a comma reads
// unambiguously.
func showKeys(ks []string) string {
	q := make([]string, len(ks))
	for i, k := range ks {
		q[i] = strconv.Quote(k)
	}
	return show("[" + strings.Join(q, ", ") + "]")
}

// occurrences lists each key's values in document order.
func occurrences(v Value) map[string][]Value {
	out := map[string][]Value{}
	for _, m := range v.Members {
		out[m.Key] = append(out[m.Key], m.Value)
	}
	return out
}

func keys(v Value) []string {
	out := make([]string, len(v.Members))
	for i, m := range v.Members {
		out[i] = m.Key
	}
	return out
}

func uniqueKeys(ks []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range ks {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func (c *comparer) outbound(node, goOut []Outbound) {
	if len(node) != len(goOut) {
		c.add("outbound: count", strconv.Itoa(len(node))+" "+outboundList(node), strconv.Itoa(len(goOut))+" "+outboundList(goOut))
	}
	for i := 0; i < min(len(node), len(goOut)); i++ {
		n, g := node[i], goOut[i]
		where := fmt.Sprintf("outbound %d %s", i+1, n.Label())
		if n.Label() != g.Label() {
			c.add(where+": request", n.Label(), g.Label())
			continue
		}
		if !sameStrings(n.Auth, g.Auth) {
			c.add(where+": auth headers", orNone(strings.Join(n.Auth, ",")), orNone(strings.Join(g.Auth, ",")))
		}
		c.body(where+": body", n.Body, g.Body)
	}
}

func outboundList(out []Outbound) string {
	labels := make([]string, len(out))
	for i, o := range out {
		labels[i] = o.Label()
	}
	return show("[" + strings.Join(labels, "; ") + "]")
}

func (c *comparer) tables(node, goTables []Table) {
	gm := map[string]Table{}
	for _, t := range goTables {
		gm[t.Name] = t
	}
	nm := map[string]bool{}
	for _, nt := range node {
		nm[nt.Name] = true
		gt, ok := gm[nt.Name]
		if !ok {
			c.add("table "+nt.Name, "present", "missing")
			continue
		}
		where := "table " + nt.Name
		if len(nt.Rows) != len(gt.Rows) {
			c.add(where+": row count", strconv.Itoa(len(nt.Rows)), strconv.Itoa(len(gt.Rows)))
		}
		for i := 0; i < max(len(nt.Rows), len(gt.Rows)); i++ {
			rw := fmt.Sprintf("%s row %d", where, i+1)
			switch {
			case i >= len(nt.Rows):
				c.add(rw, "(no row)", showValue(gt.Rows[i]))
			case i >= len(gt.Rows):
				c.add(rw, showValue(nt.Rows[i]), "(no row)")
			default:
				c.value(rw, nt.Rows[i], gt.Rows[i])
			}
		}
	}
	for _, gt := range goTables {
		if !nm[gt.Name] {
			c.add("table "+gt.Name, "missing", "present")
		}
	}
}
