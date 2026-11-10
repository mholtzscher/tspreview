package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestListenerSetupCancellationClosesBoundListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, err = listenTLS(ctx, 443, func(_, _ string) (net.Listener, error) {
		cancel() // Cancellation between readiness and local listener binding.
		return raw, nil
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("listener setup error = %v", err)
	}
	if _, err := raw.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("canceled listener remained open: %v", err)
	}
	called := false
	_, err = listenTLS(ctx, 443, func(_, _ string) (net.Listener, error) {
		called = true
		return nil, nil
	}, nil)
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("already-canceled setup attempted binding: called=%v err=%v", called, err)
	}
}

type tlsLoopbackNetwork struct{ certificate tls.Certificate }

func (n tlsLoopbackNetwork) Listen(ctx context.Context) (net.Listener, string, error) {
	return n.ListenPort(ctx, 443)
}
func (n tlsLoopbackNetwork) ListenPort(ctx context.Context, _ int) (net.Listener, string, error) {
	ln, err := listenTLS(ctx, 18234, func(_, _ string) (net.Listener, error) {
		return net.Listen("tcp", "127.0.0.1:0")
	}, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &n.certificate, nil })
	if err != nil {
		return nil, "", err
	}
	return ln, "https://" + ln.Addr().String(), nil
}
func (tlsLoopbackNetwork) Close() error { return nil }

func TestProxyTLSListenerPreservesSecureForwarding(t *testing.T) {
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	defer fixture.Close()
	c := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, c, tlsLoopbackNetwork{certificate: fixture.TLS.Certificates[0]}, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	startup, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	awaitStatus(t, startup, c, done)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s", r.Header.Get("X-Forwarded-Proto"), r.Header.Get("X-Forwarded-Host"))
	}))
	defer upstream.Close()
	lease, p, err := newControlClient(c).openProxy(ctx, startup, proxyRequest{Worktree: t.TempDir(), Name: "web", Upstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	client := fixture.Client()
	client.Timeout = 5 * time.Second
	res, err := client.Get(p.URL + "/secure")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != "https "+p.URL[len("https://"):] {
		t.Fatalf("TLS forwarding = %q, %v", body, err)
	}
}

func TestTLSListenerServesHTTPS(t *testing.T) {
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	defer fixture.Close()
	cert := fixture.TLS.Certificates[0]
	var certRequests atomic.Int32
	ln, err := listenTLS(context.Background(), 18234, func(network, addr string) (net.Listener, error) {
		if network != "tcp" || addr != ":18234" {
			t.Fatalf("unexpected binding %s %s", network, addr)
		}
		return net.Listen("tcp", "127.0.0.1:0")
	}, func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		certRequests.Add(1)
		return &cert, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "encrypted preview")
	})}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	defer func() {
		srv.Close()
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	}()
	res, err := fixture.Client().Get("https://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != 200 || string(body) != "encrypted preview" || certRequests.Load() == 0 {
		t.Fatalf("TLS response %d %q, certificate requests %d, err %v", res.StatusCode, body, certRequests.Load(), err)
	}
}
