//go:build contract

package contract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Run with: go test -tags contract ./contract/ from go/server, with
// CONTRACT_PG_URL set to a superuser URL of a Postgres 17 + pgvector server
// and npm ci done in node/api. CONTRACT_GO_BIN uses a prebuilt Go server;
// CONTRACT_OUT writes each scenario's normalized results there;
// CONTRACT_PARALLEL sets how many scenarios run at once (default 4).
var harness struct {
	cluster  *Cluster
	repoRoot string
	goBin    string
}

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	ctx := context.Background()
	pgURL := os.Getenv("CONTRACT_PG_URL")
	if pgURL == "" {
		fmt.Fprintln(os.Stderr, "contract: CONTRACT_PG_URL is not set (a superuser URL, e.g. postgresql://postgres:postgres@127.0.0.1:5432/postgres)")
		return 1
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		fmt.Fprintln(os.Stderr, "contract:", err)
		return 1
	}
	harness.repoRoot = root
	if _, err := os.Stat(filepath.Join(root, "node", "api", "node_modules")); err != nil {
		fmt.Fprintln(os.Stderr, "contract: node/api/node_modules is missing; run npm ci --ignore-scripts in node/api")
		return 1
	}

	tmp, err := os.MkdirTemp("", "contract-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "contract:", err)
		return 1
	}
	defer os.RemoveAll(tmp)
	harness.goBin = os.Getenv("CONTRACT_GO_BIN")
	if harness.goBin == "" {
		bin, err := BuildGoServer(filepath.Join(root, "go", "server"), tmp)
		if err != nil {
			fmt.Fprintln(os.Stderr, "contract:", err)
			return 1
		}
		harness.goBin = bin
	}

	cluster, err := NewCluster(pgURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "contract:", err)
		return 1
	}
	harness.cluster = cluster
	started := time.Now()
	buildErr := cluster.BuildTemplate(ctx, root)
	if buildErr != nil {
		fmt.Fprintln(os.Stderr, "contract: template:", buildErr)
	} else {
		fmt.Fprintf(os.Stderr, "contract: template %s built in %s\n", cluster.template, time.Since(started).Round(time.Millisecond))
	}
	code := 1
	if buildErr == nil {
		code = m.Run()
	}
	if err := cluster.Cleanup(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "contract: cleanup:", err)
	}
	return code
}

func TestContract(t *testing.T) {
	parallel := 4
	if v, err := strconv.Atoi(os.Getenv("CONTRACT_PARALLEL")); err == nil && v > 0 {
		parallel = v
	}
	sem := make(chan struct{}, parallel)
	seen := map[string]bool{}
	for _, sc := range Scenarios {
		if seen[sc.Name] {
			t.Fatalf("two scenarios are named %q", sc.Name)
		}
		seen[sc.Name] = true
		t.Run(sc.Name, func(t *testing.T) {
			t.Parallel()
			sem <- struct{}{}
			defer func() { <-sem }()
			runScenario(t, sc)
		})
	}
}

// runScenario gives one scenario its own fake upstream, two copies of the
// template and a Node and a Go server, then runs and compares.
func runScenario(t *testing.T, sc Scenario) {
	ctx := context.Background()
	// Cleanups run last-registered first: the servers stop, then the
	// databases are dropped, then the fake closes. Closing the fake first
	// would wait on requests from servers that are still running.
	fake := NewFakeUpstream()
	t.Cleanup(fake.Close)

	type side struct {
		name string
		db   *ScenarioDB
		proc *ServerProcess
		err  error
	}
	sides := []*side{{name: "node"}, {name: "go"}}
	for _, s := range sides {
		db, err := harness.cluster.Copy(ctx, s.name)
		if err != nil {
			t.Fatal(err)
		}
		s.db = db
		t.Cleanup(func() {
			if err := db.Drop(context.Background()); err != nil {
				t.Logf("drop %s: %v", db.Name, err)
			}
		})
	}

	logRoot := t.TempDir()
	var wg sync.WaitGroup
	for _, s := range sides {
		spec := ServerSpec{
			Name:        s.name,
			Side:        s.name,
			DatabaseURL: harness.cluster.URL(s.db.Name, true),
			LogDir:      filepath.Join(logRoot, s.name),
		}
		if s.name == "node" {
			spec.Command = []string{"node", "src/server.js"}
			spec.Dir = filepath.Join(harness.repoRoot, "node", "api")
		} else {
			spec.Command = []string{harness.goBin}
			spec.Dir = logRoot
		}
		wg.Add(1)
		go func(s *side, spec ServerSpec) {
			defer wg.Done()
			s.proc, s.err = StartServer(ctx, spec, fake)
		}(s, spec)
	}
	wg.Wait()
	for _, s := range sides {
		if s.proc != nil {
			t.Cleanup(s.proc.Stop)
		}
	}
	for _, s := range sides {
		if s.err != nil {
			t.Fatal(s.err)
		}
	}

	target := func(s *side) *Target {
		return &Target{
			Name:        s.name,
			Side:        s.name,
			BaseURL:     s.proc.BaseURL,
			Credentials: Credentials(),
			Fingerprint: s.db.Fingerprint,
			Busy:        s.db.Busy,
			Snapshot:    s.db.Snapshot,
		}
	}
	r := &Runner{Fake: fake}
	out, err := r.Run(ctx, sc, target(sides[0]), target(sides[1]))
	if err != nil {
		t.Fatalf("%v\n--- node output ---\n%s\n--- go output ---\n%s", err, tail(sides[0].proc.Output(), 3000), tail(sides[1].proc.Output(), 3000))
	}
	writeOutcome(t, sc.Name, out)
	if !out.Report.OK() {
		t.Error(out.Report.String())
	} else if len(out.Report.Allowed) > 0 {
		t.Log(out.Report.String())
	}
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// writeOutcome saves the normalized results when CONTRACT_OUT is set, for
// reading a failure side by side.
func writeOutcome(t *testing.T, name string, out Outcome) {
	dir := os.Getenv("CONTRACT_OUT")
	if dir == "" {
		return
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Logf("CONTRACT_OUT: %v", err)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err == nil {
		os.WriteFile(filepath.Join(dir, unsafeFileChars.ReplaceAllString(name, "_")+".json"), data, 0o644)
	}
}
