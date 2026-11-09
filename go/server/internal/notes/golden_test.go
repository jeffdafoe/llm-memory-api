package notes

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// goldens is testdata/goldens.json, recorded from the
// Node server's own functions. Regenerate it with that script when the Node
// side changes.
type goldens struct {
	TitleToSlug []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	} `json:"title_to_slug"`
	ValidateSlug       []slugCase `json:"validate_slug"`
	ValidateSlugPrefix []slugCase `json:"validate_slug_prefix"`
	SlugToKind         []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	} `json:"slug_to_kind"`
	Paginate []struct {
		Offset json.RawMessage `json:"offset"`
		Limit  json.RawMessage `json:"limit"`
		Out    struct {
			Text       string `json:"text"`
			TotalLines int    `json:"totalLines"`
			Paginated  bool   `json:"paginated"`
			Error      string `json:"error"`
			Status     int    `json:"status"`
		} `json:"out"`
	} `json:"paginate"`
	PaginateContent string `json:"paginate_content"`
	Stringify       []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	} `json:"stringify"`
	ToFixed []struct {
		In  float64 `json:"in"`
		Out string  `json:"out"`
	} `json:"to_fixed"`
}

type slugCase struct {
	In  string `json:"in"`
	Out struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Status int    `json:"status"`
	} `json:"out"`
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

func errorOf(err error) (string, int) {
	if err == nil {
		return "", 0
	}
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return ae.Message, ae.Status
	}
	return err.Error(), 500
}

func TestTitleToSlugMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).TitleToSlug {
		if got := TitleToSlug(c.In); got != c.Out {
			t.Errorf("TitleToSlug(%q) = %q, Node gives %q", c.In, got, c.Out)
		}
	}
}

func checkSlugs(t *testing.T, cases []slugCase, allowTrailing bool) {
	t.Helper()
	for _, c := range cases {
		msg, status := errorOf(ValidateSlug(c.In, false, allowTrailing))
		if msg != c.Out.Error || (msg != "" && status != c.Out.Status) {
			t.Errorf("ValidateSlug(%q, %v) = %q %d, Node gives %q %d", c.In, allowTrailing, msg, status, c.Out.Error, c.Out.Status)
		}
	}
}

func TestValidateSlugMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	checkSlugs(t, g.ValidateSlug, false)
	checkSlugs(t, g.ValidateSlugPrefix, true)
	if msg, _ := errorOf(ValidateSlug("x", true, false)); msg != "Slug is required" {
		t.Errorf("a slug of another JSON type: %q", msg)
	}
}

func TestSlugToKindMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).SlugToKind {
		if got := SlugToKind(c.In); got != c.Out {
			t.Errorf("SlugToKind(%q) = %q, Node gives %q", c.In, got, c.Out)
		}
	}
}

func TestPaginateContentMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	// The generator writes "undefined" for an argument it did not pass.
	arg := func(raw json.RawMessage) json.RawMessage {
		if string(raw) == `"undefined"` {
			return nil
		}
		return raw
	}
	for _, c := range g.Paginate {
		name := fmt.Sprintf("offset=%s limit=%s", c.Offset, c.Limit)
		page, err := PaginateContent(g.PaginateContent, arg(c.Offset), arg(c.Limit))
		msg, status := errorOf(err)
		if c.Out.Error != "" {
			if msg != c.Out.Error || status != c.Out.Status {
				t.Errorf("%s: error %q %d, Node gives %q %d", name, msg, status, c.Out.Error, c.Out.Status)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !c.Out.Paginated {
			if page != nil {
				t.Errorf("%s: paginated, Node returns the content as is", name)
			}
			continue
		}
		if page == nil || page.Text != c.Out.Text || page.TotalLines != c.Out.TotalLines {
			t.Errorf("%s: %+v, Node gives %q %d", name, page, c.Out.Text, c.Out.TotalLines)
		}
	}
}

func TestStringifyMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Stringify {
		got, err := jsutil.Stringify([]byte(c.In))
		if err != nil || got != c.Out {
			t.Errorf("Stringify(%s) = %s (%v), Node gives %s", c.In, got, err, c.Out)
		}
	}
}

func TestToFixedMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).ToFixed {
		if got := jsutil.ToFixed(c.In, 1); got != c.Out {
			t.Errorf("(%v).toFixed(1) = %s, Node gives %s", c.In, got, c.Out)
		}
	}
}

func TestCleanExtension(t *testing.T) {
	cases := map[string]string{"md": ".md", ".MD": ".md", "..md": "", ".m-d": ".md", ".": "", "x": ".x",
		".abcdefghijklmnopqrstu": "", ".abcdefghijklmnopqrs": ".abcdefghijklmnopqrs", "": ""}
	for in, want := range cases {
		got := ""
		if p := cleanExtension(in); p != nil {
			got = *p
		}
		if got != want {
			t.Errorf("cleanExtension(%q) = %q, want %q", in, got, want)
		}
	}
}
