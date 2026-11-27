package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Durable terminal record + discard tombstone for the agent-server's swap
// recorder (#7793), persisted in the session's sandbox dir so a restarted
// agent-server rehydrates its recorder and can still attribute the prior
// verdict the durable in-flight record's restart reconcile asks for.
//
// The recorder is process-memory by default: the daemon's in-process local
// sessions have no agent-server process to restart, so no state to rehydrate.
// The in-sandbox agent-server process — the one whose restart the bug is about
// — installs this store at startup (RunAgentServer →
// InstallDurableAccountSwapRecord), rehydrating the recorder from the file
// before any swap request or status read can cross. The file lives under
// runtimetoken.SessionDir, the convention internal/runtimetoken/runtimetoken.go
// names for "state that must survive an agent-server restart but belongs to
// this one session", and is keyed by the same session identity the
// rebuild-teardown marker uses, so an installed-af agent-server that shares its
// session dir with other sessions keeps one record per session (#5842).
//
// The clear's restart-idempotency contract ("a record left on disk is
// re-reconciled by the next daemon, idempotent, because a reconcile of a settled
// swap reaches the same verdict") depends on this: a restarted agent-server
// whose recorder forgot every verdict answered anonymous "none", a settled host
// (no staged dir, no account.prev) probed scopeUnknown, and the re-reconcile of
// a provably settled swap returned indeterminate — fencing the row forever. A
// recorder that survives its own restart answers the recorded id's own verdict,
// so the re-reconcile reaches the same verdict the prior process did.

const accountSwapRecordPrefix = "account-swap-record-"

// accountSwapRecordData is the on-disk shape of the recorder's durable state:
// the terminal verdict the last swap reached under its own id, and the discard
// tombstone that bounds a late request to rolled_back.
type accountSwapRecordData struct {
	Outcome   string `json:"outcome"`
	Program   string `json:"program,omitempty"`
	ID        string `json:"id,omitempty"`
	Cancelled string `json:"cancelled,omitempty"`
}

// accountSwapRecordMarkerName is the per-session file name for the recorder's
// durable state, keyed by the same session identity the rebuild-teardown marker
// uses (RebuildTeardownKey), so an installed-af agent-server that shares its
// session dir with other sessions keeps one record per session (#5842).
func accountSwapRecordMarkerName(key string) string {
	return accountSwapRecordPrefix + key
}

// accountSwapStore reads and writes the recorder's durable state at a fixed
// path in the session dir.
type accountSwapStore struct {
	path string
}

func newAccountSwapStore(path string) *accountSwapStore {
	return &accountSwapStore{path: path}
}

// load rehydrates the recorder's durable state a previous process left behind.
// A missing file is not an error: a fresh agent-server has recorded nothing,
// and the recorder stays at its zero value — answering swapOutcomeNone exactly
// as a recorder with no store does.
func (s *accountSwapStore) load() (accountSwapRecordData, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return accountSwapRecordData{}, nil
		}
		return accountSwapRecordData{}, fmt.Errorf("read account swap record %s: %w", s.path, err)
	}
	var d accountSwapRecordData
	if err := json.Unmarshal(b, &d); err != nil {
		return accountSwapRecordData{}, fmt.Errorf("parse account swap record %s: %w", s.path, err)
	}
	return d, nil
}

// save atomically writes the recorder's durable state and syncs both the file
// and its directory, so a restart that follows sees the terminal verdict the
// prior process reached rather than a torn or absent write. The temp-file +
// rename + fsync is the discipline rebuild_marker.go uses for the same class of
// crash-stable session state.
func (s *accountSwapStore) save(d accountSwapRecordData) error {
	b, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode account swap record %s: %w", s.path, err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".account-swap-record-*")
	if err != nil {
		return fmt.Errorf("write account swap record %s: %w", s.path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("write account swap record %s: %w", s.path, err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write account swap record %s: %w", s.path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync account swap record %s: %w", s.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write account swap record %s: %w", s.path, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("commit account swap record %s: %w", s.path, err)
	}
	return syncDir(dir)
}

// clear removes the recorder's durable state so a restart rehydrates nothing
// — the way a fresh pre-fix recorder answered swapOutcomeNone and fenced. Used
// when a terminal save cannot land but the file can still be unlinked (e.g.
// ENOSPC: a create fails, an unlink still succeeds): leaving the prior swap's
// record on disk would let a restart rehydrate the wrong swap's verdict and
// settle a row/host account mismatch (#7793 review R1). A missing file is not
// an error — there is nothing to clear.
func (s *accountSwapStore) clear() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove account swap record %s: %w", s.path, err)
	}
	// Sync the containing directory so the unlink is durable before the
	// caller rotates the account — matching removeMarkerDurable's
	// crash-stability discipline. A machine crash after the rotation must
	// not resurrect the prior swap's record, which restart reconciliation
	// could otherwise mistake for a queued request and settle as a
	// rollback against a host that already committed the new account.
	return syncDir(filepath.Dir(s.path))
}

// InstallDurableAccountSwapRecord wires the agent-server's swap recorder to a
// durable file in the session's sandbox dir and rehydrates it from a previous
// process's terminal record, the way InstallDurableRebuildTeardown wires the
// rebuild-teardown marker. Called once at agent-server startup (RunAgentServer),
// before AgentServer() builds the localAgentServer whose recorder it backs, so
// the recorder rehydrates before any swap request or status read can cross.
//
// The key is the same session identity RebuildTeardownKey mints, so an
// installed-af agent-server that shares its session dir with other sessions
// keeps one record per session (#5842). A daemon's in-process local session
// never calls this — its recorder stays process-memory, with no agent-server
// process to restart.
func (i *Instance) InstallDurableAccountSwapRecord(dir, key string) error {
	if dir == "" {
		return fmt.Errorf("account swap record needs a session directory")
	}
	if !validMarkerKey(key) {
		return fmt.Errorf("account swap record needs a session key (RebuildTeardownKey), got %q", key)
	}
	store := newAccountSwapStore(filepath.Join(dir, accountSwapRecordMarkerName(key)))
	data, err := store.load()
	if err != nil {
		return fmt.Errorf("load the account swap record: %w", err)
	}
	i.mu.Lock()
	i.durableAccountSwapStore = store
	i.durableAccountSwapRecord = data
	i.mu.Unlock()
	return nil
}
