package apiclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
)

// The saved client target (W9.6, topo:T9): point a client at a remote daemon
// once instead of repeating --daemon-url/--token or exporting AF_DAEMON_* in
// every shell. It lives in the CLIENT's AF home, is written 0600, and is the
// LOWEST-precedence source: flag, then env, then this file.
//
// Two rules keep it from becoming a way to hand a credential to the wrong host:
//
//   - The saved token is paired only with the saved URL. A URL named on the
//     command line or in the environment never inherits it — otherwise
//     `af --daemon-url http://other` would send the stored bearer token to
//     `other`.
//   - It fails closed. A saved file that cannot be read, does not parse, or is
//     readable by group/other does not fall back to the local daemon (which
//     would act on the wrong host); the target resolves as remote-with-error and
//     the client constructors return that error.

const savedTargetFile = "client-target.json"

// SavedTarget is the persisted client target.
type SavedTarget struct {
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

// SavedTargetPath is where the saved target lives for the current AF home.
func SavedTargetPath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, savedTargetFile), nil
}

// LoadSavedTarget reads the saved target. A missing file is (nil, nil); any
// other problem is an error the caller must surface.
func LoadSavedTarget() (*SavedTarget, error) {
	path, err := SavedTargetPath()
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("saved client target %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("saved client target %s is not a regular file; run `af target clear` and set it again", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("saved client target %s is accessible by other users (mode %o) and may hold a bearer token; "+
			"run `chmod 600 %s`, or `af target clear` and set it again", path, info.Mode().Perm(), path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("saved client target %s: %w", path, err)
	}
	var t SavedTarget
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("saved client target %s is malformed (%v); run `af target clear` and set it again", path, err)
	}
	if _, _, err := parseDaemonURL(t.URL); err != nil {
		return nil, fmt.Errorf("saved client target %s: %w; run `af target set` with a valid URL", path, err)
	}
	return &t, nil
}

// SaveTarget validates and persists a target, 0600.
func SaveTarget(t SavedTarget) error {
	if _, _, err := parseDaemonURL(t.URL); err != nil {
		return err
	}
	path, err := SavedTargetPath()
	if err != nil {
		return err
	}
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return config.AtomicWriteFileRefusingLink(path, append(data, '\n'), 0o600)
}

// ClearSavedTarget removes the saved target; clearing nothing is not an error.
func ClearSavedTarget() error {
	path, err := SavedTargetPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// TargetSource names which input chose the target in use.
type TargetSource string

const (
	TargetFlag  TargetSource = "flag"
	TargetEnv   TargetSource = "env"
	TargetSaved TargetSource = "saved"
	TargetLocal TargetSource = "local"
)

// TargetInfo describes the resolved target without exposing the token.
type TargetInfo struct {
	URL      string // empty for the local socket
	Source   TargetSource
	HasToken bool
	Err      error // a saved target that failed closed
}

// ResolveTargetInfo reports which target this invocation will use and why. It
// backs `af target show`.
func ResolveTargetInfo() TargetInfo {
	u, tok, src, err := resolveTargetFull()
	return TargetInfo{URL: u, Source: src, HasToken: tok != "", Err: err}
}
