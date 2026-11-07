package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
// own session — is provably foreign. Verdicts are memoized on the INSTANCE,
// keyed to the resolved generation, because the deferred capture loop re-binds
// a fresh attribution object on every attempt: state carried only on the
// snapshot's channel would re-walk the process table forever for a sibling
// that is never adopted.
//
// inconclusive is reserved for "evidence exists but could not be read" — a
// process table or descriptor set that failed mid-walk — which is the same
// not-an-answer an unwritten session_meta is, and gets the same bounded
// settle rather than a verdict. An UNBOUND snapshot (no runtime channel at
// all, the pre-binding shape every older call site produced) can attribute
// nothing: every candidate stays unproven and the capture simply never adopts
// — unknown is not foreign, but it is not attribution either.
type codexRuntimeAttribution struct {
	attribute func(candidates []string) (attributed []string, inconclusive bool)
}

// attributeCodexRollouts asks the bound runtime to judge candidates. An
// unbound snapshot attributes nothing and is NOT inconclusive: that is the
// quiet never-adopt shape, retried without churn by every caller that
// already retries absence.
func (s *ConversationCaptureSnapshot) attributeCodexRollouts(candidates []string) (attributed []string, inconclusive bool) {
	r := s.runtime
	if r == nil || r.attribute == nil {
		return nil, false
	}
	return r.attribute(candidates)
}

// judgeCodexRollouts partitions candidates into rollouts the held set
// positively contains and those it provably does not, memoizing each path's
// verdict so an already-judged sibling is never re-walked. heldFor — the
// expensive descriptor enumeration — runs only while unjudged candidates
// exist; its error makes the unjudged remainder inconclusive ("could not
// look" must never arrive as "nobody holds it").
func judgeCodexRollouts(heldFor func() (map[string]struct{}, error), verdicts map[string]bool, mu *sync.Mutex, candidates []string) (attributed []string, inconclusive bool) {
	mu.Lock()
	var pending []string
	for _, path := range candidates {
		if v, ok := verdicts[path]; ok {
			if v {
				attributed = append(attributed, path)
			}
		} else {
			pending = append(pending, path)
		}
	}
	mu.Unlock()
	if len(pending) == 0 {
		return attributed, false
	}
	held, err := heldFor()
	if err != nil {
		return attributed, true
	}
	set := newCodexHeldSet(held)
	mu.Lock()
	defer mu.Unlock()
	for _, path := range pending {
		ours := set.contains(path)
		verdicts[path] = ours
		if ours {
			attributed = append(attributed, path)
		}
	}
	return attributed, false
}

// fileID is a (device, inode) pair: the identity a file has regardless of
// which alias named it.
type fileID struct {
	dev, ino uint64
}

// fileIdentityOf stats path into its kernel identity. Stat follows symlinks,
// so an aliased path and the resolved path yield the same id.
func fileIdentityOf(path string) (fileID, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileID{}, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, false
	}
	return fileID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true
}

// codexHeldSet answers "does the runtime hold this path" under every alias
// shape: a store reached through a symlinked CODEX_HOME yields candidates
// under the alias while kernel descriptor answers carry the resolved path —
// and on darwin even EvalSymlinks output and vnode paths can differ in form
// (/var vs /private/var). Identity comparison bypasses path spelling
// entirely (#6847 review).
type codexHeldSet struct {
	byPath map[string]struct{}
	byID   map[fileID]struct{}
}

func newCodexHeldSet(raw map[string]struct{}) *codexHeldSet {
	s := &codexHeldSet{
		byPath: make(map[string]struct{}, len(raw)),
		byID:   make(map[fileID]struct{}, len(raw)),
	}
	for path := range raw {
		s.byPath[path] = struct{}{}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			s.byPath[resolved] = struct{}{}
		}
		if id, ok := fileIdentityOf(path); ok {
			s.byID[id] = struct{}{}
		}
	}
	return s
}

func (s *codexHeldSet) contains(path string) bool {
	if _, ok := s.byPath[path]; ok {
		return true
	}
	if id, ok := fileIdentityOf(path); ok {
		if _, ok := s.byID[id]; ok {
			return true
		}
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		_, ok := s.byPath[resolved]
		return ok
	}
	return false
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
	snap.runtime = &codexRuntimeAttribution{attribute: i.attributeCodexRolloutPaths}
	return snap
}

// attributeCodexRolloutPaths is the instance-side judgement behind every bound
// snapshot. It resolves the generation first: verdicts cached under a dead
// generation (a respawned pane reuses the session row) never leak into the
// replacement's judgements, and a generation that changes mid-walk discards
// the walk — its verdicts belong to no live process set.
func (i *Instance) attributeCodexRolloutPaths(candidates []string) (attributed []string, inconclusive bool) {
	gen, heldFor, err := i.codexRuntimeJudgement()
	if err != nil {
		return nil, true
	}
	i.codexAttrMu.Lock()
	if i.codexAttrGen != gen {
		i.codexAttrGen = gen
		i.codexAttrVerdicts = make(map[string]bool)
	}
	i.codexAttrMu.Unlock()
	attributed, inconclusive = judgeCodexRollouts(heldFor, i.codexAttrVerdicts, &i.codexAttrMu, candidates)
	i.codexAttrMu.Lock()
	stable := i.codexAttrGen == gen
	i.codexAttrMu.Unlock()
	if !stable {
		return nil, true
	}
	return attributed, inconclusive
}

// codexRuntimeJudgement resolves this judgement's generation key and the
// source of its held set. The test override supplies a stable generation and
// its held answer outright — a fixture with a name-only pane has no process
// generation to resolve — while production walks the pane's resolved roots.
func (i *Instance) codexRuntimeJudgement() (gen string, heldFor func() (map[string]struct{}, error), err error) {
	i.mu.RLock()
	override := i.codexRuntimeHeldOverride
	i.mu.RUnlock()
	if override != nil {
		return "test-override", func() (map[string]struct{}, error) {
			return override()
		}, nil
	}
	roots, err := i.codexRuntimeRoots()
	if err != nil {
		return "", nil, err
	}
	return codexGenerationKey(roots), func() (map[string]struct{}, error) {
		return i.heldPathsForRoots(roots)
	}, nil
}

// codexGenerationKey fingerprints a resolved root set so cached verdicts can
// be pinned to exactly one generation.
func codexGenerationKey(roots []proctree.Process) string {
	parts := make([]string, 0, len(roots))
	for _, r := range roots {
		parts = append(parts, fmt.Sprintf("%d:%d", r.PID, r.StartID))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
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

// heldPathsForRoots returns every filesystem path held open by processes in
// the given roots' runtime — each root's process tree plus the rest of its
// kernel session, which covers children reparented to init. A member that
// vanished between the process snapshot and its descriptor read is skipped:
// gone is an answer, and its descriptors are gone with it. Any other read
// failure fails the whole walk, because an unseeable member could be exactly
// the process holding the rollout being judged — "I could not look" must
// never arrive as "nobody holds it".
func (i *Instance) heldPathsForRoots(roots []proctree.Process) (map[string]struct{}, error) {
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
// rollout attribution with a fixed answer. Daemon tests model a session's
// pane with a name-only TmuxSession — there is no real process generation to
// walk — so adoption tests hand attribution the file set their fixture
// produced, exactly as SetStatusForTest hands the status poll a fixed row.
func (i *Instance) SetCodexRuntimeHeldOverrideForTest(held func() (map[string]struct{}, error)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.codexRuntimeHeldOverride = held
}
