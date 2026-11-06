package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Scenario is one contract case: steps sent in order to each server. There
// are no hand-written expected values; Node's result is the expected value.
type Scenario struct {
	Name  string
	Steps []Step
	// Replies are queued on the fake upstream before the steps run.
	Replies []QueuedReply
	// OutboundMultiset compares the upstream requests without regard to
	// order, for scenarios whose server sends them in parallel.
	OutboundMultiset bool
	// Known lists the differences this case allows, each with its reason.
	Known []KnownDifference
}

// QueuedReply is a canned upstream answer for one endpoint.
type QueuedReply struct {
	Upstream string
	Path     string
	Reply    Reply
}

// Step is an HTTP request, a settle when Settle is true, a statement on the
// server's database when SQL is set, or one pass of a background job when
// Job is set. Path, header values and Body may use {{name}}, which is
// replaced by a value captured earlier on the same server or by a credential
// of that server's Target.
type Step struct {
	Label  string // defaults to "METHOD path"
	Method string
	Path   string // path and query, sent as written
	Header [][2]string
	Body   string
	// Chunked sends the body with Transfer-Encoding: chunked and no
	// Content-Length.
	Chunked bool
	// Auth names a credential of the Target; it is sent as
	// "Authorization: Bearer <token>".
	Auth string
	// Capture maps a name to a dot path in the JSON response body
	// ("session_token", "data.0.id"). The raw value, before normalization,
	// is what later steps get.
	Capture map[string]string
	// CaptureLocation maps a name to a query parameter of the Location
	// header's URL (the code in an OAuth redirect).
	CaptureLocation map[string]string
	// Random lists dot paths in the JSON response whose values are random by
	// design but not shaped like the tokens the normalizer recognizes (a
	// passphrase of words, a clock reading in seconds). Each such value is
	// renamed like a random value wherever it appears.
	Random []string
	// VolatileLength marks a response whose length differs between correct
	// runs (it carries a random passphrase). The response_length of the
	// request_log rows with this step's method, path and status is not
	// compared; every other field of those rows, and every other row, still
	// is.
	VolatileLength bool

	Settle bool
	// Until, on a settle step, waits for an explicit condition (a reply row
	// written, for example) instead of quiet.
	Until func(ctx context.Context, t *Target) (bool, error)

	// SQL runs on the server's database copy as the superuser, the same text
	// on both sides: rows or config a later job step reads. It is not
	// compared, and a failure is a harness failure.
	SQL string
	// Job runs one pass of a background job (Target.RunJob) against the
	// server's database copy. The job's log lines are compared as the
	// step's body, and its writes with the tables.
	Job string
}

// Target is one server under test.
type Target struct {
	Name    string // "node" or "go", for messages
	Side    string // the fake upstream side this server's *_API_BASE uses
	BaseURL string
	// Credentials are the fixture's plain tokens and passwords by name.
	Credentials map[string]string
	// Fingerprint returns a short description of the server's database
	// state (row counts per table) for the settle step; nil means only the
	// fake upstream is watched.
	Fingerprint func(ctx context.Context) (string, error)
	// Busy counts the server's database connections that are running a
	// statement or holding a transaction open; settle waits for zero. A
	// commit stalled on a slow disk changes no row until it lands, so the
	// fingerprint alone can read as quiet while a write is still coming.
	// nil means not checked.
	Busy func(ctx context.Context) (int, error)
	// Snapshot reads every compared table after the steps; nil means no
	// rows are compared.
	Snapshot func(ctx context.Context) ([]Table, error)
	// Exec runs an SQL step; RunJob runs a job step and returns the job's
	// log lines. nil refuses those steps.
	Exec   func(ctx context.Context, sql string) error
	RunJob func(ctx context.Context, job string) (string, error)
}

// Runner runs scenarios against pairs of servers.
type Runner struct {
	Fake *FakeUpstream
	// RequestTimeout limits one request step (default 30 s).
	RequestTimeout time.Duration
	// QuietFor is how long nothing may change before a settle ends
	// (default 300 ms); SettleLimit is how long a settle may take
	// (default 10 s).
	QuietFor    time.Duration
	SettleLimit time.Duration
}

// Outcome is the comparison of one scenario and the two normalized results
// it was made from.
type Outcome struct {
	Report Report
	Node   Result
	Go     Result
}

type rawStep struct {
	label  string
	status int
	header http.Header
	body   []byte
	random []string // raw values at the step's Random paths
	// volatileLength is "METHOD path" when the step set VolatileLength.
	volatileLength string
}

type rawSide struct {
	steps    []rawStep
	outbound []Recorded
	tables   []Table
}

// Run sends the scenario to both servers at once, each on its own, then
// normalizes and compares. The error is for harness failures (a settle that
// timed out, a snapshot that failed); a difference between the servers is in
// the Report.
func (r *Runner) Run(ctx context.Context, sc Scenario, node, goTarget *Target) (Outcome, error) {
	for _, q := range sc.Replies {
		r.Fake.Queue(q.Upstream, q.Path, q.Reply)
	}

	var wg sync.WaitGroup
	sides := [2]rawSide{}
	errs := [2]error{}
	for i, t := range []*Target{node, goTarget} {
		wg.Add(1)
		go func(i int, t *Target) {
			defer wg.Done()
			sides[i], errs[i] = r.runSide(ctx, sc, t)
		}(i, t)
	}
	wg.Wait()
	if err := errors.Join(errs[0], errs[1]); err != nil {
		return Outcome{}, fmt.Errorf("scenario %s: %w", sc.Name, err)
	}

	out := Outcome{
		Node: normalizeSide(sides[0], node, sc.OutboundMultiset),
		Go:   normalizeSide(sides[1], goTarget, sc.OutboundMultiset),
	}
	rep, err := Compare(out.Node, out.Go, sc.Known)
	if err != nil {
		return Outcome{}, fmt.Errorf("scenario %s: %w", sc.Name, err)
	}
	out.Report = rep
	return out, nil
}

func (r *Runner) runSide(ctx context.Context, sc Scenario, t *Target) (rawSide, error) {
	var side rawSide
	vars := map[string]string{}
	for k, v := range t.Credentials {
		vars[k] = v
	}
	client := &http.Client{
		Timeout: r.requestTimeout(),
		// Bodies are compared as sent; Go would otherwise ask for gzip and
		// unpack it out of sight.
		Transport: &http.Transport{DisableCompression: true, Proxy: nil},
		// A redirect is a response to compare, not to follow.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()

	for i, st := range sc.Steps {
		if st.Settle {
			if err := r.settle(ctx, t, st); err != nil {
				return side, fmt.Errorf("%s: step %d settle: %w", t.Name, i+1, err)
			}
			continue
		}
		if st.SQL != "" {
			if t.Exec == nil {
				return side, fmt.Errorf("%s: step %d: target cannot run SQL", t.Name, i+1)
			}
			if err := t.Exec(ctx, st.SQL); err != nil {
				return side, fmt.Errorf("%s: step %d SQL: %w", t.Name, i+1, err)
			}
			continue
		}
		if st.Job != "" {
			if t.RunJob == nil {
				return side, fmt.Errorf("%s: step %d: target cannot run jobs", t.Name, i+1)
			}
			out, err := t.RunJob(ctx, st.Job)
			if err != nil {
				return side, fmt.Errorf("%s: step %d job %s: %w", t.Name, i+1, st.Job, err)
			}
			side.steps = append(side.steps, rawStep{label: "job " + st.Job, header: http.Header{}, body: []byte(out)})
			continue
		}
		step, err := r.request(ctx, client, t, st, vars)
		if err != nil {
			return side, fmt.Errorf("%s: step %d %s: %w", t.Name, i+1, stepLabel(st), err)
		}
		side.steps = append(side.steps, step)
	}

	// The last request may have started background work (request_log
	// writes, a provider call): wait for quiet before reading results.
	if err := r.settle(ctx, t, Step{}); err != nil {
		return side, fmt.Errorf("%s: final settle: %w", t.Name, err)
	}
	side.outbound = r.Fake.Requests(t.Side)
	if t.Snapshot != nil {
		tables, err := t.Snapshot(ctx)
		if err != nil {
			return side, fmt.Errorf("%s: snapshot: %w", t.Name, err)
		}
		side.tables = tables
	}
	return side, nil
}

func (r *Runner) requestTimeout() time.Duration {
	if r.RequestTimeout > 0 {
		return r.RequestTimeout
	}
	return 30 * time.Second
}

var varRe = regexp.MustCompile(`\{\{([A-Za-z0-9_.-]+)\}\}`)

// expand replaces {{name}}; a name with no value is an error, since sending
// the literal would hide a broken capture.
func expand(s string, vars map[string]string) (string, error) {
	var missing []string
	out := varRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-2]
		v, ok := vars[name]
		if !ok {
			missing = append(missing, name)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("no value for {{%s}}", strings.Join(missing, "}}, {{"))
	}
	return out, nil
}

func stepLabel(st Step) string {
	if st.Label != "" {
		return st.Label
	}
	return st.Method + " " + st.Path
}

// request sends one step. Every error is a harness failure, not a result to
// compare: two servers failing the same way (a broken {{capture}}, a timeout
// on both) would otherwise compare equal and pass without testing anything.
func (r *Runner) request(ctx context.Context, client *http.Client, t *Target, st Step, vars map[string]string) (rawStep, error) {
	res := rawStep{label: stepLabel(st)}
	if st.VolatileLength {
		res.volatileLength = st.Method + " " + st.Path
	}
	fail := func(err error) (rawStep, error) {
		return res, err
	}

	path, err := expand(st.Path, vars)
	if err != nil {
		return fail(err)
	}
	body, err := expand(st.Body, vars)
	if err != nil {
		return fail(err)
	}
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequestWithContext(ctx, st.Method, t.BaseURL+"/", reader)
	if err != nil {
		return fail(err)
	}
	// Opaque is sent as the request target unchanged, so a case can send a
	// path that needs escaping exactly as a client would. Go writes an
	// Opaque that starts with "//" as an absolute URL ("http://a//b"), so
	// such a path goes through ParseRequestURI instead, which keeps it a
	// path; its RawPath keeps the escapes as written.
	if strings.HasPrefix(path, "//") {
		u, err := url.ParseRequestURI(path)
		if err != nil {
			return fail(err)
		}
		req.URL.Path, req.URL.RawPath, req.URL.RawQuery = u.Path, u.RawPath, u.RawQuery
	} else {
		req.URL.Opaque = path
	}
	if st.Chunked {
		req.ContentLength = -1
		req.TransferEncoding = []string{"chunked"}
		req.Body = io.NopCloser(bytes.NewReader([]byte(body)))
	}
	for _, h := range st.Header {
		v, err := expand(h[1], vars)
		if err != nil {
			return fail(err)
		}
		req.Header.Add(h[0], v)
	}
	if st.Auth != "" {
		token, ok := t.Credentials[st.Auth]
		if !ok {
			return fail(fmt.Errorf("no credential %q", st.Auth))
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fail(fmt.Errorf("reading the response: %w", err))
	}
	res.status = resp.StatusCode
	res.header = resp.Header
	res.body = data

	// A capture is required: a missing one would leave a later step with no
	// value, or with a stale one from an earlier capture of the same name.
	if len(st.Capture) > 0 {
		v, err := ParseJSON(data)
		if err != nil || !isJSON(resp.Header.Get("Content-Type")) {
			return fail(fmt.Errorf("capture: status %d, response is not JSON", resp.StatusCode))
		}
		for name, p := range st.Capture {
			got, ok := v.Path(p)
			if !ok || got.Kind == Null {
				return fail(fmt.Errorf("capture %s: no value at %q (status %d)", name, p, resp.StatusCode))
			}
			vars[name] = got.Text()
		}
	}
	if len(st.CaptureLocation) > 0 {
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			return fail(fmt.Errorf("capture: Location: %w", err))
		}
		for name, param := range st.CaptureLocation {
			got := loc.Query().Get(param)
			if got == "" {
				return fail(fmt.Errorf("capture %s: no %q in Location %q (status %d)", name, param, resp.Header.Get("Location"), resp.StatusCode))
			}
			vars[name] = got
		}
	}
	if len(st.Random) > 0 {
		v, err := ParseJSON(data)
		if err != nil {
			return fail(fmt.Errorf("random: status %d, response is not JSON", resp.StatusCode))
		}
		for _, p := range st.Random {
			got, ok := v.Path(p)
			if !ok || got.Kind == Null {
				return fail(fmt.Errorf("random: no value at %q (status %d)", p, resp.StatusCode))
			}
			res.random = append(res.random, got.Text())
		}
	}
	return res, nil
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// settle waits until the server is quiet: no fake upstream request in
// flight, no database connection busy, and neither the fake's last activity
// nor the database fingerprint changing for QuietFor. With Until it waits for
// that condition instead.
func (r *Runner) settle(ctx context.Context, t *Target, st Step) error {
	quietFor := r.QuietFor
	if quietFor <= 0 {
		quietFor = 300 * time.Millisecond
	}
	limit := r.SettleLimit
	if limit <= 0 {
		limit = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()

	if st.Until != nil {
		for {
			ok, err := st.Until(ctx, t)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("condition not met within %s", limit)
			case <-tick.C:
			}
		}
	}

	var lastState string
	var stableSince time.Time
	for {
		inFlight, lastActivity := r.Fake.Activity(t.Side)
		fp := ""
		if t.Fingerprint != nil {
			var err error
			fp, err = t.Fingerprint(ctx)
			if err != nil && ctx.Err() == nil {
				return err
			}
		}
		busy := 0
		if t.Busy != nil {
			var err error
			busy, err = t.Busy(ctx)
			if err != nil && ctx.Err() == nil {
				return err
			}
		}
		state := fmt.Sprintf("upstream requests in flight %d, last upstream activity %s, database connections busy %d, database %s",
			inFlight, lastActivity.Format(time.RFC3339Nano), busy, fp)
		now := time.Now()
		if state != lastState || inFlight > 0 || busy > 0 {
			lastState = state
			stableSince = now
		} else if now.Sub(stableSince) >= quietFor {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("still changing after %s: %s", limit, lastState)
		case <-tick.C:
		}
	}
}

// normalizeSide turns one server's raw results into a Result. The order is
// fixed (responses, then upstream requests, then tables) so both sides name
// their random values in the same sequence.
func normalizeSide(side rawSide, t *Target, outboundMultiset bool) Result {
	n := NewNormalizer(t.BaseURL)
	for _, s := range side.steps {
		for _, raw := range s.random {
			n.AddRandom(raw)
		}
	}
	var res Result
	for _, s := range side.steps {
		res.Steps = append(res.Steps, StepResult{
			Label: s.label,
			Response: Response{
				Status:  s.status,
				Headers: compareHeaders(n, s.header),
				Body:    makeBody(n, s.body, isJSON(s.header.Get("Content-Type"))),
			},
		})
	}

	outbound := side.outbound
	if outboundMultiset {
		outbound = sortRecorded(n, outbound)
	}
	for _, o := range outbound {
		res.Outbound = append(res.Outbound, Outbound{
			Upstream: o.Upstream,
			Method:   o.Method,
			Path:     n.String(o.Path),
			Auth:     o.Auth,
			Body:     makeBody(n, o.Body, true),
		})
	}

	volatile := map[string]bool{}
	for _, s := range side.steps {
		if s.volatileLength != "" {
			volatile[s.volatileLength+" "+strconv.Itoa(s.status)] = true
		}
	}
	for _, tbl := range side.tables {
		rows := tbl.Rows
		if tbl.Name == "request_log" && len(volatile) > 0 {
			rows = maskVolatileLengths(rows, volatile)
		}
		if tbl.Multiset {
			rows = n.SortMultiset(rows)
		}
		out := Table{Name: tbl.Name, Multiset: tbl.Multiset, Rows: make([]Value, len(rows))}
		for i, row := range rows {
			out.Rows[i] = n.Value(row)
		}
		res.Tables = append(res.Tables, out)
	}
	return res
}

// maskVolatileLengths replaces response_length with a fixed text in the
// request_log rows of the VolatileLength steps (matched on method, path and
// status, so a later request to the same path with another outcome is still
// compared).
func maskVolatileLengths(rows []Value, volatile map[string]bool) []Value {
	out := make([]Value, len(rows))
	for i, row := range rows {
		out[i] = row
		method, _ := row.Get("method")
		path, _ := row.Get("path")
		status, _ := row.Get("status")
		if row.Kind != Object || !volatile[method.Str+" "+path.Str+" "+status.Num] {
			continue
		}
		members := make([]Member, len(row.Members))
		copy(members, row.Members)
		for j := range members {
			if members[j].Key == "response_length" {
				members[j].Value = Str("<volatile-length>")
			}
		}
		out[i] = Value{Kind: Object, Members: members}
	}
	return out
}

func compareHeaders(n *Normalizer, h http.Header) []Header {
	var out []Header
	for _, name := range ComparedHeaders {
		values := h.Values(name)
		if len(values) == 0 {
			continue
		}
		hdr := Header{Name: name}
		for _, v := range values {
			if name == "set-cookie" {
				v, _, _ = strings.Cut(v, "=")
				v = strings.TrimSpace(v)
			}
			hdr.Values = append(hdr.Values, n.String(v))
		}
		out = append(out, hdr)
	}
	return out
}

// makeBody parses a JSON body when tryJSON is set and it parses; anything
// else is compared as normalized text.
func makeBody(n *Normalizer, data []byte, tryJSON bool) Body {
	if tryJSON && len(data) > 0 {
		if v, err := ParseJSON(data); err == nil {
			return Body{IsJSON: true, JSON: n.Value(v)}
		}
	}
	return Body{Text: n.String(string(data))}
}

// sortRecorded orders upstream requests by every compared field in masked
// normalized form, so parallel calls compare the same way whatever order
// they arrived in. The key is JSON, so no field can run into the next.
func sortRecorded(n *Normalizer, recs []Recorded) []Recorded {
	m := n.masking()
	keys := make([]string, len(recs))
	for i, rec := range recs {
		b := makeBody(m, rec.Body, true)
		body := "text:" + b.Text
		if b.IsJSON {
			body = "json:" + b.JSON.Compact()
		}
		key, _ := json.Marshal([]any{rec.Upstream, rec.Method, m.String(rec.Path), rec.Auth, body})
		keys[i] = string(key)
	}
	idx := make([]int, len(recs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return keys[idx[a]] < keys[idx[b]] })
	out := make([]Recorded, len(recs))
	for i, j := range idx {
		out[i] = recs[j]
	}
	return out
}
