package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func waitProxyCount(t *testing.T, c config, count int) []proxyRegistration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var items []proxyRegistration
		if err := newControlClient(c).request(ctx, http.MethodGet, "/proxies", nil, &items); err != nil {
			t.Fatal(err)
		}
		if len(items) == count {
			return items
		}
		select {
		case <-ctx.Done():
			t.Fatalf("got %d proxies, want %d", len(items), count)
		case <-ticker.C:
		}
	}
}

func startProxyCLI(t *testing.T, c config, worktree, upstream string, extra ...string) (proxyRegistration, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	args := append([]string{"proxy", upstream, "--worktree", worktree, "--state-dir", c.StateDir, "--timeout", "1s", "--json"}, extra...)
	go func() { err := Run(ctx, args, writer, io.Discard); writer.CloseWithError(err); done <- err }()
	t.Cleanup(func() { cancel(); reader.Close() })
	var p proxyRegistration
	if err := json.NewDecoder(reader).Decode(&p); err != nil {
		t.Fatal(err)
	}
	return p, cancel, done
}

func TestProxyCLILeasesIsolateWorktreesAndPreserveSnapshots(t *testing.T) {
	c := testConfig(t)
	startTestDaemon(t, c)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "live app") }))
	defer upstream.Close()
	firstRoot, secondRoot := t.TempDir(), t.TempDir()
	first, stopFirst, firstDone := startProxyCLI(t, c, firstRoot, upstream.URL)
	second, stopSecond, secondDone := startProxyCLI(t, c, secondRoot, upstream.URL)
	if first.ID == second.ID || first.Port == second.Port || first.URL == second.URL {
		t.Fatalf("worktree collision: %+v %+v", first, second)
	}
	fetch(t, first.URL+"/assets/root-relative.js", 200, "live app")
	fetch(t, second.URL, 200, "live app")
	var listed []proxyRegistration
	if err := json.Unmarshal([]byte(cli(t, c, "proxies", "--json")), &listed); err != nil || len(listed) != 2 {
		t.Fatalf("proxies = %+v, %v", listed, err)
	}
	if got := cli(t, c, "proxies"); !strings.Contains(got, first.URL) || !strings.Contains(got, secondRoot) {
		t.Fatalf("proxy listing: %s", got)
	}
	file := filepath.Join(t.TempDir(), "still-static.html")
	if err := os.WriteFile(file, []byte("snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	var snapshot publication
	if err := json.Unmarshal([]byte(cli(t, c, file, "--json")), &snapshot); err != nil {
		t.Fatal(err)
	}
	stopFirst()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	remaining := waitProxyCount(t, c, 1)
	if remaining[0].ID != second.ID {
		t.Fatal("stopped another worktree's proxy")
	}
	fetch(t, second.URL, 200, "live app")
	fetch(t, snapshot.URL, 200, "snapshot")
	client := &http.Client{Timeout: time.Second}
	if res, err := client.Get(first.URL); err == nil {
		res.Body.Close()
		t.Fatal("released proxy still listening")
	}
	stopSecond()
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	waitProxyCount(t, c, 0)
}

func TestProxyDuplicateConflictAndUpstreamChange(t *testing.T) {
	c := testConfig(t)
	startTestDaemon(t, c)
	root := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "original") }))
	defer upstream.Close()
	p, cancel, done := startProxyCLI(t, c, root, upstream.URL)
	var output strings.Builder
	err := Run(context.Background(), []string{"proxy", upstream.URL, "--worktree", root, "--state-dir", c.StateDir}, &output, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "already has a live proxy") || output.Len() != 0 {
		t.Fatalf("duplicate: %v %q", err, output.String())
	}
	fetch(t, p.URL, 200, "original")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitProxyCount(t, c, 0)
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "replacement") }))
	defer replacement.Close()
	updated, stop, updatedDone := startProxyCLI(t, c, root, replacement.URL)
	if updated.ID != p.ID || updated.Port != p.Port {
		t.Fatalf("upstream change lost assignment: %+v %+v", p, updated)
	}
	fetch(t, updated.URL, 200, "replacement")
	stop()
	if err := <-updatedDone; err != nil {
		t.Fatal(err)
	}
}

func TestProxyDaemonRestartAndUnexpectedDisconnect(t *testing.T) {
	c := testConfig(t)
	startTestDaemon(t, c)
	root := t.TempDir()
	p, _, done := startProxyCLI(t, c, root, "http://127.0.0.1:1")
	fetch(t, p.URL, 502, "upstream unavailable\n")
	cli(t, c, "stop")
	if err := <-done; err == nil || !strings.Contains(err.Error(), "proxy lease lost") {
		t.Fatalf("unexpected daemon stop: %v", err)
	}
	startTestDaemon(t, c)
	if got := cli(t, c, "proxies", "--json"); got != "[]\n" {
		t.Fatalf("restart resurrected lease: %q", got)
	}
	newProxy, cancel, newDone := startProxyCLI(t, c, root, "http://127.0.0.1:2")
	if newProxy.ID != p.ID || newProxy.Port != p.Port {
		t.Fatalf("restart lost identity/port: %+v %+v", p, newProxy)
	}
	cancel()
	if err := <-newDone; err != nil {
		t.Fatal(err)
	}
}

func TestProxyCancellationClosesWebSocket(t *testing.T) {
	c := testConfig(t)
	startTestDaemon(t, c)
	upstreamClosed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		defer close(upstreamClosed)
		for {
			kind, body, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), kind, body); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	p, cancel, done := startProxyCLI(t, c, t.TempDir(), upstream.URL)
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	conn, _, err := websocket.Dial(ctx, p.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageText, []byte("worktree socket")); err != nil {
		t.Fatal(err)
	}
	_, body, err := conn.Read(ctx)
	if err != nil || string(body) != "worktree socket" {
		t.Fatalf("echo %q %v", body, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitProxyCount(t, c, 0)
	if _, _, err := conn.Read(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("socket not closed promptly: %v", err)
	}
	select {
	case <-upstreamClosed:
	case <-ctx.Done():
		t.Fatal("upstream WebSocket leaked")
	}
}

func TestConcurrentProxyAssignments(t *testing.T) {
	c := testConfig(t)
	startTestDaemon(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const count = 8
	registrations := make(chan proxyRegistration, count)
	var wg sync.WaitGroup
	root := t.TempDir()
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, registration, err := newControlClient(c).openProxy(ctx, ctx, proxyRequest{Worktree: root, Name: fmt.Sprintf("app-%d", i), Upstream: "http://127.0.0.1:1"})
			if err != nil {
				t.Error(err)
				return
			}
			defer lease.Close()
			registrations <- registration
			<-ctx.Done()
		}()
	}
	ports := map[int]bool{}
	for range count {
		select {
		case p := <-registrations:
			if ports[p.Port] {
				t.Errorf("duplicate assigned port %d", p.Port)
			}
			ports[p.Port] = true
		case <-ctx.Done():
			t.Fatal("concurrent registration timeout")
		}
	}
	if len(waitProxyCount(t, c, count)) != count {
		t.Fatal("lost registration")
	}
	cancel()
	wg.Wait()
	waitProxyCount(t, c, 0)
}

func TestProxyAssignmentsResolveCollisionAndPersist(t *testing.T) {
	c := testConfig(t)
	logger := log.New(io.Discard, "", 0)
	m, err := newProxyManager(c, loopbackNetwork{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	req := proxyRequest{Worktree: t.TempDir(), Name: "web", Upstream: "http://127.0.0.1:1"}
	p, err := m.register(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	candidate := p.registration.Port
	m.release(p)
	// Reserve this identity's deterministic candidate for another identity.
	id, _ := proxyIdentity(req)
	delete(m.assignments, id)
	m.assignments[strings.Repeat("a", 64)] = candidate
	p, err = m.register(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if p.registration.Port == candidate {
		t.Fatal("did not resolve saved port collision")
	}
	m.release(p)
	reloaded, err := newProxyManager(c, loopbackNetwork{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.close()
	again, err := reloaded.register(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if again.registration.Port != p.registration.Port {
		t.Fatal("collision resolution was not persisted")
	}
}

func TestProxyValidationAndWorktreeResolution(t *testing.T) {
	c := testConfig(t)
	for _, args := range [][]string{
		{"proxy"}, {"proxy", "http://127.0.0.1:3000", "--ttl", "1h"},
		{"proxy", "http://example.com"}, {"proxy", "http://127.0.0.1:3000", "--name", "../bad"},
		{"proxy", "http://127.0.0.1:3000", "--route", "bad"},
		{"proxy", "http://127.0.0.1:3000", "--route", "/=http://127.0.0.1:4000"},
		{"proxy", "http://127.0.0.1:3000", "--timeout", "0s"},
	} {
		var output strings.Builder
		err := Run(context.Background(), append(args, "--state-dir", c.StateDir), &output, io.Discard)
		if err == nil || output.Len() != 0 {
			t.Errorf("accepted %q: %v %q", args, err, output.String())
		}
	}
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "worktree-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveWorktree(context.Background(), link)
	if err != nil || resolved != root {
		t.Fatalf("canonical root %q: %v", resolved, err)
	}
	// The default resolves from a nested directory to the Git worktree root.
	cmd := exec.Command("git", "init", "--quiet", root)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	resolved, err = resolveWorktree(context.Background(), "")
	if err != nil || resolved != root {
		t.Fatalf("Git root %q: %v", resolved, err)
	}
}

func TestForegroundProxyProcessExitReleasesLease(t *testing.T) {
	c := testConfig(t)
	startTestDaemon(t, c)
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		t.Run(signal.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDaemonHelperProcess$", "--", "__proxy", c.StateDir, t.TempDir(), "http://127.0.0.1:1")
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cmd.Process.Kill() })
			var p proxyRegistration
			if err := json.NewDecoder(out).Decode(&p); err != nil {
				t.Fatal(err)
			}
			waitProxyCount(t, c, 1)
			if err := cmd.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			if signal == syscall.SIGTERM && err != nil {
				t.Fatalf("graceful exit: %v", err)
			}
			if signal == syscall.SIGKILL && err == nil {
				t.Fatal("killed process reported success")
			}
			waitProxyCount(t, c, 0)
		})
	}
}

func TestProxyLeaseOutlivesStartupDeadline(t *testing.T) {
	c := testConfig(t)
	startTestDaemon(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startup, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	lease, registration, err := newControlClient(c).openProxy(ctx, startup, proxyRequest{Worktree: t.TempDir(), Name: "web", Upstream: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	<-startup.Done()
	// A per-command startup deadline must not cancel the established lease.
	fetch(t, registration.URL, 502, "upstream unavailable\n")
	waitProxyCount(t, c, 1)
}

func TestProxyPortStateValidation(t *testing.T) {
	for _, content := range []string{
		"invalid JSON", "null", `{"not-an-id":12345}`,
		fmt.Sprintf(`{%q:443}`, strings.Repeat("a", 64)),
		fmt.Sprintf(`{%q:12345,%q:12345}`, strings.Repeat("a", 64), strings.Repeat("b", 64)),
	} {
		c := testConfig(t)
		if err := os.WriteFile(filepath.Join(c.StateDir, "proxy-ports.json"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newProxyManager(c, loopbackNetwork{}, log.Default()); err == nil {
			t.Errorf("accepted invalid port state %q", content)
		}
	}
}
