// Package proxy forwards local development apps without changing their root URL.
package proxy

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Route struct {
	Prefix   string `json:"prefix"`
	Upstream string `json:"upstream"`
}

type routeHandler struct {
	prefix string
	proxy  *httputil.ReverseProxy
}

type Handler struct {
	routes    []routeHandler
	transport *http.Transport
}

var routePrefix = regexp.MustCompile(`^(/[A-Za-z0-9_-]+)+$`)

// New accepts loopback HTTP(S) origins only. Route prefixes are stripped before
// forwarding; the default upstream receives all other paths unchanged.
func New(upstream string, routes []Route, logger *log.Logger) (*Handler, error) {
	if logger == nil {
		logger = log.Default()
	}
	h := &Handler{transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       time.Minute,
	}}
	seen := map[string]bool{}
	all := append(slices.Clone(routes), Route{Upstream: upstream})
	for _, route := range all {
		if route.Prefix != "" && !routePrefix.MatchString(route.Prefix) {
			return nil, fmt.Errorf("invalid route prefix %q: use /path segments without a trailing slash", route.Prefix)
		}
		if seen[route.Prefix] {
			return nil, fmt.Errorf("duplicate route prefix %q", route.Prefix)
		}
		seen[route.Prefix] = true
		target, err := parseOrigin(route.Upstream)
		if err != nil {
			return nil, err
		}
		prefix := route.Prefix
		h.routes = append(h.routes, routeHandler{prefix: prefix, proxy: &httputil.ReverseProxy{
			Transport:     h.transport,
			ErrorLog:      logger,
			FlushInterval: -1,
			Rewrite: func(r *httputil.ProxyRequest) {
				if prefix != "" {
					r.Out.URL.Path = strings.TrimPrefix(r.Out.URL.Path, prefix)
					if r.Out.URL.Path == "" {
						r.Out.URL.Path = "/"
					}
					// Prefixes contain only unescaped ASCII, but the client may
					// encode those bytes. Preserve the escaped suffix verbatim.
					if r.Out.URL.RawPath != "" {
						r.Out.URL.RawPath = escapedSuffix(r.Out.URL.RawPath, len(prefix))
					}
				}
				r.SetURL(target)
				r.SetXForwarded()
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				logger.Printf("upstream unavailable: %v", err)
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
			},
		}})
	}
	slices.SortFunc(h.routes, func(a, b routeHandler) int { return len(b.prefix) - len(a.prefix) })
	return h, nil
}

func parseOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("upstream must be an http(s) origin without credentials, path, query, or fragment")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("upstream must use a loopback IP address or localhost")
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, fmt.Errorf("upstream port must not be empty")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("upstream port must be between 1 and 65535")
		}
	}
	u.Path, u.RawPath = "", ""
	return u, nil
}

// escapedSuffix removes n decoded ASCII bytes without decoding the suffix.
func escapedSuffix(path string, n int) string {
	i := 0
	for range n {
		if path[i] == '%' {
			i += 3
		} else {
			i++
		}
	}
	if i == len(path) {
		return "/"
	}
	return path[i:]
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, route := range h.routes {
		if route.prefix == "" || r.URL.Path == route.prefix || strings.HasPrefix(r.URL.Path, route.prefix+"/") {
			route.proxy.ServeHTTP(w, r)
			return
		}
	}
}

func (h *Handler) Close() { h.transport.CloseIdleConnections() }
