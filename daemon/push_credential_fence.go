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
	"github.com/sachiniyer/agent-factory/session"
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
}

// pushFence is one persisted fence.
type pushFence struct {
	CallbackRef string    `json:"callback_ref"`
	At          time.Time `json:"at"`
}

const pushFenceFileName = "push-credential-fences.json"

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
// the abandoned archive's git steps have no deadline. current maps a session to
// its runtime's ref now; a session absent from it proves nothing (a row being
// restored, a daemon still starting) and keeps its fence. Caller holds s.mu.
func (s *pushCredentialState) persistLocked(current map[string]string) error {
	for id, f := range s.fences {
		if cur := current[id]; cur != "" && cur != f.CallbackRef {
			delete(s.fences, id)
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
// the caller must not start the push. current is the per-session ref snapshot
// persisting uses to drop fences a replacement made moot.
func (s *pushCredentialState) beginAutoPush(sessionID, callbackRef string, current map[string]string) (end func(confirmed bool), err error) {
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
		s.fences[sessionID] = pushFence{CallbackRef: callbackRef, At: time.Now()}
		if err := s.persistLocked(current); err != nil {
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
		once.Do(func() { s.endAutoPush(sessionID, callbackRef, confirmed, current) })
	}, nil
}

func (s *pushCredentialState) endAutoPush(sessionID, callbackRef string, confirmed bool, current map[string]string) {
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
		if err := s.persistLocked(current); err != nil {
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
// identity: the snapshot a fence write prunes against. The instances are
// collected under m.mu and read outside it, so this never holds the manager lock
// across an instance lock.
func (m *Manager) callbackRefSnapshot() map[string]string {
	m.mu.Lock()
	insts := make([]*session.Instance, 0, len(m.instances))
	for _, inst := range m.instances {
		if inst != nil {
			insts = append(insts, inst)
		}
	}
	m.mu.Unlock()
	out := make(map[string]string, len(insts))
	for _, inst := range insts {
		if p, ok := instancePushPlacement(inst); ok && p.CallbackRef != "" {
			out[inst.ID] = p.CallbackRef
		}
	}
	return out
}
