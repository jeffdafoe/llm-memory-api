package staleness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// goldens is testdata/goldens.json, written by testdata/goldens.js from
// Node's staleness.js and staleness-certificates.js. Regenerate it with that
// script when the Node side changes.
type goldens struct {
	Platform string `json:"platform"`
	Apt      []struct {
		In  string          `json:"in"`
		Out json.RawMessage `json:"out"`
	} `json:"apt"`
	EOL []struct {
		Label string      `json:"label"`
		Cycle string      `json:"cycle"`
		EOL   *string     `json:"eol"`
		Today string      `json:"today"`
		Out   outcomeJSON `json:"out"`
	} `json:"eol"`
	Cert []struct {
		From string      `json:"from"`
		To   string      `json:"to"`
		Now  string      `json:"now"`
		Out  outcomeJSON `json:"out"`
	} `json:"cert"`
	Kernels []struct {
		In  string          `json:"in"`
		Out json.RawMessage `json:"out"`
	} `json:"kernels"`
	Compare []struct {
		A    []string `json:"a"`
		B    []string `json:"b"`
		Sign int      `json:"sign"`
	} `json:"compare"`
	Reboot []struct {
		In  RebootInput     `json:"in"`
		Out json.RawMessage `json:"out"`
	} `json:"reboot"`
	Report []struct {
		In  ReportInput     `json:"in"`
		Out json.RawMessage `json:"out"`
	} `json:"report"`
	PEM          string `json:"pem"`
	Certificates []struct {
		Name              string          `json:"name"`
		Renewal           json.RawMessage `json:"renewal"`
		Live              json.RawMessage `json:"live"`
		Now               string          `json:"now"`
		PlatformDependent bool            `json:"platformDependent"`
		Out               Certificates    `json:"out"`
	} `json:"certificates"`
}

type outcomeJSON struct {
	Value json.RawMessage `json:"value"`
	Error string          `json:"error"`
}

func loadGoldens(t *testing.T) goldens {
	t.Helper()
	data, err := os.ReadFile("testdata/goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldens
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// sameJSON compares a Go value with Node's JSON as decoded values, so key
// order and number spelling do not matter.
func sameJSON(t *testing.T, got any, want json.RawMessage) bool {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal(g, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(a, b)
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseAptSimulationMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Apt {
		if got := ParseAptSimulation(c.In); !sameJSON(t, got, c.Out) {
			b, _ := json.Marshal(got)
			t.Errorf("ParseAptSimulation(%q)\n got %s\nwant %s", c.In, b, c.Out)
		}
	}
}

func TestAssessEOLMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).EOL {
		var raw []byte
		if c.EOL != nil {
			raw = []byte(*c.EOL)
		}
		got, err := AssessEOL(c.Label, c.Cycle, raw, mustTime(t, c.Today))
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if msg != c.Out.Error || (err == nil && !sameJSON(t, got, c.Out.Value)) {
			b, _ := json.Marshal(got)
			t.Errorf("AssessEOL(%v) at %s: %s %q, Node %s %q", c.EOL, c.Today, b, msg, c.Out.Value, c.Out.Error)
		}
	}
}

func TestAssessCertificateMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Cert {
		got, err := AssessCertificate("example.org", mustTime(t, c.From), mustTime(t, c.To), mustTime(t, c.Now))
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if msg != c.Out.Error || (err == nil && !sameJSON(t, got, c.Out.Value)) {
			b, _ := json.Marshal(got)
			t.Errorf("AssessCertificate(%s, %s, %s): %s %q, Node %s %q", c.From, c.To, c.Now, b, msg, c.Out.Value, c.Out.Error)
		}
	}
}

func TestKernelsMatchNode(t *testing.T) {
	g := loadGoldens(t)
	for _, c := range g.Kernels {
		if got := ParseKernelRelease(c.In); !sameJSON(t, got, c.Out) {
			b, _ := json.Marshal(got)
			t.Errorf("ParseKernelRelease(%q) = %s, Node %s", c.In, b, c.Out)
		}
	}
	sign := func(n int) int {
		switch {
		case n < 0:
			return -1
		case n > 0:
			return 1
		}
		return 0
	}
	for _, c := range g.Compare {
		if got := sign(CompareKernelVersions(c.A, c.B)); got != c.Sign {
			t.Errorf("CompareKernelVersions(%v, %v) sign %d, Node %d", c.A, c.B, got, c.Sign)
		}
	}
	for _, c := range g.Reboot {
		if got := AssessReboot(c.In); !sameJSON(t, got, c.Out) {
			b, _ := json.Marshal(got)
			t.Errorf("AssessReboot(%+v)\n got %s\nwant %s", c.In, b, c.Out)
		}
	}
}

func TestBuildReportMatchesNode(t *testing.T) {
	for i, c := range loadGoldens(t).Report {
		got := BuildReport(c.In)
		if !sameJSON(t, got, c.Out) {
			b, _ := json.Marshal(got)
			t.Errorf("report %d\n got %s\nwant %s", i, b, c.Out)
		}
	}
}

// errnoShaped is a failure whose Node text came from an fs error.
var errnoShaped = regexp.MustCompile("`: E[A-Z]+: |: E[A-Z]+: ")

// TestCollectCertificatesMatchesNode builds each tree Node's golden ran
// against and compares the result. Two kinds of failure text are compared
// only up to the reason: a certificate OpenSSL could not read (Go's x509
// gives its own reason), and an fs error the platforms report differently
// (the Windows codes the Go side cannot map, and the case flagged
// platform-dependent when the goldens come from another OS).
func TestCollectCertificatesMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	for _, c := range g.Certificates {
		root := t.TempDir()
		buildTree(t, root, c.Renewal, c.Live, g.PEM)
		got := CollectCertificates(root, mustTime(t, c.Now))
		for i, f := range got.Failures {
			got.Failures[i] = strings.ReplaceAll(strings.ReplaceAll(f, root, "<root>"), `\`, "/")
		}
		if got.Checked != c.Out.Checked || !reflect.DeepEqual(got.Findings, c.Out.Findings) {
			t.Errorf("%s: checked %d findings %+v, Node %d %+v", c.Name, got.Checked, got.Findings, c.Out.Checked, c.Out.Findings)
		}
		if len(got.Failures) != len(c.Out.Failures) {
			t.Errorf("%s: failures %q, Node %q", c.Name, got.Failures, c.Out.Failures)
			continue
		}
		for i, want := range c.Out.Failures {
			gotF := got.Failures[i]
			if gotF == want {
				continue
			}
			loose := !errnoShaped.MatchString(want) ||
				(runtime.GOOS == "windows" && !strings.Contains(want, "ENOENT")) ||
				(c.PlatformDependent && g.Platform != runtime.GOOS && !(g.Platform == "win32" && runtime.GOOS == "windows"))
			if loose && reasonPrefix(gotF) == reasonPrefix(want) {
				t.Logf("%s: failure %d compared up to its reason:\n  go:   %s\n  node: %s", c.Name, i, gotF, want)
				continue
			}
			t.Errorf("%s: failure %d\n got %s\nwant %s", c.Name, i, gotF, want)
		}
	}
}

// reasonPrefix is a failure line up to its reason ("...`name`: ").
func reasonPrefix(s string) string {
	if i := strings.Index(s, "`: "); i >= 0 {
		return s[:i+3]
	}
	if i := strings.Index(s, ": "); i >= 0 {
		return s[:i+2]
	}
	return s
}

func buildTree(t *testing.T, root string, renewal, live json.RawMessage, pem string) {
	t.Helper()
	write := func(path, data string) {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir := func(path string) {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	switch s := string(renewal); {
	case s == `"file"`:
		write(filepath.Join(root, "renewal"), "")
	case s != "null":
		var names []string
		if err := json.Unmarshal(renewal, &names); err != nil {
			t.Fatal(err)
		}
		mkdir(filepath.Join(root, "renewal"))
		for _, n := range names {
			write(filepath.Join(root, "renewal", n+".conf"), "")
		}
		write(filepath.Join(root, "renewal", "notes.txt"), "")
		mkdir(filepath.Join(root, "renewal", "dir.conf"))
	}
	switch s := string(live); {
	case s == `"file"`:
		write(filepath.Join(root, "live"), "")
	case s != "null":
		var kinds map[string]string
		if err := json.Unmarshal(live, &kinds); err != nil {
			t.Fatal(err)
		}
		mkdir(filepath.Join(root, "live"))
		write(filepath.Join(root, "live", "README"), "")
		for n, kind := range kinds {
			dir := filepath.Join(root, "live", n)
			mkdir(dir)
			switch kind {
			case "cert":
				write(filepath.Join(dir, "cert.pem"), pem)
			case "bad":
				write(filepath.Join(dir, "cert.pem"), "not a certificate")
			case "dir":
				mkdir(filepath.Join(dir, "cert.pem"))
			}
		}
	}
}
