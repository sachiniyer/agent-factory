package session

import (
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"syscall"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// codexRuntimeAttribution is the capture's positive-attribution channel: the
// process generation this snapshot was armed against, re-resolved through the
// pane on every judgement. A rollout file's mere existence after a baseline —
// even as the lone new file whose session_meta agrees with the launch
// directory — answers only WHICH DIRECTORY it names, never WHICH PROCESS
// wrote it. A sibling Codex launched in the same worktree against the same
// account-scoped store mints exactly that shape of evidence, and adopting it
// records a foreign thread as this session's conversation (#6847, the
// deferred-capture twin of the carried-rollout refusal in #6834).
//
// Held-open is the positive fact appearance cannot be: Codex's recorder
// creates its rollout when the thread starts and keeps the write descriptor
// open for the process's life (codex-rs core/rollout/recorder.rs), so a file
// open in this pane's process set is provably this runtime's thread, and a
// file open nowhere in that set — written by a sibling that holds it in ITS
// own session — is provably foreign. Because a candidate's owner never
// changes after mint, each path's verdict is judged once and memoized: the
// daemon re-asks this attribution on every armed capture attempt, and an
// already-judged sibling must not re-walk the process table forever.
//
// inconclusive is reserved for "evidence exists but could not be read" — a
// process table or descriptor set that failed mid-walk — which is the same
// not-an-answer an unwritten session_meta is, and gets the same bounded
// settle rather than a verdict. An UNBOUND snapshot (no runtime channel at
// all, the pre-binding shape every older call site produced) can attribute
// nothing: every candidate stays unproven and the capture simply never adopts
// — unknown is not foreign, but it is not attribution either.
type codexRuntimeAttribution struct {
	held func() (map[string]struct{}, error)

	mu       sync.Mutex
	verdicts map[string]bool
}

// attributeCodexRollouts partitions candidates into the rollouts the bound
// runtime positively holds open and those it provably does not, reporting
// inconclusive when the walk could not finish — a refused or vanished
// process set can hide a holder, so its unjudged candidates are undecided,
// not foreign. A snapshot with no runtime channel attributes nothing and is
// NOT inconclusive: that is the quiet never-adopt shape, retried without
// churn by every caller that already retries absence.
func (s *ConversationCaptureSnapshot) attributeCodexRollouts(candidates []string) (attributed []string, inconclusive bool) {
	r := s.runtime
	if r == nil || r.held == nil {
		return nil, false
	}
	r.mu.Lock()
	if r.verdicts == nil {
		r.verdicts = make(map[string]bool)
	}
	var pending []string
	for _, path := range candidates {
		if v, ok := r.verdicts[path]; ok {
			if v {
				attributed = append(attributed, path)
			}
		} else {
			pending = append(pending, path)
		}
	}
	r.mu.Unlock()
	if len(pending) == 0 {
		return attributed, false
	}
	held, err := r.held()
	if err != nil {
		return attributed, true
	}
	r.mu.Lock()
	for _, path := range pending {
		_, ours := held[path]
		r.verdicts[path] = ours
		if ours {
			attributed = append(attributed, path)
		}
	}
	r.mu.Unlock()
	return attributed, false
}

// BindConversationCaptureRuntime attaches this instance's live runtime — the
// pane-root process generation it launched — to snap's rollout discovery, so
// capture adopts only a rollout that runtime itself holds open. The binding
// is a resolver, not a frozen pid: it re-asks the pane on every judgement, so
// a snapshot armed before the pane exists still attributes correctly once it
// does, and a runtime replaced under the same instance attributes to the
// replacement. Snapshots for providers with no local rollout store pass
// through untouched; so does one whose launch never bound a Codex store.
func (i *Instance) BindConversationCaptureRuntime(snap ConversationCaptureSnapshot) ConversationCaptureSnapshot {
	if !snap.CapturesCodex() {
		return snap
	}
	snap.runtime = &codexRuntimeAttribution{held: i.codexRuntimeHeldPaths}
	return snap
}

// codexRuntimeRoots resolves the pane-root process this instance's agent
// runtime launched as. The recorded launch evidence — the (pid, kernel
// start-stamp) pair setRuntimeLaunch pinned to the runtime claim — is the
// authoritative answer when it still verifies, because it names the pane af
// actually launched rather than whatever pane happens to be active now. When
// it is absent or stale the live pane root is the fallback: it is the best
// remaining evidence of which process generation holds the pane.
func (i *Instance) codexRuntimeRoots() ([]proctree.Process, error) {
	i.mu.RLock()
	pid, startID := i.runtimePID, i.runtimeStartID
	ts := i.tmuxLocked()
	i.mu.RUnlock()

	if pid > 0 {
		if root, err := proctree.Lookup(pid); err == nil && root.StartID == startID {
			return []proctree.Process{root}, nil
		}
	}
	if ts == nil {
		return nil, errors.New("session has no local pane to attribute its runtime")
	}
	root, err := ts.PaneRootProcess()
	if err != nil {
		return nil, fmt.Errorf("resolve the pane's root process: %w", err)
	}
	return []proctree.Process{root}, nil
}

// codexRuntimeHeldPaths returns every filesystem path held open by processes
// in this instance's runtime — the pane root's process tree plus the rest of
// its kernel session, which covers children reparented to init — keyed for
// membership tests. A member that vanished between the process snapshot and
// its descriptor read is skipped: gone is an answer, and its descriptors are
// gone with it. Any other read failure fails the whole walk, because an
// unseeable member could be exactly the process holding the rollout being
// judged — "I could not look" must never arrive as "nobody holds it".
func (i *Instance) codexRuntimeHeldPaths() (map[string]struct{}, error) {
	i.mu.RLock()
	override := i.codexRuntimeHeldOverride
	i.mu.RUnlock()
	if override != nil {
		return override()
	}
	roots, err := i.codexRuntimeRoots()
	if err != nil {
		return nil, err
	}
	snapshot, err := proctree.Snapshot()
	if err != nil {
		return nil, err
	}
	members := make(map[int]struct{})
	for _, root := range roots {
		current, ok := snapshot[root.PID]
		if !ok || current.StartID != root.StartID {
			continue
		}
		for _, proc := range proctree.TreeOf(snapshot, root.PID) {
			members[proc.PID] = struct{}{}
		}
		for _, proc := range proctree.SessionMembers(snapshot, current.SID) {
			members[proc.PID] = struct{}{}
		}
	}
	if len(members) == 0 {
		return nil, errors.New("the pane's root process is no longer in the process table")
	}
	held := make(map[string]struct{})
	for pid := range members {
		paths, err := proctree.OpenFilePaths(pid)
		switch {
		case err == nil:
			for path := range paths {
				held[path] = struct{}{}
			}
		case errors.Is(err, proctree.ErrProcessExited),
			errors.Is(err, fs.ErrNotExist),
			errors.Is(err, syscall.ESRCH):
			continue
		default:
			return nil, err
		}
	}
	return held, nil
}

// SetCodexRuntimeHeldOverrideForTest replaces the live process walk behind
// codexRuntimeHeldPaths with a fixed answer. Daemon tests model a session's
// pane with a name-only TmuxSession — there is no real process generation to
// walk — so adoption tests hand attribution the file set their fixture
// produced, exactly as SetStatusForTest hands the status poll a fixed row.
func (i *Instance) SetCodexRuntimeHeldOverrideForTest(held func() (map[string]struct{}, error)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codexRuntimeHeldOverride = held
}
