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
	"time"

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
// snapshot's channel would re-walk the process table on every poll for a
// sibling that is never adopted.
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

// codexAttributionRejudgeInterval bounds how long a "the runtime does not
// hold this rollout" verdict may stand before the candidate is judged again.
// Held is a stable positive — Codex keeps its rollout descriptor for the
// process's life — so a positive verdict is final. A negative verdict is
// only "not held as of that walk": a rollout's header and the runtime's hold
// on it do not become visible in lockstep (#6938), and a first-sight answer
// that is never re-asked pins a not-yet verdict as forever-foreign for the
// life of the generation's cache. Re-judging on a cadence far slower than
// the 50ms poll keeps a genuinely-foreign sibling from re-walking the
// process table on every tick while letting a late-visible hold still
// attribute inside the capture's settle window. Var, not const, so tests
// can shorten the re-judgement cadence.
var codexAttributionRejudgeInterval = 250 * time.Millisecond

// codexRolloutVerdict records one judged answer for a candidate path. ours
// is the verdict; judgedAt is when the descriptor set it came from was read.
// A positive verdict never expires — a rollout's owner is fixed at mint.
type codexRolloutVerdict struct {
	ours     bool
	judgedAt time.Time
}

// judgeCodexRollouts partitions candidates into rollouts the held set
// positively contains and those it provably does not, memoizing each path's
// verdict so an already-judged sibling is never re-walked — except a stale
// negative, which is only "not held as of the last walk" and is re-asked
// once it ages past codexAttributionRejudgeInterval (#6938). heldFor — the
// expensive descriptor enumeration — runs only while unjudged or
// re-judgeable candidates exist; its error makes the unjudged remainder
// inconclusive ("could not look" must never arrive as "nobody holds it").
func judgeCodexRollouts(heldFor func() (map[string]struct{}, error), verdicts map[string]codexRolloutVerdict, mu *sync.Mutex, candidates []string) (attributed []string, inconclusive bool) {
	now := time.Now()
	mu.Lock()
	var pending []string
	for _, path := range candidates {
		v, ok := verdicts[path]
		switch {
		case !ok:
			pending = append(pending, path)
		case v.ours:
			attributed = append(attributed, path)
		case now.Sub(v.judgedAt) >= codexAttributionRejudgeInterval:
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
		verdicts[path] = codexRolloutVerdict{ours: ours, judgedAt: now}
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
	// The verdict map must be captured under the SAME lock that selects its
	// generation: a deferred loop displaced mid-attempt is allowed to overlap
	// its replacement, and reading the field unlocked could hand this attempt
	// the replacement's map — a data race, and old-generation verdicts would
	// land in the new generation's cache. The captured map keeps working even
	// if the field is swapped mid-walk; the stability check then discards
	// this attempt's result and the stale map is garbage-collected.
	verdicts := i.verdictsForGen(gen)
	attributed, inconclusive = judgeCodexRollouts(heldFor, verdicts, &i.codexAttrMu, candidates)
	i.codexAttrMu.Lock()
	stable := i.codexAttrGen == gen
	i.codexAttrMu.Unlock()
	if !stable {
		return nil, true
	}
	return attributed, inconclusive
}

// verdictsForGen returns the verdict cache for gen under codexAttrMu,
// resetting it when the live generation changed — a respawned pane's
// rollouts must be judged against the new process set, never inherit its
// predecessor's verdicts.
func (i *Instance) verdictsForGen(gen string) map[string]codexRolloutVerdict {
	i.codexAttrMu.Lock()
	defer i.codexAttrMu.Unlock()
	if i.codexAttrGen != gen {
		i.codexAttrGen = gen
		i.codexAttrVerdicts = make(map[string]codexRolloutVerdict)
	}
	return i.codexAttrVerdicts
}

// codexRuntimeJudgement resolves this judgement's generation key and the
// source of its held set. The test override supplies a stable generation and
// its held answer outright — a fixture with a name-only pane has no process
// generation to resolve — while production walks the pane's resolved roots.
func (i *Instance) codexRuntimeJudgement() (gen string, heldFor func() (map[string]struct{}, error), err error) {
	i.mu.RLock()
	override := i.codexRuntimeHeldOverride
	i.mu.RUnlock()
	roots, rerr := i.codexRuntimeRoots()
	if override == nil {
		if rerr != nil {
			return "", nil, rerr
		}
		return codexGenerationKey(roots), func() (map[string]struct{}, error) {
			return i.heldPathsForRoots(roots)
		}, nil
	}
	// The override supplies the held answer; the generation still comes from
	// the live roots when they resolve, so an override fixture's respawned
	// runtime gets a fresh verdict cache exactly like production. Pane-less
	// fixtures fall back to one synthetic generation.
	gen = "test-override"
	if rerr == nil {
		gen = codexGenerationKey(roots)
	}
	return gen, func() (map[string]struct{}, error) {
		return override()
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
