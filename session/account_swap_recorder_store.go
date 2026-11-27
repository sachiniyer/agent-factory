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
// Two edges bound the file's life (#7883, #7897). At the START, an entry that
// is not a regular file, or whose bytes fail to decode or validate, can vouch
// for nothing: the install moves it aside under a uniquely-named *.corrupt-*
// sibling (earlier evidence is never renamed over) and the recorder starts
// empty — anonymous "none", the same answer a missing record gives — rather
// than refusing to start and wedging the session until the file is deleted by
// hand. A READ error on a regular file never examined the bytes, so the file
// stays put: a transient fault must not permanently set aside a record that
// may be fine. It survives only until the recorder's next write, whose atomic
// rename replaces it. At the END, the record is retired at the explicit Kill
// RPC — the handler in daemon/agentserver_headless.go (#7897). Row-removal
// kills (af sessions kill, create-cleanup) and archive teardown both call it;
// archive keeping its row is safe because archive refuses a session with an
// in-flight swap (daemon/archive.go). Every other path that ends the process
// keeps it: a crashed server never ran Kill at all, and a signal-driven kill
// — a host reboot's SIGTERM, a restart's pre-launch stopUnadoptedServer —
// survives for the reattach's restart to rehydrate in this same dir (#7862);
// the mktemp reap's own rm -rf covers the SIGTERM-then-reap case. A record
// can also outlive its row — af reset, or a kill a dead server never answered
// — which is harmless: it sits in a per-generation mktemp dir no later
// generation reuses, so nothing loads it into a new generation's recorder.

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
// as a recorder with no store does. A non-regular entry is corrupt — never
// opened, so a FIFO cannot hang the startup that reads it — and moved aside;
// a regular file that cannot be READ is a plain error (the caller starts the
// recorder empty and leaves it in place); one that reads but fails to decode
// or validate is an *accountSwapRecordCorrupt (the caller moves it aside under
// a uniquely-named sibling) — either way the recorder starts empty rather
// than refusing to start (#7883, #7897).
func (s *accountSwapStore) load() (accountSwapRecordData, error) {
	fi, err := os.Lstat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return accountSwapRecordData{}, nil
		}
		return accountSwapRecordData{}, fmt.Errorf("stat account swap record %s: %w", s.path, err)
	}
	if !fi.Mode().IsRegular() {
		return accountSwapRecordData{}, &accountSwapRecordCorrupt{fmt.Errorf("account swap record %s is not a regular file (mode %s)", s.path, fi.Mode().Type())}
	}
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
// the recorder itself could have persisted — the validator and the writer
// must agree (#7897 review): `pending` is run's pre-rotation checkpoint, the
// three verdicts are terminal, and "" is what a discardStaged on a fresh
// recorder persists. The id is unchecked: an unmarked shared-scope sweep
// discards with id "" and a pre-identity daemon's swap omits swap_id, so the
// writer persists id-less "" / verdict / checkpoint states on routine paths —
// rejecting them would quarantine this build's own writes (a regression from
// master, which loads them). `none` is the one outcome the writer never
// persists — status reports it; no write produces it — so a file naming it,
// id or not, is corrupt: a "none" carrying an id would rehydrate into the
// reconcile's matching-id arm with no settle arm and fence a proven row
// forever — the same wedge class an uninterpretable verdict sits in (#7883).
func validAccountSwapRecordData(d accountSwapRecordData) bool {
	switch d.Outcome {
	case "", swapOutcomePending, swapOutcomeCommitted, swapOutcomeRolledBack, swapOutcomeUnknown:
		return true
	}
	return false
}

// quarantineCorrupt moves a record that failed to load aside — renamed under a
// unique <name>.corrupt-* sibling — rather than deleting it: the evidence of
// what the file held survives for inspection, and under its new name no load
// ever reads it again, so every later start rehydrates nothing (#7883). The
// name is claimed atomically BEFORE the rename (CreateTemp's O_EXCL) and this
// function is the only producer of *.corrupt-* siblings, so a second
// corruption can never rename over the evidence an earlier one left (#7897).
// The placeholder is released before the rename because rename(2) cannot move
// a non-regular entry — a directory, a FIFO — over an existing file. The
// directory is synced so the rename survives a crash of the process that
// follows. Best-effort: a failure only logs at the caller — the recorder
// still starts empty and the entry stays put.
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
	if err := os.Remove(asidePath); err != nil {
		return fmt.Errorf("release the claimed name to move the corrupt account swap record %s aside: %w", s.path, err)
	}
	if err := os.Rename(s.path, asidePath); err != nil {
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
		// Only a non-regular entry or bytes that were read and then failed to
		// decode or validate are moved aside (uniquely-named, never clobbering
		// earlier evidence); a READ error never examined the file, so it stays
		// in place — a transient fault must not permanently set aside a record
		// that may be fine. It survives only until the recorder's next write,
		// whose atomic rename replaces it (#7897).
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

// RetireDurableAccountSwapRecord removes the recorder's durable record at the
// explicit Kill RPC — the handler in daemon/agentserver_headless.go is the
// only caller (#7897): af sessions kill and create-cleanup remove the session
// row there, and archive teardown retires on a row it keeps — safe because
// archive refuses a session with an in-flight swap (daemon/archive.go). Every
// signal-driven path into the same backend kill — a host reboot's SIGTERM, a
// restart's pre-launch stopUnadoptedServer — keeps the record: the reattach
// restarts the server in this same session dir and rehydrates it (#7862), and
// the mktemp reap's own rm -rf covers the SIGTERM-then-reap case. A record
// that still outlives its row — af reset, a kill a dead server never answered
// — sits in a per-generation mktemp dir no later generation reuses, so it can
// never be loaded into a new generation's recorder. A missing store or file
// is a no-op, and a removal failure is only logged: the teardown stands.
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
