package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pushcred"
	"github.com/sachiniyer/agent-factory/log"
)

// The automatic-push fence (#5571 W7.8, the owner's answer to the plan's Q4): af's
// own preserve-push (Lost recovery, limit failover) must never get the operator's
// GitHub token, so W7.8 changes no push af makes on its own.

// pushCredentialState is the Manager's push-credential state.
type pushCredentialState struct {
	// source reads the daemon box's token.
	source pushcred.Source
	// tokenFor overrides source.Token in tests; nil in production.
	tokenFor func(ctx context.Context, o pushcred.Origin) (string, error)
	// fencePath overrides where the fences persist (tests); "" means
	// <AF home>/push-credential-fences.json.
	fencePath string

	mu sync.Mutex
	// loaded is set only by a SUCCESSFUL read: an unreadable record is retried on
	// the next use, and until it reads, every request is refused and every
	// automatic push blocked (fail closed) — an unreadable record may be hiding a
	// fence. Repairing the file restores service without a restart.
	loaded bool
	// autoPushing counts, per session id, the automatic preserve-pushes this
	// process is running right now (preserveSandboxBeforeReap).
	autoPushing map[string]int
	// fences maps a session id to the CALLBACK IDENTITY (the W3.6 ref) its runtime
	// had when an automatic preserve-push STARTED, until that push is confirmed to
	// have succeeded. The in-sandbox archive takes no context and runs its git
	// steps with no deadline, so after a timeout, a lost reply or a daemon restart
	// it may still reach its push; no elapsed time proves it will not. A fence
	// therefore lasts as long as that runtime does: until a replacement runtime is
	// minted a new ref, or the row is gone.
	//
	// The ref, not the live token: the ref is what the runtime's credential is
	// derived from, so it exists even while the daemon holds no token for the row
	// (before a reattach re-registers one), and the token re-derived later is
	// still fenced. PERSISTED, and written before the push starts, because a
	// daemon restart re-derives the very same credential.
	fences map[string]pushFence
	// writes counts the fences this process has written; each takes the next
	// number, so a snapshot can tell the fences written before it from those
	// written after (pushRefSnapshot).
	writes uint64

	// inflight holds the creates the operator did not place while they provision
	// (holdInflightCreate), under inflightMu.
	inflightMu sync.Mutex
	inflight   map[string]inflightCreate
}

// holdInflight records c under id until the returned func runs.
func (s *pushCredentialState) holdInflight(id string, c inflightCreate) (release func()) {
	s.inflightMu.Lock()
	if s.inflight == nil {
		s.inflight = map[string]inflightCreate{}
	}
	s.inflight[id] = c
	s.inflightMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.inflightMu.Lock()
			delete(s.inflight, id)
			s.inflightMu.Unlock()
		})
	}
}

// inflightCreates is a copy of the creates being held.
func (s *pushCredentialState) inflightCreates() map[string]inflightCreate {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	out := make(map[string]inflightCreate, len(s.inflight))
	for id, c := range s.inflight {
		out[id] = c
	}
	return out
}

// pushFence is one persisted fence.
type pushFence struct {
	CallbackRef string    `json:"callback_ref"`
	At          time.Time `json:"at"`
	// seq is the write number this process gave the fence: 0 for one read from
	// disk, which predates every snapshot this process takes. Not persisted.
	seq uint64
}

const pushFenceFileName = "push-credential-fences.json"

// pushRefSnapshot is every tracked session's runtime callback identity, and the
// last fence write before any of it was read. A snapshot is evidence only about
// fences written by then: a later one may name a runtime the snapshot never saw,
// and pruning against it would delete a live fence (Codex P2 on #6433, two
// sessions recovering at once). Ordered by write number, not by clock, so a clock
// step cannot make a newer fence look older.
type pushRefSnapshot struct {
	refs map[string]string
	upTo uint64
}

// writeMark is the number of the last fence written so far.
func (s *pushCredentialState) writeMark() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

func (s *pushCredentialState) path() (string, error) {
	if s.fencePath != "" {
		return s.fencePath, nil
	}
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pushFenceFileName), nil
}

// loadLocked reads the persisted fences, until one read succeeds. Caller holds
// s.mu. A failure is returned (and logged) and the next call tries again.
func (s *pushCredentialState) loadLocked() error {
	if s.loaded {
		return nil
	}
	path, err := s.path()
	if err != nil {
		return err
	}
	fences := map[string]pushFence{}
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		err = nil
	case err == nil:
		err = json.Unmarshal(data, &fences)
	}
	if err != nil {
		err = fmt.Errorf("reading %s: %w", path, err)
		log.ErrorLog.Printf("push credentials are withheld from every sandbox session until %s can be read (it records which sessions an automatic preserve-push fences): %v", path, err)
		return err
	}
	if fences == nil { // a record holding JSON null
		fences = map[string]pushFence{}
	}
	s.fences, s.loaded = fences, true
	return nil
}

// persistLocked writes the fences, 0600, atomically, dropping the entries that
// can no longer matter. A fence is moot only once its session's runtime has a
// DIFFERENT callback identity: a replacement was minted a new ref, so the fenced
// runtime's credential can never be derived again. Age is never a reason, because
// the abandoned archive's git steps have no deadline.
//
// snap is the evidence, and only for fences written BEFORE it was taken; a nil
// snapshot prunes nothing. A session absent from it proves nothing (a row being
// restored, a daemon still starting) and keeps its fence. Caller holds s.mu.
func (s *pushCredentialState) persistLocked(snap *pushRefSnapshot) error {
	if snap != nil {
		for id, f := range s.fences {
			if cur := snap.refs[id]; cur != "" && cur != f.CallbackRef && f.seq <= snap.upTo {
				delete(s.fences, id)
			}
		}
	}
	path, err := s.path()
	if err != nil {
		return err
	}
	data, err := json.Marshal(s.fences)
	if err != nil {
		return err
	}
	return config.AtomicWriteFileRefusingLink(path, data, 0o600)
}

// beginAutoPush fences sessionID for an automatic preserve-push, PERSISTED
// before the push may start, against callbackRef (the runtime's callback
// identity; "" when the runtime has no callback, so no credential is possible to
// fence). The returned end lifts the fence only when called with confirmed=true;
// any other ending leaves it. An error means the fence could not be recorded, and
// the caller must not start the push.
//
// snapshot reads the identities every write prunes against. It is called afresh
// for each write — at the start, and again at a confirmed end, never reusing the
// first across the push (second security review of #6433, N2) — and always
// before s.mu is taken, since it reads s.writes itself. A fence written between
// the snapshot and the write is newer than it and is kept (pushRefSnapshot).
func (s *pushCredentialState) beginAutoPush(sessionID, callbackRef string, snapshot func() *pushRefSnapshot) (end func(confirmed bool), err error) {
	snap := takeSnapshot(snapshot)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	if s.autoPushing == nil {
		s.autoPushing = map[string]int{}
	}
	if callbackRef != "" {
		prev, had := s.fences[sessionID]
		s.writes++
		s.fences[sessionID] = pushFence{CallbackRef: callbackRef, At: time.Now(), seq: s.writes}
		if err := s.persistLocked(snap); err != nil {
			if had {
				s.fences[sessionID] = prev
			} else {
				delete(s.fences, sessionID)
			}
			return nil, err
		}
	}
	s.autoPushing[sessionID]++
	var once sync.Once
	return func(confirmed bool) {
		once.Do(func() { s.endAutoPush(sessionID, callbackRef, confirmed, snapshot) })
	}, nil
}

// takeSnapshot calls snapshot when there is one; nil prunes nothing.
func takeSnapshot(snapshot func() *pushRefSnapshot) *pushRefSnapshot {
	if snapshot == nil {
		return nil
	}
	return snapshot()
}

func (s *pushCredentialState) endAutoPush(sessionID, callbackRef string, confirmed bool, snapshot func() *pushRefSnapshot) {
	var snap *pushRefSnapshot
	if confirmed {
		snap = takeSnapshot(snapshot)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.autoPushing[sessionID]--; s.autoPushing[sessionID] <= 0 {
		delete(s.autoPushing, sessionID)
	}
	if !confirmed || s.autoPushing[sessionID] > 0 || callbackRef == "" {
		return
	}
	if f, ok := s.fences[sessionID]; ok && f.CallbackRef == callbackRef {
		delete(s.fences, sessionID)
		// Pruned against the identities current NOW, not those read when the push
		// began.
		if err := s.persistLocked(snap); err != nil {
			// The fence stays on disk, which only withholds credentials longer:
			// the safe side to fail on.
			log.WarningLog.Printf("session %s: could not lift its push-credential fence after a confirmed automatic push: %v", sessionID, err)
		}
	}
}

// autoPushFenced reports whether sessionID, whose runtime's callback identity is
// callbackRef, is inside an automatic preserve-push's fence. An unreadable record
// fences everything.
func (s *pushCredentialState) autoPushFenced(sessionID, callbackRef string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadLocked() != nil || s.autoPushing[sessionID] > 0 {
		return true
	}
	f, ok := s.fences[sessionID]
	return ok && callbackRef != "" && f.CallbackRef == callbackRef
}

// callbackRefSnapshot maps every tracked session to its runtime's callback
// identity: the snapshot a fence write prunes against. The write mark is taken
// FIRST, so a fence written while the refs are being read is newer than the
// snapshot and is kept. The instances are collected under m.mu and read outside
// it, so this never holds the manager lock across an instance lock.
func (m *Manager) callbackRefSnapshot() *pushRefSnapshot {
	upTo := m.pushCreds.writeMark()
	insts := m.trackedInstances()
	out := make(map[string]string, len(insts))
	for _, inst := range insts {
		if p, ok := instancePushPlacement(inst); ok && p.CallbackRef != "" {
			out[inst.ID] = p.CallbackRef
		}
	}
	return &pushRefSnapshot{refs: out, upTo: upTo}
}
