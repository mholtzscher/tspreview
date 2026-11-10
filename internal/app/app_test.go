package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mholtzscher/tspreview/internal/preview"
)

// This loopback adapter only exists in tests. Production uses tsnet exclusively.
type loopbackNetwork struct{}

func (loopbackNetwork) Listen(context.Context) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	return ln, "http://" + ln.Addr().String(), nil
}
func (loopbackNetwork) Close() error { return nil }
func (n loopbackNetwork) ListenPort(ctx context.Context, _ int) (net.Listener, string, error) {
	return n.Listen(ctx)
}

func testConfig(t *testing.T) config {
	t.Helper()
	c := config{StateDir: t.TempDir(), Timeout: 10 * time.Second}
	if err := c.prepare(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(c.socket())) })
	return c
}

func awaitStatus(t *testing.T, ctx context.Context, c config, done <-chan error) status {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if s, err := newControlClient(c).status(ctx); err == nil {
			return s
		}
		select {
		case err := <-done:
			t.Fatalf("daemon exited before ready: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func startTestDaemon(t *testing.T, c config) (status, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, c, loopbackNetwork{}, io.Discard)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon failed to shut down")
		}
	})
	wait, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	return awaitStatus(t, wait, c, done), done
}

func cli(t *testing.T, c config, args ...string) string {
	t.Helper()
	var out, stderr bytes.Buffer
	args = append(args, "--state-dir", c.StateDir, "--timeout", "10s")
	if err := Run(context.Background(), args, &out, &stderr); err != nil {
		t.Fatalf("Run(%q): %v; stderr %s", args, err, stderr.String())
	}
	return out.String()
}

func fetch(t *testing.T, target string, code int, body string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != code || (body != "" && string(b) != body) {
		t.Fatalf("GET %s: %d %q, want %d %q", target, res.StatusCode, b, code, body)
	}
}

func TestCLIPublishListRemoveAndStop(t *testing.T) {
	c := testConfig(t)
	s, _ := startTestDaemon(t, c)
	file := filepath.Join(t.TempDir(), "hello #world.html")
	if err := os.WriteFile(file, []byte("<h1>Hello, world!</h1>"), 0600); err != nil {
		t.Fatal(err)
	}
	var p publication
	if err := json.Unmarshal([]byte(cli(t, c, file, "--ttl", "1h", "--json")), &p); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.URL, s.URL+"/p/"+p.ID+"/") || !strings.HasSuffix(p.URL, "hello%20%23world.html") || p.ExpiresAt.Sub(p.CreatedAt) != time.Hour {
		t.Fatalf("unexpected publication: %+v", p)
	}
	if err := os.WriteFile(file, []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	fetch(t, p.URL, 200, "<h1>Hello, world!</h1>")
	var listed []publication
	if err := json.Unmarshal([]byte(cli(t, c, "list", "--json")), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != p.ID || listed[0].URL != p.URL {
		t.Fatalf("list = %+v", listed)
	}
	var removed map[string]string
	if err := json.Unmarshal([]byte(cli(t, c, "rm", p.ID, "--json")), &removed); err != nil || removed["removed"] != p.ID {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	fetch(t, p.URL, 404, "")
	if got := cli(t, c, "list", "--json"); got != "[]\n" {
		t.Fatalf("empty list = %q", got)
	}
	if got := cli(t, c, "stop"); got != "Stopped\n" {
		t.Fatalf("stop output %q", got)
	}
	if _, err := newControlClient(c).status(context.Background()); err == nil {
		t.Fatal("stopped daemon still serves control requests")
	}
}

func TestDaemonRestartPreservesSnapshots(t *testing.T) {
	c := testConfig(t)
	startTestDaemon(t, c)
	file := filepath.Join(t.TempDir(), "a.html")
	if err := os.WriteFile(file, []byte("survives restart"), 0600); err != nil {
		t.Fatal(err)
	}
	var p publication
	if err := json.Unmarshal([]byte(cli(t, c, file, "--json")), &p); err != nil {
		t.Fatal(err)
	}
	cli(t, c, "stop")
	startTestDaemon(t, c)
	var listed []publication
	if err := json.Unmarshal([]byte(cli(t, c, "list", "--json")), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != p.ID {
		t.Fatalf("restart lost snapshot: %+v", listed)
	}
	fetch(t, listed[0].URL, 200, "survives restart")
}

func TestConcurrentClientsShareOneDetachedDaemon(t *testing.T) {
	c := testConfig(t)
	if err := c.saveIdentity(identity{Hostname: "fixture", DNSName: "fixture.test"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var launches atomic.Int32
	launch := func(c config) (*exec.Cmd, error) {
		launches.Add(1)
		return startProcess(c, os.Args[0], []string{"-test.run=^TestDaemonHelperProcess$", "--"})
	}
	const count = 8
	errs := make(chan error, count)
	pids := make(chan int, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := ensureDaemon(ctx, c, launch)
			if err != nil {
				errs <- err
				return
			}
			s, err := client.status(ctx)
			if err != nil {
				errs <- err
				return
			}
			pids <- s.PID
		}()
	}
	wg.Wait()
	close(errs)
	close(pids)
	t.Cleanup(func() { stop(context.Background(), c, false, io.Discard) })
	for err := range errs {
		t.Error(err)
	}
	seen := map[int]bool{}
	for pid := range pids {
		seen[pid] = true
		if pid == os.Getpid() {
			t.Error("daemon was not a detached subprocess")
		}
	}
	if len(seen) != 1 || launches.Load() != 1 {
		t.Fatalf("daemon PIDs %v, launches %d", seen, launches.Load())
	}
	file := filepath.Join(t.TempDir(), "concurrent.html")
	if err := os.WriteFile(file, []byte("parallel snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	publications := make(chan publication, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out bytes.Buffer
			if err := Run(ctx, []string{file, "--state-dir", c.StateDir, "--json"}, &out, io.Discard); err != nil {
				t.Error(err)
				return
			}
			var p publication
			if err := json.Unmarshal(out.Bytes(), &p); err != nil {
				t.Error(err)
				return
			}
			publications <- p
		}()
	}
	wg.Wait()
	close(publications)
	ids := map[string]bool{}
	for p := range publications {
		if ids[p.ID] {
			t.Errorf("duplicate ID %s", p.ID)
		}
		ids[p.ID] = true
		fetch(t, p.URL, 200, "parallel snapshot")
	}
	if len(ids) != count {
		t.Fatalf("published %d snapshots, want %d", len(ids), count)
	}
	if err := stop(ctx, c, false, io.Discard); err != nil {
		t.Fatal(err)
	}
}

// Invoked only by the subprocess test above, never by the production executable.
func TestDaemonHelperProcess(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--" {
			index = i
			break
		}
	}
	if index == -1 {
		return
	}
	args := os.Args[index+1:]
	if len(args) > 0 && args[0] == "__fail" {
		os.Exit(3)
	}
	if len(args) == 4 && args[0] == "__proxy" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := Run(ctx, []string{"proxy", args[3], "--state-dir", args[1], "--worktree", args[2], "--json"}, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	if len(args) == 3 && args[0] == "__publish" {
		c := config{StateDir: args[1], Timeout: 10 * time.Second}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		launch := func(c config) (*exec.Cmd, error) {
			return startProcess(c, os.Args[0], []string{"-test.run=^TestDaemonHelperProcess$", "--"})
		}
		client, err := ensureDaemon(ctx, c, launch)
		var p publication
		if err == nil {
			err = client.request(ctx, http.MethodPost, "/previews", publishRequest{Path: args[2], TTL: time.Hour}, &p)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		json.NewEncoder(os.Stdout).Encode(p)
		os.Exit(0)
	}
	if len(args) == 0 || args[0] != "__daemon" {
		os.Exit(2)
	}
	fs := flag.NewFlagSet("helper", flag.ExitOnError)
	var c config
	fs.StringVar(&c.StateDir, "state-dir", "", "")
	fs.DurationVar(&c.Timeout, "timeout", time.Second, "")
	fs.Parse(args[1:])
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runDaemon(ctx, c, loopbackNetwork{}, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestSeparatePublisherProcesses(t *testing.T) {
	c := testConfig(t)
	if err := c.saveIdentity(identity{Hostname: "fixture", DNSName: "fixture.test"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := stop(ctx, c, false, io.Discard); err != nil {
			t.Error(err)
		}
	})
	file := filepath.Join(t.TempDir(), "a.html")
	if err := os.WriteFile(file, []byte("from separate publishers"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const count = 6
	results := make(chan publication, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDaemonHelperProcess$", "--", "__publish", c.StateDir, file)
			out, err := cmd.Output()
			if err != nil {
				t.Errorf("publisher subprocess: %v", err)
				return
			}
			var p publication
			if err := json.Unmarshal(out, &p); err != nil {
				t.Errorf("publisher output %q: %v", out, err)
				return
			}
			results <- p
		}()
	}
	wg.Wait()
	close(results)
	ids, hosts := map[string]bool{}, map[string]bool{}
	for p := range results {
		ids[p.ID] = true
		hosts[strings.Split(p.URL, "/p/")[0]] = true
		// All publishing subprocesses have exited. The daemon and copies remain.
		fetch(t, p.URL, 200, "from separate publishers")
	}
	if len(ids) != count || len(hosts) != 1 {
		t.Fatalf("got %d IDs on %d hosts; want %d on one host", len(ids), len(hosts), count)
	}
	if st, err := os.Stat(c.socket()); err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("socket privacy: %v %v", st, err)
	}
}

func TestStartupFailureIsReportedAndReleasesLocks(t *testing.T) {
	c := testConfig(t)
	if err := c.saveIdentity(identity{Hostname: "fixture", DNSName: "fixture.test"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := ensureDaemon(ctx, c, func(c config) (*exec.Cmd, error) {
		return startProcess(c, os.Args[0], []string{"-test.run=^TestDaemonHelperProcess$", "--", "__fail"})
	})
	if err == nil || !strings.Contains(err.Error(), "exited during startup") || !strings.Contains(err.Error(), "daemon.log") {
		t.Fatalf("startup error: %v", err)
	}
	owner, err := lock(ctx, filepath.Join(c.StateDir, "launch.lock"), false)
	if err != nil {
		t.Fatalf("startup leaked launch lock: %v", err)
	}
	owner.Close()
	startTestDaemon(t, c)
	if _, err := newControlClient(c).status(ctx); err != nil {
		t.Fatalf("could not recover after failed startup: %v", err)
	}
}

func TestCLIValidationAndNoLogin(t *testing.T) {
	c := testConfig(t)
	file := filepath.Join(t.TempDir(), "a.html")
	if err := os.WriteFile(file, []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"list", "extra"}, "does not accept positional"},
		{[]string{"rm"}, "requires exactly one"},
		{[]string{file, "--ttl", "0s"}, "must be positive"},
		{[]string{file, "--timeout", "-1s"}, "must be positive"},
		{[]string{file, "--unknown"}, "unknown option"},
		{[]string{file, "--ttl"}, "requires a value"},
		{[]string{file, "--hostname", "other"}, "only supported by login"},
		{[]string{file}, "run tspreview login first"},
		{[]string{"login", "--hostname", "BAD NAME"}, "lowercase DNS label"},
	} {
		args := append([]string{"--state-dir", c.StateDir}, tc.args...)
		// Commands precede flags. A default publish accepts leading flags too.
		if tc.args[0] == "list" || tc.args[0] == "rm" || tc.args[0] == "login" {
			args = append(tc.args, "--state-dir", c.StateDir)
		}
		var out bytes.Buffer
		err := Run(context.Background(), args, &out, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) || out.Len() != 0 {
			t.Errorf("%q: got %v stdout %q; want %q", args, err, out.String(), tc.want)
		}
	}
	if got := cli(t, c, "stop", "--json"); got != "{\"stopped\":true}\n" {
		t.Fatalf("stopped state result %q", got)
	}
}

func TestLocksPrivacyAndContextCancellation(t *testing.T) {
	c := testConfig(t)
	path := filepath.Join(c.StateDir, "test.lock")
	f, err := lock(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := lock(context.Background(), path, false); !errors.Is(err, errLocked) {
		t.Fatalf("second owner: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lock(ctx, path, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lock wait: %v", err)
	}
	for _, path := range []string{c.StateDir, filepath.Dir(c.socket()), filepath.Join(c.StateDir, "node")} {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0700 {
			t.Fatalf("directory privacy: %s %v %v", path, st, err)
		}
	}
	link := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Symlink(c.StateDir, link); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(link); err == nil {
		t.Fatal("accepted symlink private directory")
	}
}

func TestHelpHasNoStateSideEffects(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"--help"}, &out, io.Discard); err != nil || !strings.Contains(out.String(), "tspreview login") {
		t.Fatalf("help: %v %s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "tspreview")); !os.IsNotExist(err) {
		t.Fatalf("help wrote state: %v", err)
	}
}

func TestCanceledPublishRequestDoesNotCommit(t *testing.T) {
	root := t.TempDir()
	store, err := preview.New(filepath.Join(root, "previews"))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "hello.html")
	if err := os.WriteFile(file, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(publishRequest{Path: file, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", "/previews", bytes.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	controlHandler(store, "https://fixture.test", func() {}).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || len(store.List()) != 0 {
		t.Fatalf("canceled request committed: status=%d list=%+v", w.Code, store.List())
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/previews", bytes.NewReader(body))
	controlHandler(store, "https://fixture.test", func() {}).ServeHTTP(w, r)
	if w.Code != http.StatusCreated || len(store.List()) != 1 {
		t.Fatalf("uncanceled publication failed: status=%d list=%+v", w.Code, store.List())
	}
}

func TestLostPublishResponseCanBeReconciledByListing(t *testing.T) {
	c := testConfig(t)
	store, err := preview.New(filepath.Join(c.StateDir, "previews"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", c.socket())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	h := controlHandler(store, "https://fixture.test", func() {})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/previews" {
			// Commit normally, but lose the connection before delivering the result.
			h.ServeHTTP(httptest.NewRecorder(), r)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		h.ServeHTTP(w, r)
	})}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	defer func() {
		srv.Close()
		<-done
	}()
	file := filepath.Join(t.TempDir(), "hello.html")
	if err := os.WriteFile(file, []byte("published before connection loss"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = Run(context.Background(), []string{file, "--state-dir", c.StateDir}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "outcome is unknown") || !strings.Contains(err.Error(), "list --json") || out.Len() != 0 {
		t.Fatalf("ambiguous publish: err=%v stdout=%q", err, out.String())
	}
	var items []publication
	if err := json.Unmarshal([]byte(cli(t, c, "list", "--json")), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "hello.html" || !strings.HasSuffix(items[0].URL, "/hello.html") {
		t.Fatalf("could not reconcile committed publication: %+v", items)
	}
}
