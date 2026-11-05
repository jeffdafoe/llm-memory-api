package providers

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// A redirect to another origin (here another port on the same host, which
// Go's default policy treats as the same site) loses Authorization, as in
// fetch; a same-origin redirect keeps it.
func TestRedirectDropsAuthorizationAcrossOrigins(t *testing.T) {
	var got []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+" auth="+r.Header.Get("Authorization")+" key="+r.Header.Get("X-Api-Key"))
		w.Write([]byte("{}"))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+" auth="+r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/cross":
			http.Redirect(w, r, target.URL+"/landed", http.StatusTemporaryRedirect)
		case "/same":
			http.Redirect(w, r, "/landed", http.StatusTemporaryRedirect)
		default:
			w.Write([]byte("{}"))
		}
	}))
	defer source.Close()

	p := &Providers{}
	body := "{}"
	headers := []header{{"Authorization", "Bearer secret"}, {"x-api-key", "k"}}
	if _, err := p.send(context.Background(), http.MethodPost, source.URL+"/cross", headers, &body); err != nil {
		t.Fatal(err)
	}
	if _, err := p.send(context.Background(), http.MethodPost, source.URL+"/same", headers, &body); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/cross auth=Bearer secret",
		"/landed auth= key=k",
		"/same auth=Bearer secret",
		"/landed auth=Bearer secret",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n got %q\nwant %q", got, want)
	}
	// The shared client keeps its own policy.
	if http.DefaultClient.CheckRedirect != nil {
		t.Error("send changed the default client")
	}
}

// Once a chain leaves the origin, Authorization stays gone even when it
// comes back (A → B → A).
func TestRedirectBackToOriginKeepsAuthorizationDropped(t *testing.T) {
	var got []string
	var a *httptest.Server
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, "B auth="+r.Header.Get("Authorization"))
		http.Redirect(w, r, a.URL+"/back", http.StatusTemporaryRedirect)
	}))
	defer b.Close()
	a = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, "A"+r.URL.Path+" auth="+r.Header.Get("Authorization"))
		if r.URL.Path == "/start" {
			http.Redirect(w, r, b.URL+"/hop", http.StatusTemporaryRedirect)
			return
		}
		w.Write([]byte("{}"))
	}))
	defer a.Close()
	body := "{}"
	if _, err := (&Providers{}).send(context.Background(), http.MethodPost, a.URL+"/start", []header{{"Authorization", "Bearer secret"}}, &body); err != nil {
		t.Fatal(err)
	}
	want := "A/start auth=Bearer secret|B auth=|A/back auth="
	if strings.Join(got, "|") != want {
		t.Errorf("got %q, want %q", strings.Join(got, "|"), want)
	}
}

// fetch follows 20 redirects and fails on the 21st.
func TestRedirectLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/"), "%d", &n)
		if n > 0 {
			http.Redirect(w, r, "/"+strconv.Itoa(n-1), http.StatusFound)
			return
		}
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	p := &Providers{}
	if resp, err := p.send(context.Background(), http.MethodGet, srv.URL+"/20", nil, nil); err != nil || resp.status != 200 {
		t.Errorf("20 redirects: %v", err)
	}
	if _, err := p.send(context.Background(), http.MethodGet, srv.URL+"/21", nil, nil); err == nil || !strings.Contains(err.Error(), "redirect count exceeded") {
		t.Errorf("21 redirects: %v", err)
	}
}

// Header values go out as fetch's ByteString bytes: é is one byte, 0xE9.
func TestHeaderValuesAreByteStrings(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	raw := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		var lines []string
		for {
			line, err := r.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
			lines = append(lines, line)
		}
		raw <- strings.Join(lines, "")
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}"))
	}()
	p := &Providers{}
	if _, err := p.send(context.Background(), http.MethodGet, "http://"+ln.Addr().String()+"/", []header{{"x-latin", " café "}}, nil); err != nil {
		t.Fatal(err)
	}
	if head := <-raw; !strings.Contains(head, "X-Latin: caf\xe9\r\n") {
		t.Errorf("raw request head %q", head)
	}
}

func TestRegistryCopies(t *testing.T) {
	caps := OpenRouterCapabilities()
	caps[0].Label = "changed"
	caps[2].Options[0] = "changed"
	if again := OpenRouterCapabilities(); again[0].Label != "Temperature" || again[2].Options[0] != "off" {
		t.Error("OpenRouterCapabilities shares its slice")
	}
	got, _ := GetModelCapabilities("openai", "gpt-5.5")
	got[0].DisabledWhen.Value = "changed"
	*got[0].Max = 99
	if again, _ := GetModelCapabilities("openai", "gpt-5.5"); again[0].DisabledWhen.Value != "none" || *again[0].Max != 2 {
		t.Error("GetModelCapabilities shares the registry")
	}
	pricing, _ := GetModelPricing("anthropic", "claude-opus-4-6")
	*pricing.Input = 0
	if again, _ := GetModelPricing("anthropic", "claude-opus-4-6"); *again.Input != 5 {
		t.Error("GetModelPricing shares the registry")
	}
}
