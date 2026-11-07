package notes

import "testing"

func TestEnrichMetadata(t *testing.T) {
	cases := []struct {
		raw, want string
		fails     bool
	}{
		{`{"b":1,"a":2}`, `{"b":1,"a":2}`, false},
		{``, ``, false},
		{`null`, ``, false},
		{`false`, ``, false},
		{`""`, ``, false},
		{`0`, ``, false},
		{`"{\"x\":1}"`, `{"x":1}`, false},
		{`"not json"`, ``, true},
		{`7`, `7`, false},
	}
	for _, c := range cases {
		var raw []byte
		if c.raw != "" {
			raw = []byte(c.raw)
		}
		got, err := EnrichMetadata(raw)
		if (err != nil) != c.fails || string(got) != c.want {
			t.Errorf("%s: %s %v", c.raw, got, err)
		}
	}
}
