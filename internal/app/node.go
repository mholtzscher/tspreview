package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"tailscale.com/tsnet"
)

// network is the external boundary replaced by a real loopback listener in tests.
// Production only ever constructs a tsnetNetwork, never an OS TCP listener.
type network interface {
	Listen(context.Context) (net.Listener, string, error)
	ListenPort(context.Context, int) (net.Listener, string, error)
	Close() error
}

type tsnetNetwork struct {
	server         *tsnet.Server
	getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	dnsName        string
}

func newTSNet(c config, hostname string, stderr io.Writer) *tsnetNetwork {
	s := &tsnet.Server{
		Dir:      c.StateDir + "/node",
		Hostname: hostname,
		UserLogf: func(format string, args ...any) { fmt.Fprintf(stderr, format+"\n", args...) },
	}
	if c.Verbose {
		s.Logf = s.UserLogf
	}
	return &tsnetNetwork{server: s}
}

func (n *tsnetNetwork) connect(ctx context.Context) (identity, error) {
	if err := n.server.Start(); err != nil {
		return identity{}, err
	}
	status, err := n.server.Up(ctx)
	if err != nil {
		return identity{}, fmt.Errorf("connect to Tailscale (run tspreview login if authentication expired): %w", err)
	}
	if status.Self == nil || status.Self.DNSName == "" {
		return identity{}, fmt.Errorf("Tailscale did not assign a DNS name")
	}
	if status.CurrentTailnet == nil || !status.CurrentTailnet.MagicDNSEnabled || len(status.CertDomains) == 0 {
		return identity{}, fmt.Errorf("enable MagicDNS and HTTPS certificates in the Tailscale admin DNS settings")
	}
	id := identity{Hostname: n.server.Hostname, DNSName: strings.TrimSuffix(status.Self.DNSName, ".")}
	lc, err := n.server.LocalClient()
	if err != nil {
		return identity{}, err
	}
	// Provision before reporting readiness, not lazily on the first browser request.
	if _, _, err := lc.CertPair(ctx, id.DNSName); err != nil {
		return identity{}, fmt.Errorf("provision HTTPS certificate: %w", err)
	}
	return id, nil
}

func (n *tsnetNetwork) Listen(ctx context.Context) (net.Listener, string, error) {
	id, err := n.connect(ctx)
	if err != nil {
		return nil, "", err
	}
	lc, err := n.server.LocalClient()
	if err != nil {
		return nil, "", err
	}
	getCertificate := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if !strings.EqualFold(strings.TrimSuffix(hello.ServerName, "."), id.DNSName) {
			return nil, fmt.Errorf("unexpected TLS server name")
		}
		certCtx, cancel := context.WithTimeout(hello.Context(), 30*time.Second)
		defer cancel()
		certPEM, keyPEM, err := lc.CertPair(certCtx, id.DNSName)
		if err != nil {
			return nil, err
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		return &cert, err
	}
	// tsnet.ListenTLS calls Up(context.Background()) a second time. Binding the
	// already-started node directly avoids an uncancellable second startup wait.
	n.getCertificate, n.dnsName = getCertificate, id.DNSName
	return n.ListenPort(ctx, 443)
}

func (n *tsnetNetwork) ListenPort(ctx context.Context, port int) (net.Listener, string, error) {
	ln, err := listenTLS(ctx, port, n.server.Listen, n.getCertificate)
	if err != nil {
		return nil, "", err
	}
	base := "https://" + n.dnsName
	if port != 443 {
		base += ":" + strconv.Itoa(port)
	}
	return ln, base, nil
}

func listenTLS(ctx context.Context, port int, listen func(string, string) (net.Listener, error), getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)) (net.Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ln, err := listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		ln.Close()
		return nil, err
	}
	return tls.NewListener(ln, &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: getCertificate}), nil
}

func (n *tsnetNetwork) Close() error { return n.server.Close() }
