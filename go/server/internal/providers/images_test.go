package providers

import (
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// images_test.go — image parts in a user message (LLM-1040): the neutral
// OpenAI-chat message, and each adapter's translation of it.

const pngB64 = "iVBORw0KGgpmYWtl"

// toJSON is the JSON a request body would carry, keys in insertion order.
// jsutil.Marshal takes JavaScript values, so a []*object is passed as []any.
func toJSON(t *testing.T, v any) string {
	t.Helper()
	if list, ok := v.([]*object); ok {
		values := make([]any, len(list))
		for i, o := range list {
			values[i] = o
		}
		v = values
	}
	return jsutil.Marshal(v)
}

func imageMessage() *object {
	return ImageUserMessage("look at this", []Image{{MediaType: "image/png", DataBase64: pngB64}})
}

func TestImageUserMessageIsOpenAIChatShape(t *testing.T) {
	got := toJSON(t, imageMessage())
	want := `{"role":"user","content":[{"type":"text","text":"look at this"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + pngB64 + `"}}]}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestResponsesInputTranslatesImages(t *testing.T) {
	input, err := messagesToResponsesInput([]*object{imageMessage()})
	if err != nil {
		t.Fatal(err)
	}
	got := toJSON(t, input)
	want := `[{"role":"user","content":[{"type":"input_text","text":"look at this"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,` + pngB64 + `","detail":"auto"}]}]`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestAnthropicTranslatesImages(t *testing.T) {
	out, err := translateMessagesToAnthropic([]*object{imageMessage()})
	if err != nil {
		t.Fatal(err)
	}
	got := toJSON(t, out)
	want := `[{"role":"user","content":[{"type":"text","text":"look at this"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngB64 + `"}}]}]`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGoogleTranslatesImages(t *testing.T) {
	out, err := translateMessagesToGoogle([]*object{imageMessage()})
	if err != nil {
		t.Fatal(err)
	}
	got := toJSON(t, out)
	want := `[{"role":"user","parts":[{"text":"look at this"},` +
		`{"inline_data":{"mime_type":"image/png","data":"` + pngB64 + `"}}]}]`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// A user message without images goes out exactly as before on every
// adapter: string content as a string, and a text-part array (with its
// extra keys, such as a cache marker) as the caller built it.
func TestTextOnlyContentIsUnchanged(t *testing.T) {
	str := obj("role", "user", "content", "plain")
	parts := obj("role", "user", "content", []any{obj("type", "text", "text", "a", "cache_control", obj("type", "ephemeral"))})

	responses, err := messagesToResponsesInput([]*object{str, parts})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := toJSON(t, responses), `[{"role":"user","content":"plain"},`+
		`{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}]`; got != want {
		t.Errorf("responses: got %s\nwant %s", got, want)
	}
	anthropic, err := translateMessagesToAnthropic([]*object{str, parts})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := toJSON(t, anthropic), `[{"role":"user","content":"plain"},`+
		`{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}]`; got != want {
		t.Errorf("anthropic: got %s\nwant %s", got, want)
	}
	google, err := translateMessagesToGoogle([]*object{str, parts})
	if err != nil {
		t.Fatal(err)
	}
	// Gemini wraps any content in one text part, an array included, as it
	// did before images.
	if got, want := toJSON(t, google), `[{"role":"user","parts":[{"text":"plain"}]},`+
		`{"role":"user","parts":[{"text":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}]}]`; got != want {
		t.Errorf("google: got %s\nwant %s", got, want)
	}
}

// Images go only to a provider and model known to read them: the adapter
// alone is not enough (a model may be text-only), and OpenRouter, which
// routes to models of every kind, is not listed.
func TestSupportsImages(t *testing.T) {
	cases := []struct {
		provider, model string
		want            bool
	}{
		{"openai", "gpt-6-luna", true},
		{"OpenAI", "gpt-5.4", true},
		{"openai", "gpt-4o-mini", true},
		{"openai", "o3", true},
		{"openai", "gpt-3.5-turbo", false},
		{"anthropic", "claude-haiku-4-5", true},
		{"google", "gemini-2.5-flash", true},
		{"google", "gemma-3", false},
		// Text-only variants within the vision families.
		{"openai", "gpt-4o-mini-tts", false},
		{"openai", "gpt-4o-audio-preview", false},
		{"openai", "gpt-4o-realtime-preview", false},
		{"openai", "gpt-4o-transcribe", false},
		{"openai", "gpt-4o-search-preview", false},
		{"google", "gemini-2.5-flash-preview-tts", false},
		{"google", "gemini-embedding-001", false},
		{"google", "gemini-2.0-flash-live-001", false},
		{"anthropic", "claude-2.1", false},
		{"anthropic", "claude-instant-1.2", false},
		{"anthropic", "claude-sonnet-4-5", true},
		{"openrouter", "openai/gpt-4o", false},
		{"open-router", "anthropic/claude-sonnet-4", false},
		{"perplexity", "sonar", false},
		{"xai", "grok-4", false},
		{"nope", "gpt-6", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := SupportsImages(c.provider, c.model); got != c.want {
			t.Errorf("SupportsImages(%q, %q) = %v, want %v", c.provider, c.model, got, c.want)
		}
	}
}

func TestSplitDataURL(t *testing.T) {
	mediaType, data, ok := splitDataURL("data:image/webp;base64,QUJD")
	if !ok || mediaType != "image/webp" || data != "QUJD" {
		t.Errorf("got %q %q %v", mediaType, data, ok)
	}
	for _, bad := range []string{"https://x/y.png", "data:image/png,QUJD", "data:;base64,QUJD", "data:image/png;base64"} {
		if _, _, ok := splitDataURL(bad); ok {
			t.Errorf("%q should not split", bad)
		}
	}
}
