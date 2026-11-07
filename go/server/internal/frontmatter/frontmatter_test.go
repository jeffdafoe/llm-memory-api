package frontmatter

import "testing"

func TestParse(t *testing.T) {
	f, body := Parse("---\nsubject: Hi {agent}\nbad line\n:x\ntitle : T \n---\n\nBody\n")
	if f["subject"] != "Hi {agent}" || f["title"] != "T" || len(f) != 2 || body != "Body\n" {
		t.Fatalf("%v %q", f, body)
	}
	if f, body := Parse("---\n\n---\nB"); len(f) != 0 || body != "B" {
		t.Fatalf("%v %q", f, body)
	}
	if f, body := Parse("---\n---\nB"); len(f) != 0 || body != "---\n---\nB" {
		t.Fatalf("%v %q", f, body)
	}
	if _, body := Parse("no header"); body != "no header" {
		t.Fatal(body)
	}
}
