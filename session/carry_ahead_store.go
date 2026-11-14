package session

import (
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The handoff engine asks require_carry's check-ahead answer
// (AccountSwapCarryRefusal) on every 45 s tick, for every triggered session,
// for as long as a wall lasts. On a shared Codex home of tens of thousands of
// rollouts one answer was a full walk of the store plus an lstat of every
// rollout — about half a second, per session, per tick, under the handoff
// mover's lock (#7156 review P2-1).
//
// So the answer reads a snapshot of the source store instead. One walk records
// everything the source-side checks read: each rollout's path and mtime, and
// each thread's compressed and thread/revert forms. Every session on that home
// then shares the snapshot for carryAheadStoreTTL, so the walks are one per
// home per TTL, however many sessions ask. The live swap never reads a
// snapshot: its plan and its copy-time re-check walk the store as before.
//
// The price is a staleness bounded by the TTL. A newer thread, a backtrack or
// a compression made since the snapshot is seen up to carryAheadStoreTTL late,
// as a carry the answer still confirms; admission plans the move against the
// live store and falls back to fresh, as it does today for anything that
// changes after the check. The other direction is closed: a snapshot taken
// before the session's conversation was recorded is never used for it, so a
// rollout recorded since does not read as missing.

// carryAheadStoreTTL is how long a source store snapshot answers
// require_carry's check-ahead: several engine ticks, so a standing wall costs
// one walk per home every few minutes rather than one per session every tick.
const carryAheadStoreTTL = 5 * time.Minute

// codexStoreSnapshot is one walk of a Codex home's sessions store.
type codexStoreSnapshot struct {
	takenAt time.Time
	// err is the walk's failure: like the live walk, a store that cannot be
	// traversed is never an empty-looking one (#5471).
	err error
	// plain maps a thread id (lower case) to its plain rollouts: the files
	// codexRolloutFiles would list for it.
	plain map[string][]string
	// modTimes maps every rollout codexRolloutFiles would list to its mtime at
	// the walk; a file that vanished mid-walk is absent, as an lstat of it
	// would fail.
	modTimes map[string]time.Time
	// other maps a thread id to its compressed and thread/revert forms: the
	// files codexRolloutForms reports as other.
	other map[string][]string
}

// takeCodexStoreSnapshot walks codexHome's sessions store once.
func takeCodexStoreSnapshot(codexHome string, now time.Time) *codexStoreSnapshot {
	snap := &codexStoreSnapshot{
		takenAt:  now,
		plain:    map[string][]string{},
		modTimes: map[string]time.Time{},
		other:    map[string][]string{},
	}
	if codexHome == "" {
		return snap
	}
	snap.err = walkConversationStore(filepath.Join(codexHome, "sessions"), true, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d == nil || d.IsDir() {
			return nil
		}
		if id := codexConversationIDFromPath(path); id != "" {
			snap.plain[id] = append(snap.plain[id], path)
			if info, err := d.Info(); err == nil {
				snap.modTimes[path] = info.ModTime()
			}
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if m := codexRolloutFormRE.FindStringSubmatch(d.Name()); len(m) == 4 && (m[2] != "" || m[3] != "") {
			id := strings.ToLower(m[1])
			snap.other[id] = append(snap.other[id], path)
		}
		return nil
	})
	return snap
}

// plainOf is the thread's plain rollouts.
func (s *codexStoreSnapshot) plainOf(id string) []string {
	return s.plain[strings.ToLower(id)]
}

// otherOf is the thread's compressed and thread/revert forms.
func (s *codexStoreSnapshot) otherOf(id string) []string {
	return s.other[strings.ToLower(id)]
}

// rollouts yields every listed rollout with its mtime at the walk.
func (s *codexStoreSnapshot) rollouts() iter.Seq2[string, time.Time] {
	return func(yield func(string, time.Time) bool) {
		for path, modTime := range s.modTimes {
			if !yield(path, modTime) {
				return
			}
		}
	}
}

// holdsRevertSince reports whether the snapshot files thread id under a
// thread/revert rollout in a day directory a walk from floor would open
// (codexRolloutFormsSince): the same answer codexHoldsRevertRollout gives on
// the live store.
func (s *codexStoreSnapshot) holdsRevertSince(codexHome, id string, floor time.Time) bool {
	root := filepath.Join(codexHome, "sessions")
	for _, path := range s.otherOf(id) {
		if !codexRolloutIsRevert(path) {
			continue
		}
		if rel, err := filepath.Rel(root, filepath.Dir(path)); err == nil {
			if parts := strings.Split(filepath.ToSlash(rel), "/"); len(parts) >= 3 &&
				codexRolloutDirBeforeFloor(filepath.Join(parts[:3]...), floor) {
				continue
			}
		}
		return true
	}
	return false
}

// codexStoreCache shares store snapshots between require_carry's answers: the
// source home's, and the target's when an answer names one. It holds its lock across a walk: the engine asks one
// session at a time, and a second asker for the same home must wait for that
// walk rather than start its own.
type codexStoreCache struct {
	mu     sync.Mutex
	ttl    time.Duration
	now    func() time.Time
	byHome map[string]*codexStoreSnapshot
	// walks counts the snapshots taken: the store walks the answers made.
	walks int
}

func newCodexStoreCache(ttl time.Duration) *codexStoreCache {
	return &codexStoreCache{ttl: ttl, now: time.Now, byHome: map[string]*codexStoreSnapshot{}}
}

// carryAheadStores is the snapshot cache AccountSwapCarryRefusal reads.
var carryAheadStores = newCodexStoreCache(carryAheadStoreTTL)

// snapshot returns codexHome's snapshot, walking the store only when none
// taken in the last TTL, and no earlier than notBefore, is held. Snapshots past
// their TTL are dropped on every call, and one nobody asks for again is
// dropped by a timer, so a wall that ends does not leave its store resident.
func (c *codexStoreCache) snapshot(codexHome string, notBefore time.Time) *codexStoreSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for home, snap := range c.byHome {
		if now.Sub(snap.takenAt) >= c.ttl {
			delete(c.byHome, home)
		}
	}
	if snap := c.byHome[codexHome]; snap != nil && !snap.takenAt.Before(notBefore) {
		return snap
	}
	snap := takeCodexStoreSnapshot(codexHome, now)
	c.walks++
	c.byHome[codexHome] = snap
	time.AfterFunc(c.ttl, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.byHome[codexHome] == snap {
			delete(c.byHome, codexHome)
		}
	})
	return snap
}

// codexSourceRelPath is the thread's one plain rollout in the source store,
// relative to the source home, or why af cannot name it. It reads the plan's
// snapshot when it has one; otherwise it walks the store and keeps the listing
// for the plan's staleness check.
func (c *conversationCarry) codexSourceRelPath() (string, string) {
	if c.srcStore != nil {
		if c.srcStore.err != nil {
			return "", carryStoreUnlistedReason
		}
		return codexRolloutRelPathOf(c.srcHome, c.srcStore.plainOf(c.id))
	}
	files, err := codexRolloutFiles(c.srcHome)
	if err != nil {
		return "", carryStoreUnlistedReason
	}
	c.srcFiles = files
	return codexRolloutRelPathIn(c.srcHome, files, c.id)
}

// codexOtherFormsReason is codexCarryOtherFormsReason for the source store,
// read from the plan's snapshot when it has one.
func (c *conversationCarry) codexOtherFormsReason() string {
	switch {
	case c.srcStore == nil:
		return codexCarryOtherFormsReason(c.srcHome, c.id)
	case c.srcStore.err != nil:
		return carryOtherFormsUnlistedReason
	case len(c.srcStore.otherOf(c.id)) > 0:
		return carryOtherFormsReason
	}
	return ""
}

// sourceHoldsRevert is codexHoldsRevertRollout for the source store, read from
// the plan's snapshot when it has one.
func (c *conversationCarry) sourceHoldsRevert(floor time.Time) (bool, error) {
	switch {
	case c.srcStore == nil:
		return codexHoldsRevertRollout(c.srcHome, c.id, floor)
	case c.srcStore.err != nil:
		return false, c.srcStore.err
	}
	return c.srcStore.holdsRevertSince(c.srcHome, c.id, floor), nil
}

// codexTargetRelPath is codexSourceRelPath for the target store: the thread's
// one plain rollout there, "" when it holds none, or why af cannot tell. It
// reads the plan's target snapshot when it has one.
func (c *conversationCarry) codexTargetRelPath() (string, string) {
	if c.dstStore != nil {
		if c.dstStore.err != nil {
			return "", carryStoreUnlistedReason
		}
		return codexRolloutRelPathOf(c.dstHome, c.dstStore.plainOf(c.id))
	}
	return codexRolloutRelPath(c.dstHome, c.id)
}

// targetHoldsRevert is sourceHoldsRevert for the target store.
func (c *conversationCarry) targetHoldsRevert(floor time.Time) (bool, error) {
	switch {
	case c.dstStore == nil:
		return codexHoldsRevertRollout(c.dstHome, c.id, floor)
	case c.dstStore.err != nil:
		return false, c.dstStore.err
	}
	return c.dstStore.holdsRevertSince(c.dstHome, c.id, floor), nil
}
