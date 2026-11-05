package contract

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Upstreams maps each fake upstream's path segment to the server variable
// (LLM-906, node/api/src/services/api-base.js) that points a server at it.
var Upstreams = []struct {
	Name string
	Env  string
}{
	{"anthropic", "ANTHROPIC_API_BASE"},
	{"openai", "OPENAI_API_BASE"},
	{"google", "GOOGLE_API_BASE"},
	{"openrouter", "OPENROUTER_API_BASE"},
	{"perplexity", "PERPLEXITY_API_BASE"},
	{"xai", "XAI_API_BASE"},
}

// EmbeddingDims is the size of text-embedding-3-small vectors, the model
// node/api/src/services/embeddings.js asks for.
const EmbeddingDims = 1536

// authHeaders are the headers the providers carry their key in: Bearer for
// OpenAI, OpenRouter, Perplexity and xAI, x-api-key for Anthropic,
// x-goog-api-key for Google. Only their presence is recorded.
var authHeaders = []string{"authorization", "x-api-key", "x-goog-api-key"}

const maxUpstreamBody = 32 << 20

// Reply is a canned upstream answer.
type Reply struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSONReply is a Reply with a JSON body and status 200 unless given.
func JSONReply(status int, body string) Reply {
	if status == 0 {
		status = http.StatusOK
	}
	return Reply{Status: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
}

// Recorded is one request a server sent to the fake, as it arrived.
type Recorded struct {
	Upstream string
	Method   string
	Path     string // after the side and upstream prefix, with any query
	Auth     []string
	Body     []byte
}

// FakeUpstream stands in for every LLM provider and for OpenAI embeddings.
// Each server under test is one side ("node", "go", or a per-scenario name):
// its *_API_BASE variables point at /<side>/<upstream>, so the fake knows who
// sent each request, records it, and answers it. A scenario can queue its own
// replies; the queue is copied to every side, so both servers get the same
// answers in the same order. Anything not queued gets the default reply for
// its endpoint.
type FakeUpstream struct {
	srv *httptest.Server

	mu     sync.Mutex
	sides  map[string]*fakeSide
	queued map[string][]Reply // "<upstream> <path>" -> replies, copied to new sides
}

type fakeSide struct {
	recorded []Recorded
	queue    map[string][]Reply
	inFlight int
	last     time.Time
}

// NewFakeUpstream starts the fake on a loopback port.
func NewFakeUpstream() *FakeUpstream {
	f := &FakeUpstream{sides: map[string]*fakeSide{}, queued: map[string][]Reply{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Close stops the fake.
func (f *FakeUpstream) Close() {
	f.srv.Close()
}

// URL is the fake's origin.
func (f *FakeUpstream) URL() string {
	return f.srv.URL
}

// Env returns the *_API_BASE settings for one side, as KEY=value pairs.
func (f *FakeUpstream) Env(side string) []string {
	f.side(side)
	env := make([]string, 0, len(Upstreams))
	for _, u := range Upstreams {
		env = append(env, u.Env+"="+f.srv.URL+"/"+side+"/"+u.Name)
	}
	return env
}

// side returns a side's state, creating it with a copy of the queued replies.
func (f *FakeUpstream) side(name string) *fakeSide {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sideLocked(name)
}

func (f *FakeUpstream) sideLocked(name string) *fakeSide {
	s, ok := f.sides[name]
	if !ok {
		s = &fakeSide{queue: map[string][]Reply{}}
		for k, v := range f.queued {
			s.queue[k] = append([]Reply(nil), v...)
		}
		f.sides[name] = s
	}
	return s
}

// Queue adds replies for one endpoint (path without the query, for example
// "/v1/messages") on every side, existing and future. They are used in
// order, before the default reply.
func (f *FakeUpstream) Queue(upstream, path string, replies ...Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := upstream + " " + path
	f.queued[key] = append(f.queued[key], replies...)
	for _, s := range f.sides {
		s.queue[key] = append(s.queue[key], replies...)
	}
}

// Requests returns what one side sent, in arrival order.
func (f *FakeUpstream) Requests(side string) []Recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sides[side]
	if !ok {
		return nil
	}
	return append([]Recorded(nil), s.recorded...)
}

// Activity returns how many of a side's requests are being answered now and
// when one last started or finished, for the settle step.
func (f *FakeUpstream) Activity(side string) (inFlight int, last time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sides[side]
	if !ok {
		return 0, time.Time{}
	}
	return s.inFlight, s.last
}

func (f *FakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if len(parts) < 2 {
		http.Error(w, "fake upstream: no side and upstream in path", http.StatusNotFound)
		return
	}
	sideName, upstream := parts[0], parts[1]
	rest := "/"
	if len(parts) == 3 {
		rest = "/" + parts[2]
	}

	f.mu.Lock()
	s := f.sideLocked(sideName)
	s.inFlight++
	s.last = time.Now()
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		s.inFlight--
		s.last = time.Now()
		f.mu.Unlock()
	}()

	body, _ := io.ReadAll(io.LimitReader(r.Body, maxUpstreamBody))
	rec := Recorded{Upstream: upstream, Method: r.Method, Path: rest, Body: body}
	if r.URL.RawQuery != "" {
		rec.Path += "?" + r.URL.RawQuery
	}
	for _, h := range authHeaders {
		if r.Header.Get(h) != "" {
			rec.Auth = append(rec.Auth, h)
		}
	}

	f.mu.Lock()
	s.recorded = append(s.recorded, rec)
	key := upstream + " " + rest
	var reply Reply
	queued := false
	if q := s.queue[key]; len(q) > 0 {
		reply, s.queue[key] = q[0], q[1:]
		queued = true
	}
	f.mu.Unlock()

	if !queued {
		reply = defaultReply(upstream, r.Method, rest, body)
	}
	for k, vs := range reply.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	status := reply.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	w.Write(reply.Body)
}

// The default replies are fixed values built with json.Marshal. Nothing from
// the request is echoed: the providers read only the fields below, and a
// fake that reflected its input would be a needless injection path.
const fakeModel = "fake-model"

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type tokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func marshalReply(status int, v any) Reply {
	data, err := json.Marshal(v)
	if err != nil {
		// Only fixed shapes come here; a failure is a bug in this file.
		panic(err)
	}
	return Reply{Status: status, Body: data}
}

// defaultReply is a small, valid answer for each endpoint the providers
// call, shaped like the fields node/api/src/services/providers/*.js read.
func defaultReply(upstream, method, path string, body []byte) Reply {
	switch {
	case upstream == "openai" && path == "/v1/embeddings":
		return embeddingsReply(body)
	case upstream == "anthropic" && path == "/v1/messages":
		return anthropicReply()
	case upstream == "openai" && path == "/v1/responses":
		return responsesReply("fake openai reply")
	case upstream == "xai" && path == "/v1/responses":
		return responsesReply("fake xai reply")
	case upstream == "openai" && path == "/v1/chat/completions":
		return chatReply("fake openai reply")
	case upstream == "openrouter" && path == "/api/v1/chat/completions":
		return chatReply("fake openrouter reply")
	case upstream == "perplexity" && path == "/chat/completions":
		return chatReply("fake perplexity reply")
	case upstream == "openrouter" && method == http.MethodGet && path == "/api/v1/models":
		return marshalReply(http.StatusOK, struct {
			Data []struct{} `json:"data"`
		}{Data: []struct{}{}})
	case upstream == "google" && strings.HasPrefix(path, "/v1beta/models/") && strings.HasSuffix(path, ":generateContent"):
		return googleReply()
	}
	// The recorded request names the endpoint; the reply does not echo it.
	return marshalReply(http.StatusNotFound, map[string]map[string]string{
		"error": {"message": "fake upstream has no default reply for this endpoint"},
	})
}

func anthropicReply() Reply {
	return marshalReply(http.StatusOK, struct {
		ID         string     `json:"id"`
		Type       string     `json:"type"`
		Role       string     `json:"role"`
		Model      string     `json:"model"`
		Content    []textPart `json:"content"`
		StopReason string     `json:"stop_reason"`
		Usage      tokenUsage `json:"usage"`
	}{"msg_fake", "message", "assistant", fakeModel, []textPart{{"text", "fake anthropic reply"}}, "end_turn", tokenUsage{10, 5}})
}

// responsesReply is the /v1/responses shape OpenAI and xAI share.
func responsesReply(text string) Reply {
	type outputItem struct {
		Type    string     `json:"type"`
		Role    string     `json:"role"`
		Content []textPart `json:"content"`
	}
	return marshalReply(http.StatusOK, struct {
		ID     string       `json:"id"`
		Object string       `json:"object"`
		Status string       `json:"status"`
		Model  string       `json:"model"`
		Output []outputItem `json:"output"`
		Usage  tokenUsage   `json:"usage"`
	}{"resp_fake", "response", "completed", fakeModel,
		[]outputItem{{"message", "assistant", []textPart{{"output_text", text}}}}, tokenUsage{10, 5}})
}

// chatReply is the chat-completions shape OpenAI, OpenRouter and Perplexity
// share.
func chatReply(text string) Reply {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type choice struct {
		Index        int     `json:"index"`
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}
	return marshalReply(http.StatusOK, struct {
		ID      string    `json:"id"`
		Object  string    `json:"object"`
		Model   string    `json:"model"`
		Choices []choice  `json:"choices"`
		Usage   chatUsage `json:"usage"`
	}{"chatcmpl_fake", "chat.completion", fakeModel,
		[]choice{{0, message{"assistant", text}, "stop"}}, chatUsage{10, 5}})
}

func googleReply() Reply {
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Role  string `json:"role"`
		Parts []part `json:"parts"`
	}
	type candidate struct {
		Content      content `json:"content"`
		FinishReason string  `json:"finishReason"`
	}
	type usage struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	}
	return marshalReply(http.StatusOK, struct {
		Candidates    []candidate `json:"candidates"`
		UsageMetadata usage       `json:"usageMetadata"`
	}{[]candidate{{content{"model", []part{{"fake google reply"}}}, "STOP"}}, usage{10, 5}})
}

// embeddingsReply answers /v1/embeddings for a string or an array of
// strings. Each vector is computed from its text; the text itself is not in
// the reply.
func embeddingsReply(body []byte) Reply {
	var req struct {
		Input json.RawMessage `json:"input"`
	}
	json.Unmarshal(body, &req)
	var inputs []string
	var one string
	if err := json.Unmarshal(req.Input, &one); err == nil {
		inputs = []string{one}
	} else if err := json.Unmarshal(req.Input, &inputs); err != nil {
		return marshalReply(http.StatusBadRequest, map[string]map[string]string{
			"error": {"message": "fake upstream: input must be a string or an array of strings"},
		})
	}
	type item struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	type usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	}
	items := make([]item, len(inputs))
	for i, text := range inputs {
		items[i] = item{"embedding", i, EmbeddingFor(text)}
	}
	// encoding/json writes a float32 as the shortest decimal that reads back
	// as the same float32, as VectorLiteral does.
	return marshalReply(http.StatusOK, struct {
		Object string `json:"object"`
		Data   []item `json:"data"`
		Model  string `json:"model"`
		Usage  usage  `json:"usage"`
	}{"list", items, "text-embedding-3-small", usage{len(inputs), len(inputs)}})
}

// EmbeddingFor is the fake embedding of a text: a unit vector drawn from
// SHA-256 of the text, so the same text always gets the same vector and the
// fixture can store chunks with the vectors a search will compare against.
// The values are float32, as pgvector stores them.
func EmbeddingFor(text string) []float32 {
	seed := sha256.Sum256([]byte(text))
	vec := make([]float32, EmbeddingDims)
	var block [sha256.Size]byte
	var sum float64
	for i := 0; i < EmbeddingDims; i++ {
		if i%8 == 0 {
			var counter [8]byte
			binary.BigEndian.PutUint64(counter[:], uint64(i/8))
			block = sha256.Sum256(append(seed[:], counter[:]...))
		}
		u := binary.BigEndian.Uint32(block[(i%8)*4:])
		x := float64(u)/float64(math.MaxUint32)*2 - 1
		vec[i] = float32(x)
		sum += x * x
	}
	norm := math.Sqrt(sum)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

// VectorLiteral writes a vector as a JSON array (also pgvector's text form),
// each value the shortest decimal that reads back as the same float32.
func VectorLiteral(vec []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
