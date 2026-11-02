package tmux

import (
	"bytes"
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
	// one as death (#6339). A generation is added only when the socket path
	// changes — an environment change; a same-environment restart reuses the
	// socket — and entries are never pruned by inferred death: proving a
	// socket generation dead is hard, and a dead server still answers
	// "no server running" definitively, so retention costs doctor a bounded
	// listing while dropping a live one hides its sessions. The list is
	// capped at maxPriorSocketGenerations newest generations; exceeding the
	// cap marks HistoryTruncated rather than silently losing coverage.
	PriorSockets []string `json:"prior_sockets,omitempty"`
	// HistoryTruncated records that the PriorSockets cap dropped a socket
	// generation whose file still existed — one the record can therefore no
	// longer account for. Once set it never clears: the dropped server may
	// still host sessions, so every later record inherits the admission that
	// its history is incomplete. Doctor refuses any destructive reading that
	// needs complete socket coverage while this is set (#6339).
	HistoryTruncated bool `json:"history_truncated,omitempty"`
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
	prev, ok, err := ReadServerState(home)
	switch {
	case err != nil:
		// The previous record could not be read — the lineage's socket
		// history is unrecoverable here, so admit incomplete coverage rather
		// than write a record that claims none ever existed.
		st.HistoryTruncated = true
	case ok:
		priors, truncated := priorSocketGenerations(prev, st.Socket)
		st.PriorSockets = priors
		st.HistoryTruncated = st.HistoryTruncated || prev.HistoryTruncated || truncated
	case !ok && (lastServerState.Mode != "" || lastServerState.Socket != ""):
		// The record file vanished under this live process — a state-dir
		// cleanup, a lost file, or any path that removes it while the daemon
		// keeps running. The lineage's socket history is unrecoverable from
		// disk, but this process still holds its last written record in
		// lastServerState: carry that forward (moving the last recorded
		// socket into history when the daemon has moved on) rather than write
		// a record that claims a complete, empty history and drops every
		// socket generation the daemon has used (#6339). Without this arm the
		// fall-through writes a falsely complete record stamped with the live
		// daemon's pid, and doctor's record-only death proof reads a live
		// session on a dropped prior socket as dead and SIGKILLs its
		// processes. A genuinely fresh lineage (zero lastServerState) is a
		// true first record with trivially complete history and falls through
		// unchanged.
		priors, truncated := priorSocketGenerations(lastServerState, st.Socket)
		st.PriorSockets = priors
		st.HistoryTruncated = st.HistoryTruncated || lastServerState.HistoryTruncated || truncated
	case !ok && priorDaemonGenerationEvidence(home):
		// The record file is gone and this process holds no lineage — the
		// cross-restart residual of the arm above (#6610): a restarted daemon
		// begins with a zeroed lastServerState, so a record deleted BETWEEN
		// generations leaves it nothing to carry. The home still keeps the
		// proof an earlier generation ran — committed session rows, pending
		// creates, the server-death marker — so the lineage's socket history
		// is unrecoverable and the record must admit that rather than write
		// the complete, empty history doctor would spend on a kill. Ordered
		// after the carry-forward: a live process's own lineage is the better
		// source, so this asks only what the process cannot answer.
		st.HistoryTruncated = true
	}
	if st.Mode == lastServerState.Mode && st.Unit == lastServerState.Unit &&
		st.Socket == lastServerState.Socket && st.Reason == lastServerState.Reason &&
		st.DaemonPID == lastServerState.DaemonPID &&
		st.HistoryTruncated == lastServerState.HistoryTruncated &&
		slices.Equal(st.PriorSockets, lastServerState.PriorSockets) {
		return
	}
	if err := writeJSONFile(filepath.Join(home, serverStateFileName), st); err != nil {
		return
	}
	lastServerState = st
}

// maxPriorSocketGenerations caps the socket history the record carries
// (#6339). A generation is added only per socket move; without a bound a
// daemon restarted under many different TMUX_TMPDIRs would grow the file
// without limit. Eight still accounts for every socket a realistic lineage
// leaves alive — and exceeding it sets HistoryTruncated on the record, which
// blinds doctor's record-only death proofs rather than letting a dropped
// generation read as accounted for.
const maxPriorSocketGenerations = 8

// priorSocketGenerations returns prev's socket lineage with prev's own socket
// moved into history when the daemon has moved on to current — deduplicated
// by path, oldest first, bounded. No generation is ever dropped by inferred
// death: a dead server still answers "no server running" definitively, so
// doctor lists it as an authoritative empty, while a wrong guess at death
// would hide its live sessions entirely. When more than
// maxPriorSocketGenerations generations accumulate, the oldest are dropped —
// the newest are the sockets a live leftover server is most likely still
// answering on — and the truncation is reported so doctor knows the history
// it can account for is incomplete.
func priorSocketGenerations(prev ServerState, current string) ([]string, bool) {
	seen := map[string]bool{current: true}
	var out []string
	// prev.PriorSockets is already oldest-first; prev.Socket — the socket the
	// previous record named — is the NEWEST generation and joins at the tail.
	for _, s := range append(slices.Clone(prev.PriorSockets), prev.Socket) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	truncated := false
	if len(out) > maxPriorSocketGenerations {
		out = out[len(out)-maxPriorSocketGenerations:]
		truncated = true
	}
	return out, truncated
}

// processStartedAt bounds "an earlier generation" for timestamped evidence:
// a marker file whose mtime predates this process's start could only have
// been written by a generation before this one, while a file written since
// is this generation's own work in flight. Package init runs within
// milliseconds of process start — far inside the window a generation
// boundary matters at.
var processStartedAt = time.Now()

// priorDaemonGenerationEvidence reports whether the AF home persists proof
// that an earlier daemon generation ran here — the residue a deleted
// server-state record can no longer speak for (#6610). It is consulted only
// when the record file is absent AND this process holds no in-memory
// lineage, so a genuinely fresh home must answer false: every source below
// is something only an af daemon generation could have created — nothing a
// user, an install, or a non-daemon af process produces. Reads that fail
// resolve toward evidence ("I could not tell" never resolves to "complete"),
// the same direction the unreadable-record arm takes.
//
// Why these sources and not the alternatives: daemon.pid is rewritten by the
// CURRENT daemon before it ever records the tmux state, so it only ever
// names the writing generation; state.json is UI/config state with no
// generation binding. Persisted instance rows carry a tmux session name but
// no socket path, so they cannot say WHICH generation's socket a session
// outlived — only that a generation existed whose lineage is now lost. That
// is precisely the fact the truncated admission exists to record.
func priorDaemonGenerationEvidence(home string) bool {
	generationEvidence.Lock()
	defer generationEvidence.Unlock()
	if v, ok := generationEvidence.byHome[home]; ok {
		return v
	}
	v := evaluatePriorDaemonGenerationEvidence(home)
	generationEvidence.byHome[home] = v
	return v
}

// generationEvidence latches the prior-generation verdict per home the first
// time this process evaluates it. The latch is load-bearing, not a cache for
// speed: ensureDedicatedServer evaluates eagerly before it can launch the
// dedicated-server wrapper, because this generation's own writes — the
// wrapper's death marker overwriting tmux-server-death.json on exit, a sweep
// deleting stale pending-create markers — would otherwise erase the residue
// before a record write could read it (#6610). Residue can only appear
// before the generation starts, so a latched verdict never goes stale.
var generationEvidence = struct {
	sync.Mutex
	byHome map[string]bool
}{byHome: map[string]bool{}}

func evaluatePriorDaemonGenerationEvidence(home string) bool {
	if priorGenerationInstanceRows(home) {
		return true
	}
	// pending-creates journals an in-flight session create (#6301): a marker
	// left behind means a session may have stranded live without ever
	// committing an instance row. Only a marker predating this process
	// counts: this generation's own first create writes its marker before
	// its first record — counting that would mark a genuinely fresh home
	// truncated forever. ReadDir rather than Glob: the home path itself can
	// contain glob metacharacters, and a listing failure must answer
	// evidence, not silence.
	markers, err := os.ReadDir(filepath.Join(home, "pending-creates"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return true
		}
	} else {
		for _, e := range markers {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil || !info.ModTime().After(processStartedAt) {
				return true
			}
		}
	}
	// The server-death marker is written only by an af-owned dedicated
	// server's wrapper at exit (#5429). One predating this process proves a
	// server generation already ran; one written since is this generation's
	// own bootstrap crash — that server held no sessions yet, so it is not
	// evidence of a lost lineage. That crash also overwrites the marker it
	// replaces, which is why this read is latched at bootstrap entry rather
	// than re-run after it.
	if info, err := os.Stat(filepath.Join(home, serverDeathFileName)); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return true
		}
	} else if !info.ModTime().After(processStartedAt) {
		return true
	}
	return false
}

// priorGenerationInstanceRows reports whether any repo's instances.json
// under the home holds at least one committed session row — a session only
// a live daemon generation could have created. An instances directory that
// cannot be read, and a file that exists but cannot be decoded, count as
// evidence: af owns the path, so a shape this read cannot disprove is never
// proof that no generation ran.
//
// Unlike the markers above, rows count unconditionally — not gated on
// predating this process. A committed row can only exist after a
// recordServerState write succeeded inside the create flow, so on a home
// whose record never persisted this process could not have produced one;
// the only same-generation shape that reaches here is "this generation's
// own earlier record writes failed", which is itself a lost lineage.
func priorGenerationInstanceRows(home string) bool {
	instancesDir := filepath.Join(home, "instances")
	entries, err := os.ReadDir(instancesDir)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(instancesDir, e.Name(), "instances.json"))
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return true
		case instanceRowsPresent(data):
			return true
		}
	}
	return false
}

// instanceRowsPresent decodes one repo's instances.json for the row count
// only. Two on-disk shapes count: the current
// {"schema_version":N,"instances":[...]} envelope and the legacy array root
// it wrapped (#3652). Anything that is not a decodable instance set counts
// as evidence rather than as emptiness.
func instanceRowsPresent(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '[' {
		var rows []json.RawMessage
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return true
		}
		return len(rows) > 0
	}
	var envelope struct {
		SchemaVersion int             `json:"schema_version"`
		Instances     json.RawMessage `json:"instances"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return true
	}
	// Only a shape decodeInstancesEnvelope accepts can read as "no rows":
	// schema_version 1 carrying an explicitly present, decodable instances
	// array. An envelope missing either member — {"schema_version":1} alone —
	// or naming a version this binary cannot decode is a shape it cannot
	// disprove, and the version mismatch is itself proof a different af
	// generation ran (#6610).
	if envelope.SchemaVersion != 1 || len(envelope.Instances) == 0 {
		return true
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(envelope.Instances, &rows); err != nil {
		return true
	}
	return len(rows) > 0
}

// RecordServerStateForTest invokes recordServerState so tests outside this
// package (doctor's record-only death proof) can drive the daemon's real
// record-write path — the only way to prove what the record says once the
// in-memory lineage is gone (#6610).
func RecordServerStateForTest(home string, st ServerState) {
	recordServerState(home, st)
}

// ResetServerStateLineageForTest clears the in-memory lastServerState and
// the latched generation verdicts under the mutex, replaying the "new
// process, empty lineage" state a restarted daemon begins with.
func ResetServerStateLineageForTest() {
	serverStateMu.Lock()
	lastServerState = ServerState{}
	serverStateMu.Unlock()
	generationEvidence.Lock()
	generationEvidence.byHome = map[string]bool{}
	generationEvidence.Unlock()
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
