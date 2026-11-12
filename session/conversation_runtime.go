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
// process generation this snapshot was armed against, re-resolved from the
// launch evidence on every judgement. A rollout file's mere existence after
// a baseline — even as the lone new file whose session_meta agrees with the
// launch directory — answers only WHICH DIRECTORY it names, never WHICH
// PROCESS wrote it. A sibling Codex launched in the same worktree against the same
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
	// answerHeld is the baseline-free half of the channel (#7101): it hands
	// judge what the bound runtime holds open right now, and a rollout that
	// set contains is the runtime's own thread however long ago it was
	// minted. nil on channels that can only judge candidates (test fixtures,
	// older shapes).
	answerHeld func(judge heldRolloutJudge) (AgentConversationData, error)
	// evidence reports, without asking tmux, whether the bound runtime has a
	// verified agent process to judge from right now. A diff that cannot have
	// one ends its attempt at once instead of polling the store for an answer
	// that cannot come (#7124 review F2). nil on channels that always judge.
	evidence func() error
}

// runtimeEvidence is the bound channel's evidence answer; an unbound channel,
// or one with no evidence hook, reports nothing missing.
func (s *ConversationCaptureSnapshot) runtimeEvidence() error {
	if s.runtime == nil || s.runtime.evidence == nil {
		return nil
	}
	return s.runtime.evidence()
}

// heldRolloutJudge decides a conversation from a held path set and names the
// path its answer came from.
type heldRolloutJudge func(held map[string]struct{}) (conv AgentConversationData, path string, err error)

// codexConversationHolder remembers which pane member's descriptors named the
// conversation last time, under which runtime generation, and what they named.
type codexConversationHolder struct {
	gen     string
	pid     int
	startID uint64
	id      string
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

// codexAttributionRejudgeInterval bounds how long any cached verdict may stand
// before the candidate is judged again. A negative verdict is only "not held as
// of the last walk": a rollout's header and the runtime's hold on it do not
// become visible in lockstep (#6938), and a first-sight answer that is never
// re-asked pins a not-yet verdict as forever-foreign for the life of the
// generation's cache. A positive verdict is symmetrically only "held as of the
// last walk": a runtime can RELEASE a rollout mid-life — Codex's /new retires
// the old thread's write descriptor during a ~60s unload delay while the new
// thread's recorder is already open — so a positive that is never re-asked pins
// a stale "held" answer for the life of the generation. Re-judging on a cadence
// far slower than the 50ms poll keeps a genuinely-foreign sibling from
// re-walking the process table on every tick while letting a late-visible hold
// still attribute, and a released positive stop attributing, inside the
// capture's settle window. Var, not const, so tests can shorten the cadence.
var codexAttributionRejudgeInterval = 250 * time.Millisecond

// codexRolloutVerdict records one judged answer for a candidate path. ours is
// the verdict; judgedAt is when the descriptor set it came from was read. A
// verdict of either sign expires after codexAttributionRejudgeInterval: held
// evidence is re-asked, never pinned, exactly as the follow check re-asks it
// for recorded rows.
type codexRolloutVerdict struct {
	ours     bool
	judgedAt time.Time
}

// judgeCodexRollouts partitions candidates into rollouts the held set
// positively contains and those it provably does not, memoizing each path's
// verdict so an already-judged sibling is never re-walked within one settle
// window. Both signs of verdict expire after codexAttributionRejudgeInterval:
// a negative is only "not held as of the last walk" (#6938), and a positive is
// only "held as of the last walk" — a runtime can release a rollout mid-life
// (Codex /new retires the old thread's descriptor before the process exits),
// so a cached positive must be re-asked or a single descriptor walk that
// catches two rollouts held together mints two permanent positives and wedges
// the capture's ambiguity for the life of the generation. heldFor — the
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
		case now.Sub(v.judgedAt) >= codexAttributionRejudgeInterval:
			// A cached verdict of either sign ages out and is re-walked.
			pending = append(pending, path)
		case v.ours:
			attributed = append(attributed, path)
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
// is a resolver, not a frozen pid: it re-reads the launch evidence on every
// judgement, so a snapshot armed before the launch recorded it still
// attributes correctly once it has, and a runtime replaced under the same
// instance attributes to the replacement. Without verified evidence the
// channel attributes nothing, the diff ends its attempt at once, and the held
// query reports why (codexVerifiedRuntimeRoot). Binding asks tmux nothing: an
// identity kept for capture is re-verified by CheckConversationCaptureEvidence,
// which every capture entry point calls first (#7123). Snapshots for providers
// with no local rollout store pass through untouched; so does one whose launch
// never bound a Codex store.
func (i *Instance) BindConversationCaptureRuntime(snap ConversationCaptureSnapshot) ConversationCaptureSnapshot {
	if !snap.CapturesCodex() {
		return snap
	}
	snap.runtime = &codexRuntimeAttribution{
		attribute:  i.attributeCodexRolloutPaths,
		answerHeld: i.answerHeldCodexConversation,
		evidence:   i.captureRuntimeEvidence,
	}
	return snap
}

// answerHeldCodexConversation is the instance-side answer behind a bound
// channel's held query. The follow check asks it on every rearm for every
// recorded Codex session, and the full walk reads the whole process table —
// one stat per process on the box — to find the pane's members. So when the
// member that named the conversation last time is still the same process in
// the same runtime generation, and the record is still that answer, its own
// descriptors are read first: if judge gives the same answer from them,
// nothing else could change the verdict (a second top-level rollout anywhere
// else would only make the full walk ambiguous, which keeps the record
// exactly as this answer does). Any other outcome falls through to the full
// walk, which also re-learns the holder.
//
// A generation that changes mid-walk discards the walk — its files belong to
// no live process set — exactly as attributeCodexRolloutPaths does.
func (i *Instance) answerHeldCodexConversation(judge heldRolloutJudge) (AgentConversationData, error) {
	i.mu.RLock()
	override := i.codexRuntimeHeldOverride
	i.mu.RUnlock()
	// The roots and generation this answer judges from: only the verified
	// launch evidence, never the session's active pane (#7120 review); the
	// test override keeps its own resolution.
	resolve := func() ([]proctree.Process, string, error) {
		if override != nil {
			gen, _, err := i.codexRuntimeJudgement()
			return nil, gen, err
		}
		root, err := i.codexVerifiedRuntimeRoot()
		if err != nil {
			return nil, "", err
		}
		roots := []proctree.Process{root}
		return roots, codexGenerationKey(roots), nil
	}
	roots, gen, err := resolve()
	if err != nil {
		return AgentConversationData{}, err
	}
	i.codexAttrMu.Lock()
	holder := i.codexHolder
	i.codexAttrMu.Unlock()
	// The fast path may only CONFIRM the record, never change it: its
	// equivalence with the full walk ("anything else could only make the walk
	// ambiguous, which keeps the record too") holds only while the record is
	// the holder's own answer. A record that moved since — an operator's
	// set-conversation to a thread another member holds — needs the full walk,
	// which reports that ambiguity instead of re-pointing the repair (#7120
	// review).
	if override == nil && holder.pid > 0 && holder.gen == gen && holder.id == i.AgentConversation().ID {
		if p, err := proctree.Lookup(holder.pid); err == nil && p.StartID == holder.startID {
			if paths, err := proctree.OpenFilePaths(holder.pid); err == nil {
				if conv, _, err := judge(paths); err == nil && conv.ID == holder.id {
					return conv, nil
				}
			}
		}
	}

	var byMember map[int]heldMemberFiles
	if override != nil {
		paths, err := override()
		if err != nil {
			return AgentConversationData{}, err
		}
		// A fixture has no per-process answer, so it never learns a holder.
		byMember = map[int]heldMemberFiles{0: {paths: paths}}
	} else if byMember, err = heldPathsByMemberForRoots(roots); err != nil {
		return AgentConversationData{}, err
	}
	held := make(map[string]struct{})
	for _, member := range byMember {
		for path := range member.paths {
			held[path] = struct{}{}
		}
	}
	conv, path, judgeErr := judge(held)
	if _, after, err := resolve(); err != nil || after != gen {
		return AgentConversationData{}, errors.New("the pane's runtime was replaced while its open files were read")
	}
	var learned codexConversationHolder
	if judgeErr == nil && conv.HasID() {
		for pid, member := range byMember {
			if _, ok := member.paths[path]; ok && pid > 0 {
				learned = codexConversationHolder{gen: gen, pid: pid, startID: member.startID, id: conv.ID}
				break
			}
		}
	}
	i.codexAttrMu.Lock()
	i.codexHolder = learned
	i.codexAttrMu.Unlock()
	return conv, judgeErr
}

// attributeCodexRolloutPaths is the instance-side judgement behind every bound
// snapshot. It resolves the generation first: verdicts cached under a dead
// generation (a respawned pane reuses the session row) never leak into the
// replacement's judgements, and a generation that changes mid-walk discards
// the walk — its verdicts belong to no live process set.
//
// A runtime with no verified agent process — no launch record, or one naming
// a process that is gone or replaced — attributes nothing, and it is not
// inconclusive either. The session's active pane used to stand in, and it is
// not evidence of which process is the agent: a user's split or a new window
// running its own codex in the same worktree and store minted exactly the
// rollout the diff then adopted as this session's (#7123). So the diff fails
// closed the way an unbound snapshot does, without spending the settle window
// on an answer that cannot come, and the held query — which the daemon loop
// asks next — reports why.
func (i *Instance) attributeCodexRolloutPaths(candidates []string) (attributed []string, inconclusive bool) {
	gen, heldFor, err := i.codexRuntimeJudgement()
	if err != nil {
		return nil, false
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
// generation to resolve — while production walks the verified agent
// process's runtime (codexVerifiedRuntimeRoot), and nothing else.
func (i *Instance) codexRuntimeJudgement() (gen string, heldFor func() (map[string]struct{}, error), err error) {
	i.mu.RLock()
	override := i.codexRuntimeHeldOverride
	i.mu.RUnlock()
	root, rerr := i.codexVerifiedRuntimeRoot()
	roots := []proctree.Process{root}
	if override == nil {
		if rerr != nil {
			return "", nil, rerr
		}
		return codexGenerationKey(roots), func() (map[string]struct{}, error) {
			return i.heldPathsForRoots(roots)
		}, nil
	}
	// The override supplies the held answer; the generation still comes from
	// the launch evidence when it verifies, so an override fixture's respawned
	// runtime gets a fresh verdict cache exactly like production. Fixtures
	// with no evidence fall back to one synthetic generation.
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

// codexVerifiedRuntimeRoot is the only root any attribution may judge from —
// the held query, the follow, and the new-since-baseline diff alike: the
// (pid, start-stamp) launch evidence setRuntimeLaunch pinned to this runtime,
// or the identity an unanswerable reattach probe kept for capture once the
// session-wide pane scan has re-verified it (#7123) — still naming the same
// process. The session's ACTIVE pane is not evidence of which process is the
// agent: it can be a user's split or a new window running its own codex in the
// same worktree and store, whose thread the held query would record and
// re-record every interval (#7120 review) and the diff would adopt (#7123).
// Without verified evidence the answer is ErrNoConversationCaptureEvidence,
// which the capture loop logs, never a guess. It asks tmux nothing.
func (i *Instance) codexVerifiedRuntimeRoot() (proctree.Process, error) {
	i.mu.RLock()
	pid, startID := i.runtimePID, i.runtimeStartID
	kept := i.captureEvidence
	i.mu.RUnlock()
	if pid <= 0 && kept.pid > 0 {
		if !kept.verified {
			return proctree.Process{}, fmt.Errorf("af has not yet re-verified that the agent process it launched for this session (pid %d) still roots one of its panes, so %w",
				kept.pid, ErrNoConversationCaptureEvidence)
		}
		pid, startID = kept.pid, kept.startID
	}
	if pid <= 0 {
		return proctree.Process{}, fmt.Errorf("af has no launch record of this session's agent process, so %w", ErrNoConversationCaptureEvidence)
	}
	root, err := proctree.Lookup(pid)
	if err != nil || root.StartID != startID {
		return proctree.Process{}, fmt.Errorf("the agent process af launched for this session (pid %d) is gone or replaced, so %w",
			pid, ErrNoConversationCaptureEvidence)
	}
	return root, nil
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
	byMember, err := heldPathsByMemberForRoots(roots)
	if err != nil {
		return nil, err
	}
	held := make(map[string]struct{})
	for _, member := range byMember {
		for path := range member.paths {
			held[path] = struct{}{}
		}
	}
	return held, nil
}

// heldMemberFiles is one runtime member's open files, with the start stamp
// that proves a later lookup of the same pid is the same process.
type heldMemberFiles struct {
	startID uint64
	paths   map[string]struct{}
}

// heldPathsByMemberForRoots is heldPathsForRoots keyed by member pid, for the
// follow check's holder fast path (#7101).
func heldPathsByMemberForRoots(roots []proctree.Process) (map[int]heldMemberFiles, error) {
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
	held := make(map[int]heldMemberFiles, len(members))
	for pid := range members {
		paths, err := proctree.OpenFilePaths(pid)
		switch {
		case err == nil:
			held[pid] = heldMemberFiles{startID: snapshot[pid].StartID, paths: paths}
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
