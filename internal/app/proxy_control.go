package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

func (m *proxyManager) serveControl(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, m.list())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req proxyRequest
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
	p, err := m.register(r.Context(), req)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errProxyConflict) {
			code = http.StatusConflict
		}
		writeError(w, code, err)
		return
	}
	defer m.release(p)
	// The streaming control connection is the lease. Closing the foreground
	// client's connection releases only this app, including upgraded sockets.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusCreated)
	enc := json.NewEncoder(w)
	if err := enc.Encode(p.registration); err != nil {
		return
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.Context().Done():
			return
		case err := <-p.done:
			enc.Encode(map[string]string{"error": fmt.Sprintf("proxy listener stopped: %v", err)})
			return
		case <-ticker.C:
			if err := enc.Encode(struct{}{}); err != nil {
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
		}
	}
}
