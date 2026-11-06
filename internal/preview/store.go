// Package preview stores immutable, expiring snapshots for HTTP publication.
package preview

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxBytes = 100 << 20
	maxFiles = 10000
)

var ErrNotFound = errors.New("preview not found")

type Preview struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Entry     string    `json:"entry"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Store is owned by one daemon. Its methods are safe for concurrent use.
type Store struct {
	dir      string
	mu       sync.RWMutex
	previews map[string]Preview
	now      func() time.Time
}

// New opens a private store, removing interrupted publications and expired data.
// Only one Store may own a given directory at a time.
func New(dir string) (*Store, error) {
	for _, path := range []string{dir, filepath.Join(dir, "data"), filepath.Join(dir, "meta"), filepath.Join(dir, "staging")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0700); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: dir, previews: make(map[string]Preview), now: time.Now}
	entries, err := os.ReadDir(filepath.Join(dir, "staging"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, "staging", e.Name())); err != nil {
			return nil, err
		}
	}
	entries, err = os.ReadDir(filepath.Join(dir, "meta"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".json")
		if !validID(id) || e.Name() != id+".json" || !e.Type().IsRegular() {
			return nil, fmt.Errorf("invalid preview metadata: %s", e.Name())
		}
		b, err := os.ReadFile(filepath.Join(dir, "meta", e.Name()))
		if err != nil {
			return nil, err
		}
		var p Preview
		if err := json.Unmarshal(b, &p); err != nil || p.ID != id || p.Name == "" || !validEntry(p.Entry) || p.CreatedAt.IsZero() || !p.ExpiresAt.After(p.CreatedAt) {
			return nil, fmt.Errorf("invalid preview metadata: %s", e.Name())
		}
		info, err := os.Lstat(filepath.Join(dir, "data", id))
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("missing preview data for %s", id)
		}
		s.previews[id] = p
	}
	if err := s.Prune(); err != nil {
		return nil, err
	}
	// A crash after data rename but before metadata rename can leave an orphan.
	entries, err = os.ReadDir(filepath.Join(dir, "data"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if _, ok := s.previews[e.Name()]; !ok {
			if err := os.RemoveAll(filepath.Join(dir, "data", e.Name())); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validEntry(entry string) bool {
	return entry == "" || (entry != "." && entry != ".." && !strings.HasPrefix(entry, ".") && !strings.ContainsAny(entry, "/\\\x00"))
}

// Publish copies a regular file or directory into a new immutable snapshot.
// Cancellation aborts copying and is checked before commit. Cancellation after
// commit starts may still leave a successful publication; reconcile a lost
// response using List rather than assuming a retry cannot create a second copy.
func (s *Store) Publish(ctx context.Context, source string, ttl time.Duration) (Preview, error) {
	if err := ctx.Err(); err != nil {
		return Preview{}, err
	}
	if ttl <= 0 {
		return Preview{}, errors.New("preview TTL must be positive")
	}
	abs, err := filepath.Abs(source)
	if err != nil {
		return Preview{}, err
	}
	name := filepath.Base(abs)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return Preview{}, fmt.Errorf("cannot publish invalid source %q", name)
	}
	parent, err := os.OpenRoot(filepath.Dir(abs))
	if err != nil {
		return Preview{}, err
	}
	defer parent.Close()
	info, err := parent.Lstat(name)
	if err != nil {
		return Preview{}, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return Preview{}, fmt.Errorf("source %q is a symlink or special file", source)
	}
	if info.Mode().IsRegular() && !validEntry(name) {
		return Preview{}, fmt.Errorf("cannot publish hidden file %q", name)
	}
	stage, err := os.MkdirTemp(filepath.Join(s.dir, "staging"), "snapshot-")
	if err != nil {
		return Preview{}, err
	}
	defer os.RemoveAll(stage)
	data := filepath.Join(stage, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		return Preview{}, err
	}
	c := copier{ctx: ctx}
	entry := ""
	if info.IsDir() {
		r, err := parent.OpenRoot(name)
		if err != nil {
			return Preview{}, err
		}
		defer r.Close()
		opened, err := r.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			return Preview{}, errors.New("source directory changed during publication")
		}
		if err := c.directory(r, ".", data); err != nil {
			return Preview{}, err
		}
	} else {
		entry = name
		if err := c.file(parent, name, info, filepath.Join(data, name)); err != nil {
			return Preview{}, err
		}
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Preview{}, err
	}
	id := hex.EncodeToString(random[:])
	now := s.now()
	p := Preview{ID: id, Name: name, Entry: entry, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	b, err := json.Marshal(p)
	if err != nil {
		return Preview{}, err
	}
	meta := filepath.Join(stage, "meta.json")
	if err := os.WriteFile(meta, b, 0600); err != nil {
		return Preview{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Preview{}, err
	}
	if _, exists := s.previews[id]; exists {
		return Preview{}, errors.New("random preview ID collision")
	}
	dest := filepath.Join(s.dir, "data", id)
	if _, err := os.Lstat(dest); err == nil {
		return Preview{}, errors.New("random preview ID collision")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Preview{}, err
	}
	if err := os.Rename(data, dest); err != nil {
		return Preview{}, err
	}
	if err := os.Rename(meta, filepath.Join(s.dir, "meta", id+".json")); err != nil {
		if cleanupErr := os.RemoveAll(dest); cleanupErr != nil {
			return Preview{}, errors.Join(err, cleanupErr)
		}
		return Preview{}, err
	}
	s.previews[id] = p
	return p, nil
}

type copier struct {
	ctx   context.Context
	bytes int64
	files int
}

func (c *copier) directory(root *os.Root, rel, dest string) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	f, err := root.Open(rel)
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, e := range entries {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(rel, e.Name())
		info, err := root.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%s is a symlink or special file", path)
		}
		target := filepath.Join(dest, e.Name())
		if info.IsDir() {
			// Pin each directory before descending; the inode comparison detects
			// replacement by a different directory, and os.Root contains lookups.
			sub, err := root.OpenRoot(path)
			if err != nil {
				return err
			}
			opened, statErr := sub.Stat(".")
			if statErr != nil || !os.SameFile(info, opened) {
				sub.Close()
				return fmt.Errorf("directory %s changed during publication", path)
			}
			if err := os.Mkdir(target, 0700); err != nil {
				sub.Close()
				return err
			}
			err = c.directory(sub, ".", target)
			closeErr := sub.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		} else if err := c.file(root, path, info, target); err != nil {
			return err
		}
	}
	return nil
}

func (c *copier) file(root *os.Root, name string, info os.FileInfo, dest string) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	c.files++
	if c.files > maxFiles {
		return fmt.Errorf("snapshot exceeds %d files", maxFiles)
	}
	if info.Size() > maxBytes-c.bytes {
		return fmt.Errorf("snapshot exceeds %d MiB", maxBytes>>20)
	}
	// A source can be replaced with a FIFO between Lstat and Open. Nonblocking
	// open prevents such a replacement from hanging the daemon.
	in, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return fmt.Errorf("file %s changed during publication", name)
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(out, io.LimitReader(contextReader{ctx: c.ctx, Reader: in}, maxBytes-c.bytes+1))
	closeErr := out.Close()
	c.bytes += n
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if c.bytes > maxBytes {
		return fmt.Errorf("snapshot exceeds %d MiB", maxBytes>>20)
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(b)
}

func (s *Store) List() []Preview {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]Preview, 0, len(s.previews))
	now := s.now()
	for _, p := range s.previews {
		if now.Before(p.ExpiresAt) {
			list = append(list, p)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].ID < list[j].ID
		}
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})
	return list
}

func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.previews[id]; !ok {
		return ErrNotFound
	}
	return s.remove(id)
}

// remove commits removal by deleting metadata first. Orphaned data is never
// served and will be retried by New if its subsequent deletion fails.
func (s *Store) remove(id string) error {
	if err := os.Remove(filepath.Join(s.dir, "meta", id+".json")); err != nil {
		return err
	}
	delete(s.previews, id)
	return os.RemoveAll(filepath.Join(s.dir, "data", id))
}

func (s *Store) Prune() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	now := s.now()
	for id, p := range s.previews {
		if !now.Before(p.ExpiresAt) {
			if err := s.remove(id); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
