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
	status := reply.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	w.Write(reply.Body)
}

// defaultReply is a small, valid answer for each endpoint the providers
// call, shaped like the fields node/api/src/services/providers/*.js read.
func defaultReply(upstream, method, path string, body []byte) Reply {
	var req struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	json.Unmarshal(body, &req)
	model := req.Model
	if model == "" {
		model = "fake-model"
	}
	m, _ := json.Marshal(model)
	quotedModel := string(m)

	switch {
	case upstream == "openai" && path == "/v1/embeddings":
		return embeddingsReply(req.Input)
	case upstream == "anthropic" && path == "/v1/messages":
		return JSONReply(200, `{"id":"msg_fake","type":"message","role":"assistant","model":`+quotedModel+
			`,"content":[{"type":"text","text":"fake anthropic reply"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":10,"output_tokens":5}}`)
	case (upstream == "openai" || upstream == "xai") && path == "/v1/responses":
		return JSONReply(200, `{"id":"resp_fake","object":"response","status":"completed","model":`+quotedModel+
			`,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fake `+upstream+` reply"}]}],`+
			`"usage":{"input_tokens":10,"output_tokens":5}}`)
	case (upstream == "openai" && path == "/v1/chat/completions") ||
		(upstream == "openrouter" && path == "/api/v1/chat/completions") ||
		(upstream == "perplexity" && path == "/chat/completions"):
		return JSONReply(200, `{"id":"chatcmpl_fake","object":"chat.completion","model":`+quotedModel+
			`,"choices":[{"index":0,"message":{"role":"assistant","content":"fake `+upstream+` reply"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	case upstream == "openrouter" && method == http.MethodGet && path == "/api/v1/models":
		return JSONReply(200, `{"data":[]}`)
	case upstream == "google" && strings.HasPrefix(path, "/v1beta/models/") && strings.HasSuffix(path, ":generateContent"):
		return JSONReply(200, `{"candidates":[{"content":{"role":"model","parts":[{"text":"fake google reply"}]},"finishReason":"STOP"}],`+
			`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`)
	}
	return JSONReply(http.StatusNotFound, `{"error":{"message":"fake upstream has no default reply for `+upstream+` `+method+` `+strings.ReplaceAll(path, `"`, `'`)+`"}}`)
}

// embeddingsReply answers /v1/embeddings for a string or an array of strings.
func embeddingsReply(raw json.RawMessage) Reply {
	var inputs []string
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		inputs = []string{one}
	} else if err := json.Unmarshal(raw, &inputs); err != nil {
		return JSONReply(http.StatusBadRequest, `{"error":{"message":"fake upstream: input must be a string or an array of strings"}}`)
	}
	var b strings.Builder
	b.WriteString(`{"object":"list","data":[`)
	tokens := 0
	for i, text := range inputs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"object":"embedding","index":`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`,"embedding":`)
		b.WriteString(VectorLiteral(EmbeddingFor(text)))
		b.WriteByte('}')
		tokens += len(strings.Fields(text))
	}
	b.WriteString(`],"model":"text-embedding-3-small","usage":{"prompt_tokens":`)
	b.WriteString(strconv.Itoa(tokens))
	b.WriteString(`,"total_tokens":`)
	b.WriteString(strconv.Itoa(tokens))
	b.WriteString(`}}`)
	return JSONReply(200, b.String())
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
