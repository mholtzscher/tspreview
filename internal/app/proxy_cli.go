package app

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mholtzscher/tspreview/internal/proxy"
)

const proxyUsage = `Usage: tspreview proxy <http(s)://loopback:port> [options]

Run a foreground proxy on the shared Tailscale node. Exiting removes this
proxy only. The app stays at / on a saved HTTPS port in 10000–59999.
Mise or another supervisor owns upstream processes and their local ports.

Options:
  --name NAME        App identity within a worktree; default web
  --worktree PATH    Identity root; default Git worktree root or current directory
  --route PREFIX=URL Additional loopback upstream; strip prefix (repeatable)
  --json             Print registration JSON instead of its URL
  --state-dir PATH   Shared tspreview state directory
  --timeout DURATION Startup deadline only; default 2m
  --verbose          Enable diagnostics when starting the shared server
  --help             Show this help

Run tspreview login first. Allow tailnet TCP ports 10000–59999.
Run tspreview proxies to list active proxies, or tspreview stop to stop all.
`

type routeFlags []proxy.Route

func (r *routeFlags) String() string { return fmt.Sprint([]proxy.Route(*r)) }
func (r *routeFlags) Set(raw string) error {
	prefix, upstream, ok := strings.Cut(raw, "=")
	if !ok || prefix == "" || upstream == "" {
		return fmt.Errorf("route must be /prefix=http(s)://loopback:port")
	}
	*r = append(*r, proxy.Route{Prefix: prefix, Upstream: upstream})
	return nil
}

func runProxy(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tspreview proxy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var c config
	var req proxyRequest
	var jsonOutput, help bool
	var routes routeFlags
	fs.StringVar(&c.StateDir, "state-dir", "", "shared state directory")
	fs.DurationVar(&c.Timeout, "timeout", 2*time.Minute, "startup deadline")
	fs.BoolVar(&c.Verbose, "verbose", false, "diagnostic logging")
	fs.StringVar(&req.Name, "name", "web", "app identity")
	fs.StringVar(&req.Worktree, "worktree", "", "identity root")
	fs.Var(&routes, "route", "path-prefix upstream")
	fs.BoolVar(&jsonOutput, "json", false, "JSON output")
	fs.BoolVar(&help, "help", false, "show help")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if help {
		_, err := io.WriteString(stdout, proxyUsage)
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("proxy requires exactly one upstream URL")
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("timeout must be a positive duration")
	}
	startup, startupCancel := context.WithTimeout(ctx, c.Timeout)
	defer startupCancel()
	var err error
	req.Worktree, err = resolveWorktree(startup, req.Worktree)
	if err != nil {
		return err
	}
	req.Upstream, req.Routes = fs.Arg(0), routes
	if _, err := proxyIdentity(req); err != nil {
		return err
	}
	// Validate before daemon startup or touching shared state.
	handler, err := proxy.New(req.Upstream, req.Routes, nil)
	if err != nil {
		return err
	}
	handler.Close()
	if c.StateDir == "" {
		c.StateDir, err = defaultStateDir()
		if err != nil {
			return err
		}
	}
	c.StateDir, err = filepath.Abs(c.StateDir)
	if err != nil {
		return err
	}
	client, err := ensureDaemon(startup, c, launchDaemon)
	if err != nil {
		return err
	}
	lease, registration, err := client.openProxy(ctx, startup, req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer lease.Close()
	startupCancel()
	if jsonOutput {
		err = json.NewEncoder(stdout).Encode(registration)
	} else {
		_, err = fmt.Fprintln(stdout, registration.URL)
	}
	if err != nil {
		return err
	}
	for {
		var event struct {
			Error string `json:"error"`
		}
		err := lease.decoder.Decode(&event)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("proxy lease lost: %w; restart the proxy command", err)
		}
		if event.Error != "" {
			return fmt.Errorf("proxy lease lost: %s; restart the proxy command", event.Error)
		}
	}
}

func resolveWorktree(ctx context.Context, selected string) (string, error) {
	if selected == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		selected = cwd
		cmd := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel")
		if output, err := cmd.Output(); err == nil {
			selected = strings.TrimSuffix(string(output), "\n")
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	abs, err := filepath.Abs(selected)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve worktree: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("worktree must be a directory")
	}
	return root, nil
}

type proxyClientLease struct {
	body    io.ReadCloser
	decoder *json.Decoder
	cancel  context.CancelFunc
}

func (l *proxyClientLease) Close() { l.cancel(); l.body.Close() }

func (c *controlClient) openProxy(ctx, startup context.Context, target proxyRequest) (*proxyClientLease, proxyRegistration, error) {
	var registration proxyRegistration
	b, err := json.Marshal(target)
	if err != nil {
		return nil, registration, err
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	stopStartup := context.AfterFunc(startup, cancel)
	defer stopStartup()
	req, err := http.NewRequestWithContext(leaseCtx, http.MethodPost, "http://tspreview.local/proxies", bytes.NewReader(b))
	if err != nil {
		cancel()
		return nil, registration, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		cancel()
		return nil, registration, err
	}
	if res.StatusCode != http.StatusCreated {
		defer res.Body.Close()
		defer cancel()
		if res.StatusCode == http.StatusNotFound {
			return nil, registration, fmt.Errorf("server does not support proxies; run tspreview stop, then retry")
		}
		var msg struct{ Error string }
		if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&msg); err != nil || msg.Error == "" {
			return nil, registration, fmt.Errorf("proxy registration returned %s", res.Status)
		}
		return nil, registration, &controlError{message: msg.Error}
	}
	decoder := json.NewDecoder(res.Body)
	if err := decoder.Decode(&registration); err != nil {
		res.Body.Close()
		cancel()
		return nil, registration, fmt.Errorf("read proxy registration: %w", err)
	}
	if !stopStartup() && startup.Err() != nil {
		res.Body.Close()
		cancel()
		return nil, registration, startup.Err()
	}
	if err := leaseCtx.Err(); err != nil {
		res.Body.Close()
		cancel()
		return nil, registration, err
	}
	return &proxyClientLease{body: res.Body, decoder: decoder, cancel: cancel}, registration, nil
}
