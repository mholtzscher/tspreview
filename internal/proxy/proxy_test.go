package proxy

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestForwardingAndLongestRoute(t *testing.T) {
	observed := make(chan string, 10)
	upstream := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			observed <- label + " " + r.Method + " " + r.URL.RequestURI() + " " + string(body) + " " + r.Header.Get("X-Forwarded-Host")
			w.Header().Set("Location", "/next")
			w.Header().Set("Set-Cookie", "session=abc; Path=/")
			w.WriteHeader(http.StatusTemporaryRedirect)
			io.WriteString(w, label)
		}))
	}
	base, nats, nested := upstream("web"), upstream("nats"), upstream("nested")
	defer base.Close()
	defer nats.Close()
	defer nested.Close()
	h, err := New(base.URL, []Route{{Prefix: "/nats", Upstream: nats.URL}, {Prefix: "/nats/admin", Upstream: nested.URL}}, log.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	server := httptest.NewServer(h)
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, tc := range []struct{ path, want string }{
		{"/assets/a%2Fb.js?x=1&x=2", "web POST /assets/a%2Fb.js?x=1&x=2 payload preview.test:12345"},
		{"/nats", "nats POST / payload preview.test:12345"},
		{"/nats/events?x=1", "nats POST /events?x=1 payload preview.test:12345"},
		{"/%6eats/a%2Fb?x=1", "nats POST /a%2Fb?x=1 payload preview.test:12345"},
		{"/nats/admin/status", "nested POST /status payload preview.test:12345"},
		{"/nats-other", "web POST /nats-other payload preview.test:12345"},
	} {
		req, _ := http.NewRequest(http.MethodPost, server.URL+tc.path, strings.NewReader("payload"))
		req.Host = "preview.test:12345"
		req.Header.Set("X-Forwarded-Host", "spoofed.test")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if got := <-observed; got != tc.want {
			t.Errorf("forwarded %q, want %q", got, tc.want)
		}
		if res.StatusCode != 307 || res.Header.Get("Location") != "/next" || res.Header.Get("Set-Cookie") != "session=abc; Path=/" {
			t.Fatalf("response changed: %d %v", res.StatusCode, res.Header)
		}
	}
}

func TestRejectInvalidOriginsAndRoutes(t *testing.T) {
	for _, origin := range []string{
		"", "localhost:3000", "ftp://127.0.0.1:3000", "http://example.com:3000",
		"http://0.0.0.0:3000", "http://127.0.0.1:0", "http://127.0.0.1:65536",
		"http://user:pass@127.0.0.1:3000", "http://127.0.0.1:3000/path",
		"http://127.0.0.1:3000?", "http://127.0.0.1:3000?x=1", "http://127.0.0.1:3000#fragment",
		"http://127.0.0.1:",
	} {
		t.Run(origin, func(t *testing.T) {
			if _, err := New(origin, nil, nil); err == nil {
				t.Fatal("accepted invalid upstream")
			}
		})
	}
	for _, prefix := range []string{"", "/", "nats", "/nats/", "/nats//x", "/../nats", "/nats?x", "/%6eats"} {
		if _, err := New("http://localhost:3000", []Route{{prefix, "http://127.0.0.1:4223"}}, nil); err == nil {
			t.Errorf("accepted prefix %q", prefix)
		}
	}
	if _, err := New("http://localhost:3000", []Route{{"/nats", "http://127.0.0.1:4223"}, {"/nats", "http://127.0.0.1:4224"}}, nil); err == nil {
		t.Error("accepted duplicate prefix")
	}
	for _, origin := range []string{"http://localhost:3000/", "https://127.0.0.1:443", "http://[::1]:3000"} {
		h, err := New(origin, nil, nil)
		if err != nil {
			t.Errorf("rejected %q: %v", origin, err)
		} else {
			h.Close()
		}
	}
}

func TestStreamingArrivesBeforeUpstreamFinishes(t *testing.T) {
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first\n")
		http.NewResponseController(w).Flush()
		select {
		case <-finish:
		case <-r.Context().Done():
		}
		io.WriteString(w, "last\n")
	}))
	defer upstream.Close()
	h, err := New(upstream.URL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	server := httptest.NewServer(h)
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(server.URL)
	if err != nil {
		close(finish)
		t.Fatal(err)
	}
	defer res.Body.Close()
	first := make([]byte, 6)
	_, err = io.ReadFull(res.Body, first)
	close(finish)
	if err != nil || string(first) != "first\n" {
		t.Fatalf("first chunk = %q, %v", first, err)
	}
	last, err := io.ReadAll(res.Body)
	if err != nil || string(last) != "last\n" {
		t.Fatalf("last chunk = %q, %v", last, err)
	}
}

func TestWebSocketRoute(t *testing.T) {
	paths := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.RequestURI()
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		kind, message, err := conn.Read(r.Context())
		if err == nil {
			conn.Write(r.Context(), kind, message)
		}
	}))
	defer upstream.Close()
	h, err := New(upstream.URL, []Route{{"/nats", upstream.URL}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	server := httptest.NewServer(h)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, server.URL+"/nats/events?token=test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageText, []byte("hello over websocket")); err != nil {
		t.Fatal(err)
	}
	kind, body, err := conn.Read(ctx)
	if err != nil || kind != websocket.MessageText || string(body) != "hello over websocket" {
		t.Fatalf("echo: %v %q %v", kind, body, err)
	}
	if path := <-paths; path != "/events?token=test" {
		t.Fatalf("WebSocket path = %q", path)
	}
}

func TestUnavailableUpstreamRecoversWithoutProxyRestart(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	h, err := New("http://"+address, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	server := httptest.NewServer(h)
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 502 {
		t.Fatalf("unavailable upstream returned %d", res.StatusCode)
	}
	listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "recovered") }))
	upstream.Listener.Close()
	upstream.Listener = listener
	upstream.Start()
	defer upstream.Close()
	res, err = client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != 200 || string(body) != "recovered" {
		t.Fatalf("recovery = %d %q, %v", res.StatusCode, body, err)
	}
}
