package mcp

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

type goldens struct {
	Tools       string `json:"tools"`
	Permissions string `json:"permissions"`
	Validation  []struct {
		Tool   string  `json:"tool"`
		Args   *string `json:"args"`
		OK     bool    `json:"ok"`
		Status int     `json:"status"`
		Error  string  `json:"error"`
	} `json:"validation"`
	ContentTypes []struct {
		Header string `json:"header"`
		JSON   bool   `json:"json"`
	} `json:"contentTypes"`
}

func loadGoldens(t *testing.T) goldens {
	t.Helper()
	raw, err := os.ReadFile("testdata/goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldens
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestToolListMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	list := make([]any, len(toolDefs))
	for i, d := range toolDefs {
		list[i] = d.def
	}
	if got := jsutil.Marshal(list); got != g.Tools {
		t.Fatalf("tools.json differs from Node's TOOLS:\n got %.300s\nwant %.300s", got, g.Tools)
	}
}

func TestPermissionMapMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	v, err := jsutil.Parse([]byte(g.Permissions))
	if err != nil {
		t.Fatal(err)
	}
	node := v.(*jsutil.Object)
	if node.Len() != len(toolPermissions) {
		t.Fatalf("Node has %d permissions, Go %d", node.Len(), len(toolPermissions))
	}
	for _, name := range node.Keys() {
		if toolPermissions[name] != node.Value(name) {
			t.Errorf("%s: Go %q, Node %v", name, toolPermissions[name], node.Value(name))
		}
	}
	// Every tool has a permission, and no permission names a missing tool.
	for _, d := range toolDefs {
		if _, ok := toolPermissions[d.name]; !ok {
			t.Errorf("tool %s has no permission", d.name)
		}
	}
	if len(toolDefs) != len(toolPermissions) {
		t.Errorf("%d tools, %d permissions", len(toolDefs), len(toolPermissions))
	}
}

func TestValidateToolArgsMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	if len(g.Validation) == 0 {
		t.Fatal("no validation cases")
	}
	for _, c := range g.Validation {
		args := jsutil.Undefined
		label := "undefined"
		if c.Args != nil {
			label = *c.Args
			v, err := jsutil.Parse([]byte(*c.Args))
			if err != nil {
				t.Fatalf("%s %s: %v", c.Tool, label, err)
			}
			args = v
		}
		if len(label) > 120 {
			label = label[:120] + "…"
		}
		_, err := validateToolArgs(c.Tool, args)
		if c.OK {
			if err != nil {
				t.Errorf("%s %s: Go refused (%v), Node accepted", c.Tool, label, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s %s: Go accepted, Node refused: %s", c.Tool, label, c.Error)
			continue
		}
		var apiErr *apierr.Error
		if !errors.As(err, &apiErr) || apiErr.Status != c.Status {
			t.Errorf("%s %s: status %v, Node %d", c.Tool, label, err, c.Status)
		}
		if err.Error() != c.Error {
			t.Errorf("%s %s:\n  Go   %s\n  Node %s", c.Tool, label, err.Error(), c.Error)
		}
	}
}

func TestToolPermissionLookup(t *testing.T) {
	p, ok := toolPermission("read_note")
	if !ok || p.text != "mcp_read_note" || !p.held([]string{"mcp_search", "mcp_read_note"}) || p.held(nil) {
		t.Fatalf("read_note: %+v %v", p, ok)
	}
	if _, ok := toolPermission("no_such_tool"); ok {
		t.Fatal("unknown tool has a permission")
	}
	p, ok = toolPermission("constructor")
	if !ok || !p.inherited || p.held([]string{p.text}) {
		t.Fatalf("constructor: %+v %v", p, ok)
	}
	if p.text != "function Object() { [native code] }" {
		t.Fatalf("constructor text %q", p.text)
	}
}

func TestIsJSONContentTypeMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	if len(g.ContentTypes) == 0 {
		t.Fatal("no content-type cases")
	}
	for _, c := range g.ContentTypes {
		// Node holds a header as one character per byte.
		var raw []byte
		for _, r := range c.Header {
			raw = append(raw, byte(r))
		}
		if got := isJSONContentType(string(raw)); got != c.JSON {
			t.Errorf("%q: Go %v, Node %v", c.Header, got, c.JSON)
		}
	}
}
