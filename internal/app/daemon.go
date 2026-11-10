package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mholtzscher/tspreview/internal/preview"
)

func runDaemon(ctx context.Context, c config, node network, stderr io.Writer) error {
	if err := c.prepare(); err != nil {
		return err
	}
	owner, err := lock(ctx, filepath.Join(c.StateDir, "daemon.lock"), false)
	if err != nil {
		return err
	}
	defer owner.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	store, err := preview.New(filepath.Join(c.StateDir, "previews"))
	if err != nil {
		return err
	}
	startup, startupCancel := context.WithTimeout(ctx, c.Timeout)
	defer startupCancel()
	publicListener, baseURL, err := node.Listen(startup)
	defer node.Close()
	if err != nil {
		return err
	}
	startupCancel()
	defer publicListener.Close()
	// Only the lock owner may remove a stale socket or bind a new one.
	if err := os.Remove(c.socket()); err != nil && !os.IsNotExist(err) {
		return err
	}
	controlListener, err := net.Listen("unix", c.socket())
	if err != nil {
		return err
	}
	defer controlListener.Close()
	defer os.Remove(c.socket())
	if err := os.Chmod(c.socket(), 0600); err != nil {
		return err
	}
	logger := log.New(stderr, "tspreview: ", log.LstdFlags)
	proxies, err := newProxyManager(c, node, logger)
	if err != nil {
		return err
	}
	defer proxies.close()
	publicServer := &http.Server{
		Handler: store.Handler(), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: time.Minute, ErrorLog: logger,
	}
	controlMux := http.NewServeMux()
	controlMux.Handle("/", controlHandler(store, baseURL, cancel))
	controlMux.HandleFunc("GET /proxies", func(w http.ResponseWriter, r *http.Request) { proxies.serveControl(ctx, w, r) })
	controlMux.HandleFunc("POST /proxies", func(w http.ResponseWriter, r *http.Request) { proxies.serveControl(ctx, w, r) })
	controlServer := &http.Server{
		Handler: controlMux, ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: time.Minute, ErrorLog: logger,
	}
	serverErrors := make(chan error, 2)
	go func() { serverErrors <- publicServer.Serve(publicListener) }()
	go func() { serverErrors <- controlServer.Serve(controlListener) }()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var runErr error
running:
	for {
		select {
		case <-ctx.Done():
			break running
		case err := <-serverErrors:
			if !errors.Is(err, http.ErrServerClosed) {
				runErr = err
			}
			break running
		case <-ticker.C:
			if err := store.Prune(); err != nil {
				logger.Printf("prune previews: %v", err)
			}
		}
	}
	cancel()
	proxies.close()
	shutdown, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := controlServer.Shutdown(shutdown); err != nil {
		controlServer.Close()
		runErr = errors.Join(runErr, err)
	}
	if err := publicServer.Shutdown(shutdown); err != nil {
		publicServer.Close()
		runErr = errors.Join(runErr, err)
	}
	return runErr
}

// startProcess starts a new session with no inherited terminal or pipe handles.
// prefixArgs is nil in production; subprocess tests use the Go test executable.
func startProcess(c config, executable string, prefixArgs []string) (*exec.Cmd, error) {
	fd, err := syscall.Open(filepath.Join(c.StateDir, "daemon.log"), syscall.O_WRONLY|syscall.O_CREAT|syscall.O_APPEND|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	logFile := os.NewFile(uintptr(fd), "daemon.log")
	defer logFile.Close()
	args := append(append([]string{}, prefixArgs...), "__daemon", "--state-dir", c.StateDir, "--timeout", c.Timeout.String())
	if c.Verbose {
		args = append(args, "--verbose")
	}
	cmd := exec.Command(executable, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

type launcher func(config) (*exec.Cmd, error)

func launchDaemon(c config) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return startProcess(c, exe, nil)
}

func ensureDaemon(ctx context.Context, c config, launch launcher) (*controlClient, error) {
	if err := c.prepare(); err != nil {
		return nil, err
	}
	// Serialize startup across separate CLI invocations, not just goroutines.
	starter, err := lock(ctx, filepath.Join(c.StateDir, "launch.lock"), true)
	if err != nil {
		return nil, err
	}
	defer starter.Close()
	client := newControlClient(c)
	if _, err := client.status(ctx); err == nil {
		return client, nil
	}
	if _, err := c.loadIdentity(); err != nil {
		return nil, err
	}
	var exit <-chan error
	var child *exec.Cmd
	owner, err := lock(ctx, filepath.Join(c.StateDir, "daemon.lock"), false)
	if err == nil {
		owner.Close()
		child, err = launch(c)
		if err != nil {
			return nil, err
		}
		ch := make(chan error, 1)
		go func() { ch <- child.Wait() }()
		exit = ch
	} else if !errors.Is(err, errLocked) {
		return nil, err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := client.status(ctx); err == nil {
			return client, nil
		}
		select {
		case <-ctx.Done():
			if child != nil {
				child.Process.Signal(syscall.SIGTERM)
			}
			return nil, fmt.Errorf("waiting for preview server: %w; check %s", ctx.Err(), filepath.Join(c.StateDir, "daemon.log"))
		case err := <-exit:
			return nil, fmt.Errorf("preview server exited during startup (%v); check %s", err, filepath.Join(c.StateDir, "daemon.log"))
		case <-ticker.C:
		}
	}
}
