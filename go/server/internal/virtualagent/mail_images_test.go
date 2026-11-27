package virtualagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// mail_images_test.go — a virtual agent answering mail with images
// (LLM-1040). The request-level tests replay the Node "happy path" mail
// golden (an Anthropic claude-haiku agent) with images attached, through the
// golden harness's recorder, which captures the provider call.

// The lines a mail's images add to the text of the user message: what the
// logged prompt, the transcript and learning extraction keep of them.
func TestImageLines(t *testing.T) {
	if got := imageLines(nil); got != "" {
		t.Errorf("no images: got %q", got)
	}
	got := imageLines([]mailImage{{Filename: "wagons.png"}, {Filename: "cart.png"}})
	want := "\n\nAttached images, in order:\n[image 1: wagons.png]\n[image 2: cart.png]"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// The base64 of two images as Postgres's encode() returns it: wrapped at 76
// characters, the newlines the service drops.
const (
	imgA        = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	imgAWrapped = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9\nawAAAABJRU5ErkJggg=="
	imgB        = "/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAAMCAgICAgMCAgIDAwMDBAYEBAQEBAgGBgUGCQgKCgkICQkKDA8MCgsOCwkJDRENDg8QEBEQCgwSExIQEw8QEBD/"
)

// imageScenario is the happy-path golden with two images attached, and the
// agent row's provider and model replaced when given.
func imageScenario(t *testing.T, provider, model string) (scenarioCase, string) {
	t.Helper()
	var c scenarioCase
	for _, m := range loadMailGoldens(t).Mail {
		if m.Name == "happy path" {
			c = scenarioCase{scenario: m.scenario, Agent: m.Agent, From: m.From, MailID: m.MailID}
		}
	}
	if c.Agent == "" {
		t.Fatal("no happy path golden")
	}
	rows := map[string]json.RawMessage{}
	for k, v := range c.Rows {
		rows[k] = v
	}
	rows["mail-attachments"] = json.RawMessage(`[` +
		`{"filename":"a.png","media_type":"image/png","data":"` + strings.ReplaceAll(imgAWrapped, "\n", `\n`) + `"},` +
		`{"filename":"b.jpg","media_type":"image/jpeg","data":"` + imgB + `"}]`)
	if provider != "" {
		// jsutil keeps the row's column order, which the harness scans by.
		v, err := jsutil.Parse(rows["load-agent"])
		if err != nil {
			t.Fatal(err)
		}
		row := v.([]any)[0].(*jsutil.Object)
		row.Set("provider", provider)
		row.Set("model", model)
		rows["load-agent"] = json.RawMessage(jsutil.Marshal(v))
	}
	c.Rows = rows
	// The original call's system prompt, to compare with.
	system := ""
	if len(c.Calls) > 0 {
		system = jsonOf(c.Calls[0].System)
	}
	return c, system
}

type scenarioCase struct {
	scenario
	Agent, From, MailID string
}

// settle waits, as compareMail does, until the background work (learning
// extraction, the transcript) has written as much as the golden recording
// did, then holds the recorder's lock for the caller's reads; the caller
// unlocks.
func settle(r *recorder, want *scenario) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		done := len(r.queries) >= len(want.Queries) && len(r.logs) >= len(want.Logs) && len(r.notes) >= len(want.Notes)
		r.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	r.mu.Lock()
}

func TestDirectMailWithImagesSendsThemToTheModel(t *testing.T) {
	old := localZone
	localZone = time.FixedZone("IST", 5*3600+1800)
	defer func() { localZone = old }()
	c, wantSystem := imageScenario(t, "", "")
	s, r := newService(t, &c.scenario)
	if err := s.HandleDirectMail(context.Background(), c.Agent, c.From, c.MailID); err != nil {
		t.Fatal(err)
	}
	settle(r, &c.scenario)
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		t.Fatal("no provider call")
	}
	call := r.calls[0]
	// The system prompt is the one the mail gets without images.
	if got := jsonOf(call.System); got != wantSystem {
		t.Errorf("system prompt changed:\n got %s\nwant %s", got, wantSystem)
	}
	// The text names the images, in order.
	if !strings.HasSuffix(call.User, "\n\nAttached images, in order:\n[image 1: a.png]\n[image 2: b.jpg]") {
		t.Errorf("user text: %q", call.User)
	}
	// The call carries one user message: that text, then the two images in
	// order, as data URLs with the base64 unwrapped.
	wantOpts := `{"messages":[{"role":"user","content":[{"type":"text","text":` + jsonOf(call.User) + `},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + imgA + `"}},` +
		`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,` + imgB + `"}}]}]}`
	if call.Opts != wantOpts {
		t.Errorf("opts:\n got %s\nwant %s", call.Opts, wantOpts)
	}
	// The image data reaches the provider call only: not the call log, the
	// usage rows, the transcript, the learnings or the log lines.
	for _, q := range r.queries {
		for _, p := range q.Params {
			if p != nil && (strings.Contains(*p, imgA[:40]) || strings.Contains(*p, imgB[:40])) {
				t.Errorf("image data in a %s query", q.Kind)
			}
		}
	}
	for _, n := range r.notes {
		if strings.Contains(string(n.Content), imgA[:40]) || strings.Contains(string(n.Content), imgB[:40]) {
			t.Errorf("image data in note %s", n.Slug)
		}
	}
	for _, l := range r.logs {
		if strings.Contains(l.Details, imgA[:40]) || strings.Contains(l.Details, imgB[:40]) {
			t.Errorf("image data in log %s/%s", l.Subsystem, l.Action)
		}
	}
}

// A model not known to read images gets no call; the sender is told in a
// threaded [Error] reply.
func TestDirectMailWithImagesToATextModelIsRefused(t *testing.T) {
	c, _ := imageScenario(t, "openrouter", "meta-llama/llama-3.3-70b")
	s, r := newService(t, &c.scenario)
	if err := s.HandleDirectMail(context.Background(), c.Agent, c.From, c.MailID); err != nil {
		t.Fatal(err)
	}
	// The refusal sends its reply before returning; nothing runs in the
	// background, so the reads only need the lock.
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 0 {
		t.Errorf("%d provider call(s); want none", len(r.calls))
	}
	sent := false
	for _, q := range r.queries {
		if q.Kind != "mail-send" {
			continue
		}
		body, inReplyTo := *q.Params[3], *q.Params[4]
		if strings.HasPrefix(body, "[Error] This mail has 2 image attachment(s)") && inReplyTo == c.MailID {
			sent = true
		}
	}
	if !sent {
		t.Error("no [Error] reply threaded on the mail")
	}
}
