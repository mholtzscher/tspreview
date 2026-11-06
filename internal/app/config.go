package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type config struct {
	StateDir string
	Hostname string
	Timeout  time.Duration
	Verbose  bool
}

type identity struct {
	Hostname string `json:"hostname"`
	DNSName  string `json:"dns_name"`
}

func defaultStateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("XDG_STATE_HOME must be an absolute path")
		}
		return filepath.Join(dir, "tspreview"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "tspreview"), nil
}

var invalidHostname = regexp.MustCompile(`[^a-z0-9-]+`)

func defaultHostname() string {
	host, _ := os.Hostname()
	host = strings.Split(host, ".")[0]
	host = strings.Trim(invalidHostname.ReplaceAllString(strings.ToLower(host), "-"), "-")
	if host == "" {
		host = "host"
	}
	return strings.TrimRight("tspreview-"+host[:min(len(host), 53)], "-")
}

func (c config) socket() string {
	// Keep Unix socket paths short even with a deeply nested custom state directory.
	// The directory is verified as private and owned before a socket is used.
	hash := sha256.Sum256([]byte(c.StateDir))
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" || !filepath.IsAbs(base) {
		base = os.TempDir()
	}
	return filepath.Join(base, "tspreview-"+fmt.Sprint(os.Getuid())+"-"+hex.EncodeToString(hash[:8]), "control.sock")
}

func (c config) prepare() error {
	for _, dir := range []string{c.StateDir, filepath.Join(c.StateDir, "node"), filepath.Dir(c.socket())} {
		if err := privateDir(dir); err != nil {
			return err
		}
	}
	if len(c.socket()) > 103 {
		return fmt.Errorf("control socket path is too long; use a shorter XDG_RUNTIME_DIR")
	}
	return nil
}

func (c config) loadIdentity() (identity, error) {
	var id identity
	b, err := os.ReadFile(filepath.Join(c.StateDir, "identity.json"))
	if os.IsNotExist(err) {
		return id, fmt.Errorf("not logged in; run tspreview login first (or TS_AUTHKEY=… tspreview login)")
	}
	if err != nil {
		return id, err
	}
	if err := json.Unmarshal(b, &id); err != nil {
		return id, fmt.Errorf("read identity: %w", err)
	}
	if id.Hostname == "" || id.DNSName == "" {
		return id, fmt.Errorf("invalid saved identity; run tspreview login")
	}
	return id, nil
}

func (c config) saveIdentity(id identity) error {
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(c.StateDir, "identity.json")
	if err := os.WriteFile(path+".tmp", append(b, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}
