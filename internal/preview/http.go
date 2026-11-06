package preview

import (
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Handler serves only live snapshots. It does not expose directory listings.
func (s *Store) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Store) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// URL.Path is decoded by net/http, so this also rejects encoded traversal.
	// Never clean an attacker-supplied path into another.
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 || parts[0] != "" || parts[1] != "p" || !validID(parts[2]) {
		http.NotFound(w, r)
		return
	}
	for i, part := range parts[3:] {
		if part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") || (part == "" && i != len(parts[3:])-1) {
			http.NotFound(w, r)
			return
		}
	}
	id := parts[2]
	s.mu.RLock()
	p, ok := s.previews[id]
	live := ok && s.now().Before(p.ExpiresAt)
	s.mu.RUnlock()
	// Never hold the registry lock across a potentially slow network response.
	// Concurrent removal may turn this request into a 404; already-open files
	// remain safe to serve because snapshot IDs and bytes are never reused.
	if !live {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 3 {
		redirect(w, r, r.URL.EscapedPath()+"/")
		return
	}
	if len(parts) == 4 && parts[3] == "" && p.Entry != "" {
		redirect(w, r, "/p/"+id+"/"+url.PathEscape(p.Entry))
		return
	}
	data, err := os.OpenRoot(filepath.Join(s.dir, "data"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer data.Close()
	info, err := data.Lstat(id)
	if err != nil || !info.IsDir() {
		http.NotFound(w, r)
		return
	}
	root, err := data.OpenRoot(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		http.NotFound(w, r)
		return
	}
	rel := strings.Join(parts[3:], "/")
	if rel == "" {
		rel = "."
	}
	f, err := root.Open(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if stat.IsDir() {
		index, err := root.Open(path.Join(rel, "index.html"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer index.Close()
		stat, err = index.Stat()
		if err != nil || !stat.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/") {
			redirect(w, r, r.URL.EscapedPath()+"/")
			return
		}
		http.ServeContent(w, r, "index.html", stat.ModTime(), index)
		return
	}
	if !stat.Mode().IsRegular() || strings.HasSuffix(r.URL.Path, "/") {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), f)
}

func redirect(w http.ResponseWriter, r *http.Request, location string) {
	if r.URL.RawQuery != "" {
		location += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, location, http.StatusMovedPermanently)
}
