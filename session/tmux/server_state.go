package tmux

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// ServerModeAdopted: the daemon found a tmux server already running on its
	// socket and attached its sessions to it rather than launching a dedicated
	// one (#6339). The server is not af-owned and keeps its own lifecycle —
	// af never mutates it — but the socket is still where the daemon's
	// sessions live, and recording it is what lets doctor pin its reads to
	// the server this daemon generation actually uses.
	ServerModeAdopted ServerMode = "adopted"
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
	// DaemonPID is the pid of the daemon generation that wrote this record
	// (#6339). Doctor can prove the record is CURRENT only when a daemon
	// serving this home still runs under the stamped pid — a restart that
	// adopts an already-running server stamps a new record, so a record
	// carrying a dead or mismatched pid is one a previous generation left
	// behind and may name a socket the live daemon no longer uses. A record
	// written before this field existed carries zero and is trusted only when
	// no daemon is live to contradict it.
	DaemonPID int `json:"daemon_pid,omitempty"`
	// PriorSockets lists every earlier socket this daemon lineage recorded,
	// oldest first. A restart under a different TMUX_TMPDIR moves the daemon
	// to a new socket while the previous server and its sessions can stay
	// alive; the record must account for every socket generation the daemon
	// has used before doctor may read a session's absence from the current
	// one as death (#6339).
	PriorSockets []string `json:"prior_sockets,omitempty"`
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

// DefaultSocketPath is the socket a bare `tmux` run by this process resolves
// to — the very socket the ambient `tmux ls` (no -S/-L) lists, derived the way
// tmux itself resolves it: $TMUX's socket when this process is inside a tmux
// session, else ${TMUX_TMPDIR:-/tmp}/tmux-<uid>/default.
//
// It is also the identity the dedicated server's unit name is derived from.
//
// Exported for `af doctor`, whose orphaned-process kill arm must compare this
// ambient socket against the socket a marked process's own TMUX env names:
// only when they agree can an ambient listing's absence of a session be read
// as proof that session is dead (see doctor/tmux_sessions.go). Doctor is a
// separate process from the daemon and can resolve a DIFFERENT socket, so a
// bare `tmux ls` and a marked process's $TMUX can name different servers —
// the wrong-server divergence this comparison closes for the record-absent
// state.
func DefaultSocketPath() string {
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
// the timestamp changed (the fallback path runs on every session spawn). Every
// record is stamped with this daemon's pid — the generation marker doctor uses
// to prove the record is current — and carries forward every socket generation
// the previous record knew about, so a restart that moves the daemon to a new
// socket leaves the daemon's full socket history accountable (#6339).
func recordServerState(home string, st ServerState) {
	serverStateMu.Lock()
	defer serverStateMu.Unlock()
	st.DaemonPID = os.Getpid()
	if prev, ok, err := ReadServerState(home); err == nil && ok {
		st.PriorSockets = priorSocketGenerations(prev, st.Socket)
	}
	if st.Mode == lastServerState.Mode && st.Unit == lastServerState.Unit &&
		st.Socket == lastServerState.Socket && st.Reason == lastServerState.Reason &&
		st.DaemonPID == lastServerState.DaemonPID &&
		slices.Equal(st.PriorSockets, lastServerState.PriorSockets) {
		return
	}
	if err := writeJSONFile(filepath.Join(home, serverStateFileName), st); err != nil {
		return
	}
	lastServerState = st
}

// priorSocketGenerations returns prev's socket lineage with prev's own socket
// moved into history when the daemon has moved on to current — deduplicated,
// oldest first. When prev.Socket still equals current it is filtered out by
// the seen set, so the result is simply prev's carried-forward history.
func priorSocketGenerations(prev ServerState, current string) []string {
	seen := map[string]bool{current: true}
	var out []string
	for _, s := range append([]string{prev.Socket}, prev.PriorSockets...) {
		if s != "" && !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
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
