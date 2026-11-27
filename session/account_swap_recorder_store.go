package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/log"
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
//
// Two edges bound the file's life (#7883, #7897). At the START, a record whose
// bytes fail to decode or validate can vouch for nothing: the install moves it
// aside under a uniquely-named *.corrupt-* sibling (earlier evidence is never
// renamed over) and the recorder starts empty — anonymous "none", the same
// answer a missing record gives — rather than refusing to start and wedging
// the session until the file is deleted by hand. A READ error never examined
// the bytes, so the file stays put: a transient fault must not permanently
// set aside a record that may be fine. At the END, the record belongs to one
// session generation — the daemon's session ROW — so the explicit Kill RPC
// (af sessions kill, archive teardown, create-cleanup — the handler in
// daemon/agentserver_headless.go) retires it with the row: a session
// re-created on the same (repo, title) key never inherits a verdict a dead
// generation reached. Every other path that ends the process keeps it: a
// crashed server never ran Kill at all, and a signal-driven kill — a host
// reboot's SIGTERM, a restart's pre-launch stopUnadoptedServer — leaves the
// daemon's row alive, so the reattach restarts the server in this session
// dir and rehydrates it (#7862). The mktemp reap's own rm -rf covers the
// SIGTERM-then-reap case.

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

// accountSwapRecordCorrupt marks the load error whose file was read and whose
// bytes then failed to decode or validate — the ONLY error class whose
// evidence the caller may move aside (#7897): a plain read error (EIO,
// EACCES, EMFILE) never examined the file, so a transient fault must not
// permanently set aside a record that may be fine — it starts the recorder
// empty and leaves the file in place instead.
type accountSwapRecordCorrupt struct{ err error }

func (e *accountSwapRecordCorrupt) Error() string { return e.err.Error() }
func (e *accountSwapRecordCorrupt) Unwrap() error { return e.err }

// load rehydrates the recorder's durable state a previous process left behind.
// A missing file is not an error: a fresh agent-server has recorded nothing,
// and the recorder stays at its zero value — answering swapOutcomeNone exactly
// as a recorder with no store does. A file that cannot be read is a plain
// error (the caller starts the recorder empty and leaves it in place); a file
// that reads but fails to decode or validate is an *accountSwapRecordCorrupt
// (the caller moves it aside under a uniquely-named sibling) — either way the
// recorder starts empty rather than refusing to start (#7883, #7897).
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
		return accountSwapRecordData{}, &accountSwapRecordCorrupt{fmt.Errorf("parse account swap record %s: %w", s.path, err)}
	}
	if !validAccountSwapRecordData(d) {
		return accountSwapRecordData{}, &accountSwapRecordCorrupt{fmt.Errorf("account swap record %s does not name a state the recorder persists (outcome %q, id %q)", s.path, d.Outcome, d.ID)}
	}
	return d, nil
}

// validAccountSwapRecordData reports whether a decoded record names a state
// the recorder itself could have persisted — exactly the states `run` and the
// discard persist (#7897): `pending` is run's pre-rotation checkpoint and the
// three verdicts are terminal, and every write carries the swap's own id, so
// `none` (status reports it; no write produces it), an empty id, and any
// verdict this build cannot interpret are all corrupt for vouching purposes.
// The strictness matters: a "none" carrying an id would rehydrate into the
// reconcile's matching-id default arm and fence a proven row forever — the
// same wedge class an uninterpretable verdict sits in (#7883).
func validAccountSwapRecordData(d accountSwapRecordData) bool {
	switch d.Outcome {
	case swapOutcomePending, swapOutcomeCommitted, swapOutcomeRolledBack, swapOutcomeUnknown:
		return d.ID != ""
	}
	return false
}

// quarantineCorrupt moves a record that failed to load aside — renamed under a
// unique <name>.corrupt-* sibling — rather than deleting it: the evidence of
// what the file held survives for inspection, and under its new name no load
// ever reads it again, so every later start rehydrates nothing (#7883). The
// name is claimed atomically BEFORE the rename (CreateTemp's O_EXCL), so a
// second corruption can never rename over the evidence an earlier one left
// (#7897). The directory is synced so the rename survives a crash of the
// process that follows. Best-effort: a failure only logs at the caller — the
// recorder still starts empty and the file stays put.
func (s *accountSwapStore) quarantineCorrupt() error {
	aside, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".corrupt-*")
	if err != nil {
		return fmt.Errorf("claim a name to move the corrupt account swap record %s aside: %w", s.path, err)
	}
	asidePath := aside.Name()
	if err := aside.Close(); err != nil {
		_ = os.Remove(asidePath)
		return fmt.Errorf("prepare a name to move the corrupt account swap record %s aside: %w", s.path, err)
	}
	// The rename replaces the placeholder WE just created — unique and ours —
	// never another quarantine's evidence.
	if err := os.Rename(s.path, asidePath); err != nil {
		_ = os.Remove(asidePath)
		return fmt.Errorf("move the corrupt account swap record %s aside: %w", s.path, err)
	}
	return syncDir(filepath.Dir(s.path))
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
		// A record that cannot be read, decoded, or interpreted can vouch for
		// nothing (#7883): the recorder starts EMPTY — answering anonymous
		// "none", the same answer a missing record gives — so every later
		// reconcile takes master's probe-and-discard path and settles only what
		// the host's own markers prove. It is never rehydrated as `unknown`:
		// an anonymous unknown asserts an indeterminate verdict the corrupt
		// record cannot prove AND fences every reconcile it meets, the #7858
		// forever-fence in a new shape. Refusing to start here wedged the
		// sandbox session — the only way out was deleting the file by hand.
		// Only bytes that were read and then failed to decode or validate are
		// moved aside (uniquely-named, never clobbering earlier evidence); a
		// READ error never examined the file, so it stays in place — a
		// transient fault must not permanently set aside a record that may be
		// fine (#7897).
		var corrupt *accountSwapRecordCorrupt
		if errors.As(err, &corrupt) {
			log.WarningLog.Printf("%v; the recorder starts with nothing it can vouch for, and the record is moved aside", err)
			if qerr := store.quarantineCorrupt(); qerr != nil {
				log.WarningLog.Printf("account swap record: %v", qerr)
			}
		} else {
			log.WarningLog.Printf("%v; the recorder starts with nothing it can vouch for, and the unread record is left in place", err)
		}
		data = accountSwapRecordData{}
	}
	i.mu.Lock()
	i.durableAccountSwapStore = store
	i.durableAccountSwapRecord = data
	i.mu.Unlock()
	return nil
}

// RetireDurableAccountSwapRecord removes the recorder's durable record when
// the session ROW it serves is removed (#7897): the record belongs to one
// session generation — the row's workspace lifetime — so it leaves with the
// row rather than outliving it for a session re-created on the same
// (repo, title) key to inherit a verdict a dead generation reached. The
// agent-server's explicit Kill RPC is the only caller — af sessions kill,
// archive teardown, and create-cleanup all land in the handler in
// daemon/agentserver_headless.go. Every signal-driven path into the same
// backend kill — a host reboot's SIGTERM, a restart's pre-launch
// stopUnadoptedServer — leaves the daemon's row alive, and the reattach
// restarts the server in this same session dir, which must rehydrate the
// record (#7862); the mktemp reap's own rm -rf covers the SIGTERM-then-reap
// case. A missing store or file is a no-op, and a removal failure is only
// logged: the teardown stands.
func (i *Instance) RetireDurableAccountSwapRecord() {
	i.mu.RLock()
	store := i.durableAccountSwapStore
	i.mu.RUnlock()
	if store == nil {
		return
	}
	if err := store.clear(); err != nil {
		log.WarningLog.Printf("account swap record: the session row's teardown could not retire the durable record: %v", err)
	}
}
