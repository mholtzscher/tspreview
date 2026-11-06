package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

const usage = `tspreview — disposable HTTPS previews on your tailnet

Usage:
  tspreview login [--hostname NAME]  Authenticate once; provision HTTPS
  tspreview <file-or-directory>     Publish a snapshot and print its URL
  tspreview list                   List unexpired previews (starts server)
  tspreview rm <id>                Delete a preview
  tspreview stop                   Stop the server; keep unexpired snapshots

Options (may appear after the path):
  --ttl DURATION    Preview lifetime; default 24h (e.g. 30m, 2h)
  --json            Structured JSON output instead of human-readable output
  --state-dir PATH  Private state directory; default $XDG_STATE_HOME/tspreview
                    or ~/.local/state/tspreview
  --timeout DURATION  Startup/login/request deadline; default 2m
  --verbose         Include tsnet diagnostics in stderr or daemon.log
  --help            Show this help

First run: tspreview login. For unattended setup: TS_AUTHKEY=… tspreview login.
Directory publication includes assets and skips hidden files. Single-file
publication includes only that file. Symlinks and special files are rejected.
Requires Linux, tailnet MagicDNS and HTTPS certificates. No sudo or Funnel.
`

// parseInterspersed preserves -- semantics while accepting flags after a path.
func parseInterspersed(fs *flag.FlagSet, args []string) error {
	var options, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		options = append(options, a)
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		f := fs.Lookup(name)
		if f == nil {
			return fmt.Errorf("unknown option %q", a)
		}
		isBool := false
		if v, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
			isBool = v.IsBoolFlag()
		}
		if !hasValue && !isBool {
			i++
			if i >= len(args) {
				return fmt.Errorf("option %q requires a value", a)
			}
			options = append(options, args[i])
		}
	}
	return fs.Parse(append(append(options, "--"), positional...))
}

// Run executes one CLI command. Successful publication emits only its URL unless
// --json is set; authentication and diagnostic output go to stderr.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(stdout, usage)
		return err
	}
	command := "publish"
	switch args[0] {
	case "login", "list", "rm", "stop", "publish", "__daemon":
		command, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("tspreview", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var c config
	var jsonOutput, help bool
	var ttl time.Duration
	fs.StringVar(&c.StateDir, "state-dir", "", "state directory")
	fs.StringVar(&c.Hostname, "hostname", "", "node hostname (login only)")
	fs.DurationVar(&c.Timeout, "timeout", 2*time.Minute, "command timeout")
	fs.BoolVar(&c.Verbose, "verbose", false, "tsnet diagnostic logging")
	fs.BoolVar(&jsonOutput, "json", false, "JSON output")
	fs.BoolVar(&help, "help", false, "show help")
	fs.DurationVar(&ttl, "ttl", 24*time.Hour, "preview lifetime")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if help {
		_, err := io.WriteString(stdout, usage)
		return err
	}
	if c.Timeout <= 0 || ttl <= 0 {
		return fmt.Errorf("timeout and ttl must be positive durations")
	}
	if command != "login" && c.Hostname != "" {
		return fmt.Errorf("--hostname is only supported by login")
	}
	if command == "publish" || command == "rm" {
		if fs.NArg() != 1 {
			argument := "preview ID"
			if command == "publish" {
				argument = "file or directory"
			}
			return fmt.Errorf("%s requires exactly one %s", command, argument)
		}
	} else if fs.NArg() != 0 {
		return fmt.Errorf("%s does not accept positional arguments", command)
	}
	if c.StateDir == "" {
		var err error
		c.StateDir, err = defaultStateDir()
		if err != nil {
			return err
		}
	}
	var err error
	c.StateDir, err = filepath.Abs(c.StateDir)
	if err != nil {
		return err
	}
	if err := c.prepare(); err != nil {
		return err
	}
	if command == "__daemon" {
		id, err := c.loadIdentity()
		if err != nil {
			return err
		}
		return runDaemon(ctx, c, newTSNet(c, id.Hostname, stderr), stderr)
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	switch command {
	case "login":
		return login(ctx, c, jsonOutput, stdout, stderr)
	case "stop":
		return stop(ctx, c, jsonOutput, stdout)
	}
	var source string
	if command == "publish" {
		source, err = filepath.Abs(fs.Arg(0))
		if err != nil {
			return err
		}
		if _, err := os.Lstat(source); err != nil {
			return fmt.Errorf("source: %w", err)
		}
	}
	client, err := ensureDaemon(ctx, c, launchDaemon)
	if err != nil {
		return err
	}
	switch command {
	case "publish":
		var p publication
		if err := client.request(ctx, http.MethodPost, "/previews", publishRequest{Path: source, TTL: ttl}, &p); err != nil {
			var rejected *controlError
			if !errors.As(err, &rejected) {
				return fmt.Errorf("publication outcome is unknown: %w; run tspreview list --json with the same --state-dir before retrying", err)
			}
			return err
		}
		if jsonOutput {
			return json.NewEncoder(stdout).Encode(p)
		}
		_, err := fmt.Fprintln(stdout, p.URL)
		return err
	case "list":
		var items []publication
		if err := client.request(ctx, http.MethodGet, "/previews", nil, &items); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(stdout).Encode(items)
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tEXPIRES\tURL")
		for _, p := range items {
			fmt.Fprintf(tw, "%s\t%q\t%s\t%s\n", p.ID, p.Name, p.ExpiresAt.Format(time.RFC3339), p.URL)
		}
		return tw.Flush()
	case "rm":
		if err := client.request(ctx, http.MethodDelete, "/previews/"+url.PathEscape(fs.Arg(0)), nil, nil); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(stdout).Encode(map[string]string{"removed": fs.Arg(0)})
		}
		_, err := fmt.Fprintln(stdout, "Removed", fs.Arg(0))
		return err
	}
	return fmt.Errorf("unknown command %q", command)
}

func login(ctx context.Context, c config, jsonOutput bool, stdout, stderr io.Writer) error {
	starter, err := lock(ctx, filepath.Join(c.StateDir, "launch.lock"), true)
	if err != nil {
		return err
	}
	defer starter.Close()
	if s, err := newControlClient(c).status(ctx); err == nil {
		return fmt.Errorf("server already running at %s; run tspreview stop before logging in again", s.URL)
	}
	owner, err := lock(ctx, filepath.Join(c.StateDir, "daemon.lock"), false)
	if err != nil {
		return err
	}
	defer owner.Close()
	if c.Hostname == "" {
		if saved, err := c.loadIdentity(); err == nil {
			c.Hostname = saved.Hostname
		} else {
			c.Hostname = defaultHostname()
		}
	}
	if len(c.Hostname) > 63 || strings.Trim(c.Hostname, "-") != c.Hostname || invalidHostname.MatchString(c.Hostname) {
		return fmt.Errorf("hostname must be a lowercase DNS label of at most 63 characters")
	}
	node := newTSNet(c, c.Hostname, stderr)
	id, err := node.connect(ctx)
	defer node.Close()
	if err != nil {
		return err
	}
	if err := c.saveIdentity(id); err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(stdout).Encode(id)
	}
	_, err = fmt.Fprintln(stdout, "Authenticated:", "https://"+id.DNSName)
	return err
}

func stop(ctx context.Context, c config, jsonOutput bool, stdout io.Writer) error {
	starter, err := lock(ctx, filepath.Join(c.StateDir, "launch.lock"), true)
	if err != nil {
		return err
	}
	defer starter.Close()
	client := newControlClient(c)
	if _, err := client.status(ctx); err == nil {
		if err := client.request(ctx, http.MethodPost, "/stop", nil, nil); err != nil {
			return err
		}
		owner, err := lock(ctx, filepath.Join(c.StateDir, "daemon.lock"), true)
		if err != nil {
			return err
		}
		owner.Close()
	} else {
		owner, err := lock(ctx, filepath.Join(c.StateDir, "daemon.lock"), false)
		if errors.Is(err, errLocked) {
			return fmt.Errorf("server is busy but control socket is unavailable; check %s", filepath.Join(c.StateDir, "daemon.log"))
		}
		if err != nil {
			return err
		}
		owner.Close()
	}
	if jsonOutput {
		return json.NewEncoder(stdout).Encode(map[string]bool{"stopped": true})
	}
	_, err = fmt.Fprintln(stdout, "Stopped")
	return err
}
