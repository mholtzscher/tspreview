package app

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mholtzscher/tspreview/internal/proxy"
)

const proxyPortMin, proxyPortCount = 10000, 50000

var (
	errProxyConflict = errors.New("this worktree/app already has a live proxy; stop its foreground process first")
	appNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	proxyIDPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type proxyRequest struct {
	Worktree string        `json:"worktree"`
	Name     string        `json:"name"`
	Upstream string        `json:"upstream"`
	Routes   []proxy.Route `json:"routes,omitempty"`
}

type proxyRegistration struct {
	proxyRequest
	ID   string `json:"id"`
	Port int    `json:"port"`
	URL  string `json:"url"`
}

func proxyIdentity(req proxyRequest) (string, error) {
	if !filepath.IsAbs(req.Worktree) || filepath.Clean(req.Worktree) != req.Worktree {
		return "", fmt.Errorf("worktree must be a clean absolute path")
	}
	if !appNamePattern.MatchString(req.Name) {
		return "", fmt.Errorf("app name must be 1–64 letters, digits, dots, underscores, or hyphens, starting with a letter or digit")
	}
	hash := sha256.Sum256([]byte(req.Worktree + "\x00" + req.Name))
	return hex.EncodeToString(hash[:]), nil
}

type proxyManager struct {
	mu          sync.Mutex
	assignments map[string]int
	active      map[string]*activeProxy
	closed      bool
	path        string
	node        network
	logger      *log.Logger
}

type activeProxy struct {
	registration proxyRegistration
	server       *http.Server
	connections  *proxyConnections
	handler      *proxy.Handler
	done         chan error
}

func newProxyManager(c config, node network, logger *log.Logger) (*proxyManager, error) {
	m := &proxyManager{
		assignments: map[string]int{}, active: map[string]*activeProxy{},
		path: filepath.Join(c.StateDir, "proxy-ports.json"), node: node, logger: logger,
	}
	b, err := os.ReadFile(m.path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m.assignments); err != nil {
		return nil, fmt.Errorf("read proxy port assignments: %w", err)
	}
	if m.assignments == nil {
		return nil, fmt.Errorf("invalid proxy port assignments: expected an object")
	}
	used := map[int]bool{}
	for id, port := range m.assignments {
		if !proxyIDPattern.MatchString(id) || port < proxyPortMin || port >= proxyPortMin+proxyPortCount || port == 10080 || used[port] {
			return nil, fmt.Errorf("invalid or duplicate proxy port assignment")
		}
		used[port] = true
	}
	return m, nil
}

func (m *proxyManager) register(ctx context.Context, req proxyRequest) (*activeProxy, error) {
	id, err := proxyIdentity(req)
	if err != nil {
		return nil, err
	}
	handler, err := proxy.New(req.Upstream, req.Routes, m.logger)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("proxy server is stopping")
	}
	if m.active[id] != nil {
		return nil, errProxyConflict
	}
	port, saved := m.assignments[id]
	if !saved {
		// Fetch blocks 10080 in browsers; other blocked ports are below our range.
		// https://fetch.spec.whatwg.org/#port-blocking
		used := map[int]bool{10080: true}
		for _, assigned := range m.assignments {
			used[assigned] = true
		}
		hash := sha256.Sum256([]byte(id))
		candidate := int(binary.BigEndian.Uint32(hash[:4]) % proxyPortCount)
		for offset := range proxyPortCount {
			p := proxyPortMin + (candidate+offset)%proxyPortCount
			if !used[p] {
				port = p
				break
			}
		}
		if port == 0 {
			return nil, fmt.Errorf("all proxy ports are assigned")
		}
	}
	ln, base, err := m.node.ListenPort(ctx, port)
	if err != nil {
		return nil, fmt.Errorf("listen on tailnet port %d: %w", port, err)
	}
	if !saved {
		m.assignments[id] = port
		b, err := json.MarshalIndent(m.assignments, "", "  ")
		if err == nil {
			err = os.WriteFile(m.path+".tmp", append(b, '\n'), 0600)
		}
		if err == nil {
			err = os.Rename(m.path+".tmp", m.path)
		}
		if err != nil {
			delete(m.assignments, id)
			ln.Close()
			return nil, fmt.Errorf("save proxy port assignment: %w", err)
		}
	}
	connections := &proxyConnections{connections: map[net.Conn]http.ConnState{}}
	p := &activeProxy{
		registration: proxyRegistration{proxyRequest: req, ID: id, Port: port, URL: base},
		connections:  connections,
		handler:      handler, done: make(chan error, 1),
		server: &http.Server{Handler: connections.handler(handler), ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout: time.Minute, ErrorLog: m.logger, ConnState: connections.state,
			ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
				return context.WithValue(ctx, proxyConnectionKey{}, conn)
			}},
	}
	m.active[id] = p
	go func() { p.done <- p.server.Serve(ln) }()
	return p, nil
}

func (m *proxyManager) release(p *activeProxy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active[p.registration.ID] != p {
		return
	}
	p.close()
	delete(m.active, p.registration.ID)
}

func (p *activeProxy) close() {
	p.server.Close()
	p.connections.close() // Includes hijacked WebSocket connections.
	p.handler.Close()
}

func (m *proxyManager) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, p := range m.active {
		p.close()
	}
	clear(m.active)
}

func (m *proxyManager) list() []proxyRegistration {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]proxyRegistration, 0, len(m.active))
	for _, p := range m.active {
		items = append(items, p.registration)
	}
	slices.SortFunc(items, func(a, b proxyRegistration) int {
		if order := strings.Compare(a.Worktree, b.Worktree); order != 0 {
			return order
		}
		return strings.Compare(a.Name, b.Name)
	})
	return items
}

// http.Server.Close excludes hijacked sockets. Keep those until their handler
// returns, without wrapping net.Conn (which would hide *tls.Conn from net/http).
type proxyConnections struct {
	mu          sync.Mutex
	connections map[net.Conn]http.ConnState
	closed      bool
}

type proxyConnectionKey struct{}

func (c *proxyConnections) state(conn net.Conn, state http.ConnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		conn.Close()
		return
	}
	if state == http.StateClosed {
		delete(c.connections, conn)
	} else {
		c.connections[conn] = state
	}
}

func (c *proxyConnections) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			conn, _ := r.Context().Value(proxyConnectionKey{}).(net.Conn)
			c.mu.Lock()
			if c.connections[conn] == http.StateHijacked {
				delete(c.connections, conn)
			}
			c.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

func (c *proxyConnections) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for conn := range c.connections {
		conn.Close()
	}
	clear(c.connections)
}
