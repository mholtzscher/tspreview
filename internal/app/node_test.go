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
)

func TestListenerSetupCancellationClosesBoundListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, err = listenTLS(ctx, func(_, _ string) (net.Listener, error) {
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
	_, err = listenTLS(ctx, func(_, _ string) (net.Listener, error) {
		called = true
		return nil, nil
	}, nil)
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("already-canceled setup attempted binding: called=%v err=%v", called, err)
	}
}

func TestTLSListenerServesHTTPS(t *testing.T) {
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	defer fixture.Close()
	cert := fixture.TLS.Certificates[0]
	var certRequests atomic.Int32
	ln, err := listenTLS(context.Background(), func(network, addr string) (net.Listener, error) {
		if network != "tcp" || addr != ":443" {
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
