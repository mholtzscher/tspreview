package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mholtzscher/tspreview/internal/preview"
)

type status struct {
	URL string `json:"url"`
	PID int    `json:"pid"`
}

type publication struct {
	preview.Preview
	URL string `json:"url"`
}

type publishRequest struct {
	Path string        `json:"path"`
	TTL  time.Duration `json:"ttl"`
}

func withURL(p preview.Preview, base string) publication {
	path := (&url.URL{Path: "/p/" + p.ID + "/" + p.Entry}).String()
	return publication{Preview: p, URL: strings.TrimSuffix(base, "/") + path}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func controlHandler(store *preview.Store, base string, stop context.CancelFunc) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, status{URL: base, PID: os.Getpid()})
	})
	mux.HandleFunc("GET /previews", func(w http.ResponseWriter, r *http.Request) {
		items := []publication{}
		for _, p := range store.List() {
			items = append(items, withURL(p, base))
		}
		writeJSON(w, http.StatusOK, items)
	})
	mux.HandleFunc("POST /previews", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var req publishRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			writeError(w, http.StatusBadRequest, fmt.Errorf("expected one JSON request"))
			return
		}
		if req.Path == "" || req.TTL <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("path and positive ttl are required"))
			return
		}
		p, err := store.Publish(r.Context(), req.Path, req.TTL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, withURL(p, base))
	})
	mux.HandleFunc("DELETE /previews/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Remove(r.PathValue("id")); err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, preview.ErrNotFound) {
				code = http.StatusNotFound
			}
			writeError(w, code, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
		stop()
	})
	return mux
}

type controlClient struct {
	client *http.Client
}

// controlError is a definitive server rejection, unlike a lost response after
// a successful commit, which leaves the publication outcome ambiguous.
type controlError struct {
	message string
}

func (e *controlError) Error() string { return e.message }

func newControlClient(c config) *controlClient {
	return &controlClient{client: &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", c.socket())
		},
	}}}
}

func (c *controlClient) request(ctx context.Context, method, path string, body, result any) error {
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://tspreview.local"+path, &b)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		var msg struct{ Error string }
		if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&msg); err != nil || msg.Error == "" {
			return &controlError{message: fmt.Sprintf("control server returned %s", res.Status)}
		}
		return &controlError{message: msg.Error}
	}
	if result == nil {
		_, err := io.Copy(io.Discard, res.Body)
		return err
	}
	return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(result)
}

func (c *controlClient) status(ctx context.Context) (status, error) {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var s status
	err := c.request(ctx, http.MethodGet, "/status", nil, &s)
	return s, err
}
