package tmux

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	serverStateFileName = "tmux-server-state.json"
	serverDeathFileName = "tmux-server-death.json"
)

// ServerMode says how the daemon's sessions are hosted (#6180).
type ServerMode string

const (
	// ServerModeDedicated: a dedicated tmux server runs in its own systemd scope.
	ServerModeDedicated ServerMode = "dedicated"
	// ServerModeFallback: the dedicated server could not be launched, so each
	// session is started in its own systemd scope and the first of them becomes
	// the server for the socket.
	ServerModeFallback ServerMode = "fallback"
)

// ServerState is what the daemon last decided about its tmux server. The daemon
// writes it; `af doctor` runs in another process and reads it.
type ServerState struct {
	Mode ServerMode `json:"mode"`
	// Socket is the tmux socket the sessions live on, as tmux resolves it for the
	// daemon. Any plain `tmux` run with the same TMUX_TMPDIR (or none) and without
	// $TMUX set reaches every session on it.
	Socket string    `json:"socket"`
	Unit   string    `json:"unit,omitempty"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// ServerDeath is the marker the dedicated server's wrapper leaves when tmux exits
// (#5429): exit code or signal, how long the server lived, and which unit it ran
// in. systemd reports a scope's teardown but not who ended the server; this at
// least records what the server's own parent saw.
type ServerDeath struct {
	At            time.Time `json:"at"`
	Unit          string    `json:"unit,omitempty"`
	Socket        string    `json:"socket,omitempty"`
	ExitCode      int       `json:"exit_code"`
	Signal        string    `json:"signal,omitempty"`
	Error         string    `json:"error,omitempty"`
	UptimeSeconds float64   `json:"uptime_seconds"`
}

// Describe is the one-line form used in logs and `af doctor`.
func (d ServerDeath) Describe() string {
	how := fmt.Sprintf("exit code %d", d.ExitCode)
	if d.Signal != "" {
		how = "signal " + d.Signal
	}
	return fmt.Sprintf("the dedicated tmux server ended at %s with %s after %s",
		d.At.Format(time.RFC3339), how, (time.Duration(d.UptimeSeconds) * time.Second).String())
}

// defaultSocketPath is the socket a bare `tmux` run by this process resolves to.
// It is also the identity the dedicated server's unit name is derived from.
func defaultSocketPath() string {
	identity := os.Getenv("TMUX")
	if socket, _, ok := strings.Cut(identity, ","); ok {
		identity = socket
	}
	if identity == "" {
		base := os.Getenv("TMUX_TMPDIR")
		if base == "" {
			base = "/tmp"
		}
		identity = fmt.Sprintf("%s/tmux-%d/default", base, os.Getuid())
	}
	return identity
}

var (
	serverStateMu   sync.Mutex
	lastServerState ServerState
)

// recordServerState persists st under home, skipping the write when nothing but
// the timestamp changed (the fallback path runs on every session spawn).
func recordServerState(home string, st ServerState) {
	serverStateMu.Lock()
	defer serverStateMu.Unlock()
	if st.Mode == lastServerState.Mode && st.Unit == lastServerState.Unit &&
		st.Socket == lastServerState.Socket && st.Reason == lastServerState.Reason {
		return
	}
	if err := writeJSONFile(filepath.Join(home, serverStateFileName), st); err != nil {
		return
	}
	lastServerState = st
}

// ReadServerState returns the daemon's last recorded tmux server decision.
// ok is false when none has been recorded.
func ReadServerState(home string) (ServerState, bool, error) {
	var st ServerState
	ok, err := readJSONFile(filepath.Join(home, serverStateFileName), &st)
	return st, ok, err
}

// ReadServerDeath returns the last death marker, if any.
func ReadServerDeath(home string) (ServerDeath, bool, error) {
	var d ServerDeath
	ok, err := readJSONFile(filepath.Join(home, serverDeathFileName), &d)
	return d, ok, err
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	err = tmp.Chmod(0o600)
	if err == nil {
		_, err = tmp.Write(append(data, '\n'))
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

func readJSONFile(path string, v any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}
