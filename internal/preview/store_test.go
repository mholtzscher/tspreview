package preview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func request(t *testing.T, s *Store, method, target string, status int, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(method, target, nil))
	if w.Code != status {
		t.Fatalf("%s %s: got status %d body %q, want %d", method, target, w.Code, w.Body.String(), status)
	}
	if body != "" && w.Body.String() != body {
		t.Fatalf("%s: got body %q, want %q", target, w.Body.String(), body)
	}
	for key, expected := range map[string]string{"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer"} {
		if got := w.Header().Get(key); got != expected {
			t.Errorf("%s: %s = %q, want %q", target, key, got, expected)
		}
	}
	return w
}

func TestFileSnapshotRestartAndRemoval(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state")
	file := filepath.Join(root, "hello world.html")
	write(t, file, "original")
	s := openStore(t, dir)
	p, err := s.Publish(context.Background(), file, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "hello world.html" || p.Entry != "hello world.html" || len(p.ID) != 32 || !p.ExpiresAt.Equal(p.CreatedAt.Add(time.Hour)) {
		t.Fatalf("unexpected preview: %+v", p)
	}
	write(t, file, "changed")
	base := "/p/" + p.ID + "/"
	w := request(t, s, "GET", base+"?x=1", http.StatusMovedPermanently, "")
	if got := w.Header().Get("Location"); got != base+"hello%20world.html?x=1" {
		t.Fatalf("redirect = %q", got)
	}
	request(t, s, "GET", base+"hello%20world.html", 200, "original")
	request(t, s, "HEAD", base+"hello%20world.html", 200, "")
	request(t, s, "POST", base, 405, "")
	s = openStore(t, dir)
	if got := s.List(); len(got) != 1 || got[0].ID != p.ID || got[0].Name != p.Name || got[0].Entry != p.Entry || !got[0].CreatedAt.Equal(p.CreatedAt) || !got[0].ExpiresAt.Equal(p.ExpiresAt) {
		t.Fatalf("after restart: %+v, want %+v", got, p)
	}
	request(t, s, "GET", base+"hello%20world.html", 200, "original")
	if err := s.Remove(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
	request(t, s, "GET", base+"hello%20world.html", 404, "")
	if got := openStore(t, dir).List(); len(got) != 0 {
		t.Fatalf("after removal and restart: %+v", got)
	}
	for _, path := range []string{dir, filepath.Join(dir, "data"), filepath.Join(dir, "meta"), filepath.Join(dir, "staging")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("private directory %s: %v %v", path, info, err)
		}
	}
}

func TestDirectoryAndContainment(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "site")
	write(t, filepath.Join(source, "index.html"), "home")
	write(t, filepath.Join(source, "assets", "app.js"), "asset")
	write(t, filepath.Join(source, "nested", "index.html"), "nested")
	write(t, filepath.Join(source, "empty", "a.txt"), "not an index")
	write(t, filepath.Join(source, ".secret"), "SECRET")
	write(t, filepath.Join(source, ".private", "index.html"), "SECRET")
	s := openStore(t, filepath.Join(root, "state"))
	p, err := s.Publish(context.Background(), source, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if p.Entry != "" {
		t.Fatalf("directory entry = %q", p.Entry)
	}
	base := "/p/" + p.ID + "/"
	request(t, s, "GET", base, 200, "home")
	request(t, s, "GET", base+"assets/app.js", 200, "asset")
	request(t, s, "GET", base+"nested/", 200, "nested")
	if got := request(t, s, "GET", base+"nested?x=1", 301, "").Header().Get("Location"); got != base+"nested/?x=1" {
		t.Fatalf("directory redirect = %q", got)
	}
	if got := request(t, s, "GET", strings.TrimSuffix(base, "/")+"?x=1", 301, "").Header().Get("Location"); got != base+"?x=1" {
		t.Fatalf("root redirect = %q", got)
	}
	for _, target := range []string{
		"/", "/p/", "/p/0123456789abcdef0123456789abcdef/", base + "missing", base + "empty/", base + "empty", base + ".secret", base + ".private/index.html",
		base + "../" + p.ID + "/index.html", base + "%2e%2e/" + p.ID + "/index.html", base + "%2e%2e%2f" + p.ID + "/index.html",
		base + "%2e%2e%2f%2e%2e%2fmeta%2f" + p.ID + ".json", base + "%5c..%5cmeta", base + "assets//app.js",
	} {
		request(t, s, "GET", target, 404, "")
	}
	otherSource := filepath.Join(root, "other.html")
	write(t, otherSource, "other secret")
	other, err := s.Publish(context.Background(), otherSource, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request(t, s, "GET", base+"%2e%2e/"+other.ID+"/other.html", 404, "")
	// Even if the private snapshot is tampered with after publication, its
	// filesystem root cannot follow links into metadata or another preview.
	for name, target := range map[string]string{
		"metadata-link": filepath.Join(s.dir, "meta", p.ID+".json"),
		"other-link":    filepath.Join("..", other.ID, "other.html"),
	} {
		if err := os.Symlink(target, filepath.Join(s.dir, "data", p.ID, name)); err != nil {
			t.Fatal(err)
		}
		request(t, s, "GET", base+name, 404, "")
	}
	for _, path := range []string{filepath.Join(s.dir, "meta", p.ID+".json"), filepath.Join(s.dir, "data", p.ID, "index.html")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private file %s: %v %v", path, info, err)
		}
	}
}

func TestExpiryAndCrashCleanup(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.html")
	write(t, file, "a")
	dir := filepath.Join(root, "state")
	s := openStore(t, dir)
	now := time.Now().Add(-2 * time.Hour)
	s.now = func() time.Time { return now }
	p, err := s.Publish(context.Background(), file, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if got := s.List(); len(got) != 0 {
		t.Fatalf("expired list: %+v", got)
	}
	request(t, s, "GET", "/p/"+p.ID+"/a.html", 404, "")
	if err := s.Prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", p.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired data remains: %v", err)
	}
	s.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	p, err = s.Publish(context.Background(), file, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "staging", "interrupted", "partial"), "partial")
	write(t, filepath.Join(dir, "data", "orphan", "partial"), "partial")
	s = openStore(t, dir)
	if got := s.List(); len(got) != 0 {
		t.Fatalf("startup expiry: %+v", got)
	}
	for _, path := range []string{filepath.Join(dir, "data", p.ID), filepath.Join(dir, "data", "orphan"), filepath.Join(dir, "staging", "interrupted")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cleanup %s: %v", path, err)
		}
	}
}

func TestRejectUnsafeSourcesAndRollback(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, filepath.Join(root, "state"))
	secret := filepath.Join(root, ".secret")
	write(t, secret, "secret")
	source := filepath.Join(root, "site")
	write(t, filepath.Join(source, "index.html"), "ok")
	link := filepath.Join(source, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{secret, link, source} {
		if _, err := s.Publish(context.Background(), src, time.Hour); err == nil {
			t.Errorf("published unsafe source %s", src)
		}
	}
	if _, err := s.Publish(context.Background(), source, 0); err == nil {
		t.Error("accepted zero TTL")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(link, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(context.Background(), source, time.Hour); err == nil {
		t.Fatal("published FIFO")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(source, "large")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(context.Background(), source, time.Hour); err == nil || !strings.Contains(err.Error(), "100 MiB") {
		t.Fatalf("oversize error: %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatalf("failed publication left registry entries: %+v", s.List())
	}
	for _, sub := range []string{"data", "meta", "staging"} {
		entries, err := os.ReadDir(filepath.Join(s.dir, sub))
		if err != nil || len(entries) != 0 {
			t.Fatalf("failed publication left %s: %v %v", sub, entries, err)
		}
	}
	if err := os.Remove(large); err != nil {
		t.Fatal(err)
	}
	p, err := s.Publish(context.Background(), source, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request(t, s, "GET", "/p/"+p.ID+"/", 200, "ok")
}

func TestMetadataCommitFailureRollsBackData(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, filepath.Join(root, "state"))
	file := filepath.Join(root, "a.html")
	write(t, file, "a")
	meta := filepath.Join(s.dir, "meta")
	if err := os.Rename(meta, meta+"-saved"); err != nil {
		t.Fatal(err)
	}
	write(t, meta, "not a directory")
	if _, err := s.Publish(context.Background(), file, time.Hour); err == nil {
		t.Fatal("publication succeeded without a metadata directory")
	}
	if len(s.List()) != 0 {
		t.Fatal("failed publication became visible")
	}
	for _, sub := range []string{"data", "staging"} {
		entries, err := os.ReadDir(filepath.Join(s.dir, sub))
		if err != nil || len(entries) != 0 {
			t.Fatalf("rollback left %s: %v %v", sub, entries, err)
		}
	}
	if err := os.Remove(meta); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(meta+"-saved", meta); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(context.Background(), file, time.Hour); err != nil {
		t.Fatalf("publish after recovery: %v", err)
	}
}

func TestFileCountLimit(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "many")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxFiles; i++ {
		write(t, filepath.Join(source, fmt.Sprintf("%05d", i)), "")
	}
	s := openStore(t, filepath.Join(root, "state"))
	if _, err := s.Publish(context.Background(), source, time.Hour); err == nil || !strings.Contains(err.Error(), "10000 files") {
		t.Fatalf("file count error: %v", err)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("failed publication visible: %+v", got)
	}
}

func TestConcurrentOperations(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.html")
	write(t, file, "contents")
	s := openStore(t, filepath.Join(root, "state"))
	const count = 32
	results := make(chan Preview, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := s.Publish(context.Background(), file, time.Hour)
			if err != nil {
				errs <- err
				return
			}
			results <- p
			s.List()
			if err := s.Prune(); err != nil {
				errs <- err
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/p/"+p.ID+"/a.html", nil))
			if w.Code != 200 || w.Body.String() != "contents" {
				errs <- fmt.Errorf("HTTP status %d body %q", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	seen := make(map[string]bool)
	for p := range results {
		if seen[p.ID] {
			t.Errorf("duplicate ID %s", p.ID)
		}
		seen[p.ID] = true
	}
	if len(seen) != count || len(s.List()) != count {
		t.Fatalf("published %d, listed %d; want %d", len(seen), len(s.List()), count)
	}
	for id := range seen {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Remove(id); err != nil {
				t.Error(err)
			}
			s.List()
		}()
	}
	wg.Wait()
	if got := s.List(); len(got) != 0 {
		t.Fatalf("after concurrent removals: %+v", got)
	}
}

func TestHiddenUnsafeEntriesAreSkipped(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "site")
	write(t, filepath.Join(source, "index.html"), "visible")
	secret := filepath.Join(root, "secret")
	write(t, secret, "must not publish")
	if err := os.Symlink(secret, filepath.Join(source, ".link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(source, ".pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, filepath.Join(root, "state"))
	p, err := s.Publish(context.Background(), source, time.Hour)
	if err != nil {
		t.Fatalf("hidden entries should be skipped: %v", err)
	}
	request(t, s, "GET", "/p/"+p.ID+"/", 200, "visible")
	for _, name := range []string{".link", ".pipe"} {
		request(t, s, "GET", "/p/"+p.ID+"/"+name, 404, "")
	}
}

func TestCancellationBeforeCommitLeavesNoSnapshot(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, filepath.Join(root, "state"))
	file := filepath.Join(root, "hello.html")
	write(t, file, "fully copied but not committed")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.now = func() time.Time {
		cancel() // Publish reads the clock after copying, just before commit.
		return time.Now()
	}
	if _, err := s.Publish(ctx, file, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled publication = %v", err)
	}
	for _, sub := range []string{"data", "meta", "staging"} {
		entries, err := os.ReadDir(filepath.Join(s.dir, sub))
		if err != nil || len(entries) != 0 {
			t.Fatalf("canceled publication left %s: %v %v", sub, entries, err)
		}
	}
	if len(s.List()) != 0 {
		t.Fatal("canceled publication became visible")
	}
	s.now = time.Now
	if _, err := s.Publish(context.Background(), file, time.Hour); err != nil {
		t.Fatalf("publication after cancellation: %v", err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(b []byte) (int, error) { return f(b) }

func TestCopyStopsReadingAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	source := readerFunc(func(b []byte) (int, error) {
		reads++
		cancel()
		return copy(b, "first chunk"), nil
	})
	var out bytes.Buffer
	n, err := io.Copy(&out, contextReader{ctx: ctx, Reader: source})
	if !errors.Is(err, context.Canceled) || n != 11 || reads != 1 || out.String() != "first chunk" {
		t.Fatalf("copy n=%d reads=%d out=%q err=%v", n, reads, out.String(), err)
	}
}
