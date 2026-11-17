package session

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

var (
	// ErrPromptReceiptUnavailable means the provider exposes no receiver-side
	// acknowledgement af knows how to read. It is distinct from a missing
	// receipt: unsupported is a capability question; not-observed means a
	// provider that does expose receipts never recorded this prompt.
	ErrPromptReceiptUnavailable = errors.New("prompt receipt unavailable")
	// ErrPromptReceiptNotObserved means the receiver never recorded the prompt
	// before the acknowledgement deadline. A tmux send returning nil does not
	// override this observation (#2220).
	ErrPromptReceiptNotObserved = errors.New("prompt receipt not observed")
	// ErrPromptReceiptAmbiguous means more than one new receiver conversation
	// appeared after the snapshot. Prompt text cannot identify which process owns
	// which rollout, even when only one has recorded it so far (#2228 review).
	ErrPromptReceiptAmbiguous = errors.New("prompt receipt ambiguous")
	// ErrPromptReceiptUndecidable means the wait expired while a NEW rollout
	// existed whose session_meta never became classifiable, or the prompt
	// itself cannot produce a receipt verdict. In the first case the file
	// could still be the launched process's own rollout mid-write, so a
	// never-arrived verdict must not claim it (#5471 review). In the second,
	// a prompt empty after trimming the whitespace Codex strips from a
	// recorded turn would degrade the substring matcher to an empty needle —
	// which matches every rollout — so it is refused up front (#5488). It is
	// a recognized unconfirmed outcome — callers that keep observing on
	// ErrPromptReceiptNotObserved keep observing on this too.
	ErrPromptReceiptUndecidable = errors.New("prompt receipt undecidable")
)

// errCodexCaptureUndecided marks a retryable observation in which at least one
// new rollout file exists but its session_meta cannot be classified yet. It is
// what separates "no evidence appeared" (absence, bounded by the caller's
// timeout) from "evidence exists and is still being written" (bounded by
// codexSessionMetaSettleTimeout on top). Both refuse if their bound expires.
var errCodexCaptureUndecided = errors.New("codex conversation capture undecided")

// codexSessionMetaSettleTimeout bounds how much longer capture keeps polling
// past its base timeout when a new rollout file exists but its session_meta
// header is not yet classifiable. The base timeout answers "did a rollout
// appear at all"; once one has, the only open question is evidence that Codex
// has not finished writing. Codex 0.147.0 was measured writing the header
// 2.6–2.8s after process start (#3266), so a base window sized for file
// creation expired moments before the header became readable and a successful
// handoff permanently lost its conversation id. 15s is ~5x that observed
// latency; a header still unclassifiable at this bound gets the same undecided
// refusal as before. Var, not const, so tests can shorten the bound.
var codexSessionMetaSettleTimeout = 15 * time.Second

// ConversationCaptureSnapshot records provider-local state before a pane is
// spawned. It lets post-spawn capture identify a newly-created conversation
// without reading transcript contents.
type ConversationCaptureSnapshot struct {
	startedAt   time.Time
	codexHome   string
	workingDir  string
	codexBefore map[string]struct{}
	// codexBeforeErr is set when the before-image scan itself failed: the map
	// can no longer distinguish "new" from "pre-existing" inside the capture
	// window, and every check against this snapshot must surface that failure
	// instead of guessing.
	codexBeforeErr error
	// scopeErr is set when the working directory the snapshot binds discovery
	// to did not resolve: the lookup did not answer within its bound, or
	// failed (#7292). Correlating against it would be a guess, so while it
	// stands the snapshot decides nothing: the diff scan and the held query
	// report the channel unavailable, and a receipt pool is unscoped. Each
	// decision retries the resolution of scopeDir first (rescoped), so a
	// transient non-answer does not outlive the mount's recovery (#7298
	// review).
	scopeErr error
	// scopeDir is the launch dir as given, kept while scopeErr stands.
	scopeDir string
	// poolScopePending marks a receipt pool tracked while scopeErr stood: it
	// could scope nothing, so it holds the whole store. When the scope first
	// resolves, rescoped filters it to the members a scoped pool would hold,
	// keeping their offsets (#7299).
	poolScopePending bool
	// receiptUntracked marks a receipt wait whose tracking could not look at
	// a rollout the launch could append to (#7378): a pool member's or the
	// pinned target's stat did not answer within its bound, or the store
	// listing that finds them did not answer or failed. That rollout may hold
	// the receipt too, so no check of this wait may confirm a lone match
	// (#7384 review R2, R5).
	receiptUntracked bool
	// receiptState carries the scan memoization across the separate
	// WaitForPromptReceipt calls that share one snapshot — the config agent's
	// initial, escalation, and five-minute windows are three different calls
	// re-reading the same rollout files every poll.
	receiptState *codexReceiptState
	// receiptPaths are rollout files outside the new-file set that receipt
	// checks must still scan — a `codex resume` target, whose file predates
	// the snapshot and so can never appear among observed new rollouts even
	// though it is exactly where the resumed session records its turns (#5471
	// review).
	receiptPaths []trackedReceiptPath
	// carried is the conversation a same-agent account swap copied into this
	// store and resumes (#4367). Its rollout is recorded in codexBefore even
	// though the copy lands after the snapshot, so capture only ever reports a
	// thread Codex itself minted.
	carried AgentConversationData
	// runtime is the process generation this capture attributes rollouts to.
	// Armed snapshots are bound to their instance's live pane at arm time
	// (BindConversationCaptureRuntime); an unbound snapshot attributes
	// nothing and adopts nothing — a rollout's appearance inside the window
	// is not evidence of which process wrote it (#6847).
	runtime *codexRuntimeAttribution
	// scopeOnly marks a snapshot that binds a store and launch directory but
	// took no before-image (SessionCodexCaptureScope): it serves the held
	// query and the follow check, which need no baseline, and the
	// new-since-baseline diff refuses it — with no before-set, every file in
	// the window would read as new (#7120 review).
	scopeOnly bool
}

// expectCarriedCodexRollout tells capture that path holds conv's rollout,
// placed there by af rather than minted by the replacement.
func (s *ConversationCaptureSnapshot) expectCarriedCodexRollout(conv AgentConversationData, path string) {
	s.carried = conv
	if s.codexBefore == nil {
		s.codexBefore = make(map[string]struct{})
	}
	s.codexBefore[path] = struct{}{}
}

func cloneConversationCaptureSnapshot(snap ConversationCaptureSnapshot) ConversationCaptureSnapshot {
	copy := snap
	if snap.codexBefore != nil {
		copy.codexBefore = make(map[string]struct{}, len(snap.codexBefore))
		for path := range snap.codexBefore {
			copy.codexBefore[path] = struct{}{}
		}
	}
	// A clone is a new observation: it must identify its own rollout rather
	// than inherit the source's scan memoization. Tracked receipt paths are
	// launch knowledge, not observation state, so they carry over.
	copy.receiptState = newCodexReceiptState()
	copy.receiptPaths = append([]trackedReceiptPath(nil), snap.receiptPaths...)
	return copy
}

// BeginConversationCapture snapshots local provider transcript stores before
// spawning a tab. Today only Codex exposes a local file id we can safely observe;
// other providers degrade to their existing latest-session resume path unless a
// deterministic id was injected before spawn (Claude).
func BeginConversationCapture() ConversationCaptureSnapshot {
	return BeginConversationCaptureAtCodexHome(codexHomeDir())
}

// BeginConversationCaptureAtCodexHome snapshots an exact Codex store rather
// than the daemon's inherited CODEX_HOME. Config-agent commands can carry an
// inline environment assignment, so the process-specific path must be resolved
// before launch and supplied here (#2228 review).
func BeginConversationCaptureAtCodexHome(home string) ConversationCaptureSnapshot {
	return beginConversationCaptureAtCodexHomeAndWorkingDir(home, "")
}

// LaunchCaptureWorkingDir is the directory a conversation capture must correlate
// rollouts against: the directory the launch command actually runs the agent in
// as af composed it, which an `env -C DIR codex` command moves away from the
// worktree (#5861). It is the modeled command-environment cwd when that is
// established entirely through the launch directory and modeled env -C options;
// when it is not provable (an arbitrary prefix, `cd … &&`, a dynamic chdir) it
// returns "", which leaves correlation unscoped rather than keying on a
// directory the agent may not be in.
func LaunchCaptureWorkingDir(program, workDir string) string {
	launch, err := tmux.CommandEnvironmentFromCommand(program, workDir)
	if err != nil || !launch.WorkingDirKnown() {
		return ""
	}
	return launch.WorkingDir
}

// BeginConversationCaptureAtCodexHomeAndWorkingDir snapshots an exact Codex
// store and binds later discovery to one launch directory. Account-scoped
// runtimes share a provider store, so the directory correlation keeps another
// session's concurrently created rollout from making this capture ambiguous.
func BeginConversationCaptureAtCodexHomeAndWorkingDir(home, workingDir string) ConversationCaptureSnapshot {
	return beginConversationCaptureAtCodexHomeAndWorkingDir(home, workingDir)
}

func beginConversationCaptureAtCodexHomeAndWorkingDir(home, workingDir string) ConversationCaptureSnapshot {
	workingDir, scopeErr := resolveCaptureWorkingDir(workingDir)
	startedAt := time.Now()
	// The baseline is bounded to the same capture window the diff scan uses:
	// a rollout directory provably older than the snapshot can hold nothing
	// this session minted, so its contents neither help nor are owed the
	// fail-closed guarantee a window inside the scan gets (#6845).
	before, beforeErr := codexRolloutFilesSince(home, codexRolloutScanFloor(startedAt))
	return ConversationCaptureSnapshot{
		startedAt:      startedAt,
		codexHome:      home,
		workingDir:     workingDir,
		codexBefore:    before,
		codexBeforeErr: beforeErr,
		scopeErr:       scopeErr,
		scopeDir:       scopeDirFor(workingDir, scopeErr),
		receiptState:   newCodexReceiptState(),
	}
}

// CapturesCodex reports whether this snapshot binds a Codex store at all —
// false for the empty snapshot a non-Codex or unresolvable launch produces,
// which a deferred arming must not confuse for a real baseline (#6769).
func (s *ConversationCaptureSnapshot) CapturesCodex() bool {
	return s != nil && strings.TrimSpace(s.codexHome) != ""
}

// BaselineFree reports whether the snapshot binds a store without a
// before-image (SessionCodexCaptureScope): only the held query may answer from
// it, never the new-since-baseline diff.
func (s *ConversationCaptureSnapshot) BaselineFree() bool {
	return s != nil && s.scopeOnly
}

// CodexBaselineErr reports why the snapshot's before-image scan failed, or nil
// when it held or the snapshot took none. A failed before-image can no longer
// distinguish "new" from "pre-existing" inside the capture window, so the
// new-since-baseline diff can never answer from it and must not be asked —
// only the held query, which needs no baseline, can (#7123).
func (s *ConversationCaptureSnapshot) CodexBaselineErr() error {
	if s == nil {
		return nil
	}
	return s.codexBeforeErr
}

// CaptureAgentConversation waits until a supported provider exposes the
// conversation id for a just-spawned tab. Unsupported providers return empty
// data and nil error so callers gracefully keep existing --last/latest behavior.
func CaptureAgentConversation(agent string, snap ConversationCaptureSnapshot, timeout time.Duration) (AgentConversationData, error) {
	switch agent {
	case tmux.ProgramCodex:
		return captureCodexConversation(snap, timeout)
	default:
		return AgentConversationData{}, nil
	}
}

func captureCodexConversation(snap ConversationCaptureSnapshot, timeout time.Duration) (AgentConversationData, error) {
	if snap.codexHome == "" {
		return AgentConversationData{}, nil
	}
	if snap.carried.HasID() {
		return captureCarriedCodexConversation(snap)
	}
	if snap.runtimeEvidence() != nil {
		// The bound runtime has no verified agent process, so no candidate can
		// be attributed however long this waits: end the attempt rather than
		// re-walk the store and re-read every new rollout's header for the
		// whole absent window (#7124 review F2). Nothing found is an answer
		// every caller already handles — the loop asks the held query, which
		// says why, and the swap defers to its post-delivery capture. A carried
		// confirmation, above, needs no attribution and is unaffected.
		return AgentConversationData{}, nil
	}
	start := time.Now()
	absentDeadline := start.Add(timeout)
	settleDeadline := start.Add(timeout + codexSessionMetaSettleTimeout)
	// extended latches the settle window once any poll has seen evidence in
	// flight. A later "not this session's" observation — a candidate judged
	// while the runtime's hold on it was not yet visible — must not shrink
	// the bound back to absent: the hold can still appear, and capture owes
	// the file the full settle window the undecided evidence already earned
	// (#6938).
	extended := false
	for {
		conv, retry, err := captureCodexConversationOnce(snap)
		if conv.HasID() || !retry || timeout <= 0 {
			return conv, err
		}
		// Expiry while a concrete candidate is still unclassifiable is not
		// absence (#3266): the file already appeared inside the base window, so
		// the extended bound only waits for Codex to finish writing a header
		// that classification — unchanged and fail-closed — will then rule on.
		if errors.Is(err, errCodexCaptureUndecided) {
			extended = true
		}
		deadline := absentDeadline
		if extended {
			deadline = settleDeadline
		}
		if time.Now().After(deadline) {
			return conv, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// captureCarriedCodexConversation confirms which thread a replacement that was
// told to resume a carried rollout is on. Codex resumes in place — same id,
// same file (measured for #4367) — so no new rollout for this working
// directory means the carried id stands, and it returns immediately rather
// than waiting out the absence timeout.
//
// A new rollout correlated to the working directory is NOT positive evidence
// that the replacement forked: an unrelated sibling Codex sharing the
// account-scoped store can mint the lone same-cwd rollout during the capture
// window, and af cannot tell it from a real fork by appearance alone. The
// carried rollout is in the before-set, so any new file is by definition a
// different thread. Fail closed: keep the carried id unless positive evidence
// (lineage in the rollout header, a recorded briefing match) proves the new
// rollout is the replacement's own — mirroring codexPromptReceiptOnce, which
// refuses to attribute identity by first-seen appearance. A store that cannot
// be enumerated is a real error, not a guess.
//
// The returned record is freshly stamped even when the id is unchanged: the
// caller commits it through SetAgentConversationForRuntime, which reports
// "no change" as a lost runtime.
func captureCarriedCodexConversation(snap ConversationCaptureSnapshot) (AgentConversationData, error) {
	settleDeadline := time.Now().Add(codexSessionMetaSettleTimeout)
	for {
		_, retry, err := captureCodexConversationOnce(snap)
		if errors.Is(err, errCodexCaptureUndecided) && time.Now().Before(settleDeadline) {
			// A new rollout's session_meta is still mid-write: it could be
			// the replacement's own fork, so keep waiting for it to settle
			// rather than decide around a header Codex has not finished
			// writing. Once it settles, the same fail-closed rule applies.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err != nil && retry && !errors.Is(err, errCodexCaptureUndecided) {
			// A store that cannot be enumerated is a real error: the before-set
			// can no longer distinguish "new" from "pre-existing", so
			// returning the carried id would hide a broken store behind a
			// false "conversation confirmed".
			return AgentConversationData{}, err
		}
		// No new rollout, ambiguity among new rollouts, an undecided rollout
		// past the settle window, or a lone same-cwd sibling: all fail closed
		// to the carried id. The carried id is the known-good thread; only
		// positive evidence of a fork would justify following a different one.
		carried := snap.carried
		carried.CapturedAt = time.Now()
		return carried, nil
	}
}

func captureCodexConversationOnce(snap ConversationCaptureSnapshot) (AgentConversationData, bool, error) {
	snap = snap.rescoped()
	observed, err := newCodexRolloutFiles(snap)
	if err != nil {
		// A store that cannot be enumerated is retried inside the caller's
		// bound — the same grace mid-write headers get — then surfaces as the
		// error it is rather than as absence.
		return AgentConversationData{}, true, err
	}
	candidates, uncorrelated, _, unreadable := classifyCodexRolloutFiles(observed, snap.workingDir)
	if uncorrelated > 0 {
		// Codex can create each file before its first JSONL record is visible.
		// Retry unknowns instead of treating an incomplete session_meta as either
		// a match or proof that the rollout belongs to another process. The error
		// is returned only if the caller's bounded retry window expires.
		err := fmt.Errorf("%w: %d new rollout files appeared and %d had uncorrelatable session metadata",
			errCodexCaptureUndecided, len(observed), uncorrelated)
		if unreadable != nil {
			// A header af could not open or read at all — permissions, an I/O
			// error, a symlink it refuses to follow — is a channel it could not
			// look through, not a header still being written: it says nothing
			// about which thread the runtime is on (#7140). It keeps the
			// undecided wrap, so the same settle gives it time to become
			// readable.
			err = captureChannelUnavailable(fmt.Errorf("%w; af could not read one: %w", err, unreadable))
		}
		return AgentConversationData{}, true, err
	}
	if len(candidates) == 0 {
		return AgentConversationData{}, true, nil
	}
	// A same-directory match only says WHICH DIRECTORY a rollout names; it
	// says nothing about WHICH PROCESS wrote it. A sibling Codex sharing the
	// worktree and the account-scoped store mints exactly this evidence
	// shape, and adopting it records a foreign thread as this session's
	// conversation (#6847). Appearance alone never earned adoption: the
	// rollout this session owns is the one its runtime holds open — Codex
	// keeps the write descriptor on its thread file for the process's life.
	attributed, inconclusive := snap.attributeCodexRollouts(candidates)
	switch {
	case inconclusive:
		// Evidence exists but could not be read — a process table or
		// descriptor set that failed mid-walk. Like a mid-write
		// session_meta it is a not-yet-answer, so it gets the same
		// bounded settle rather than a verdict.
		return AgentConversationData{}, true, captureChannelUnavailable(fmt.Errorf(
			"%w: could not enumerate the runtime's open files to attribute %d new rollout files",
			errCodexCaptureUndecided, len(candidates)))
	case len(attributed) == 1:
		return AgentConversationData{
			Agent:       tmux.ProgramCodex,
			ID:          codexConversationIDFromPath(attributed[0]),
			CapturedAt:  time.Now(),
			CaptureKind: ConversationCaptureCodexRollout,
		}, false, nil
	case len(attributed) > 1:
		return AgentConversationData{}, false, fmt.Errorf(
			"codex conversation capture ambiguous: %d new rollout files are held open by the session's runtime",
			len(attributed))
	default:
		// Every candidate is provably another process's thread — a same-cwd
		// sibling, not this session's — or the runtime has no verified agent
		// process to judge from, and the active pane is no substitute (#7123).
		// Unknown stays unrecorded; the caller keeps waiting for the rollout
		// the runtime itself mints.
		return AgentConversationData{}, true, nil
	}
}

func newCodexRolloutFiles(snap ConversationCaptureSnapshot) ([]string, error) {
	if snap.scopeOnly {
		return nil, nil
	}
	if snap.scopeErr != nil {
		return nil, captureChannelUnavailable(snap.scopeErr)
	}
	if snap.codexBeforeErr != nil {
		return nil, captureChannelUnavailable(fmt.Errorf("the snapshot's scan of the Codex rollout store failed: %w", snap.codexBeforeErr))
	}
	// The diff scan descends only into day directories that could hold a
	// rollout minted around or after the snapshot; the daemon re-asks this on
	// every rearm for the life of an unrecorded session, so its cost must be
	// bounded by what this session could have produced, not by the size of
	// the user's whole Codex history (#6845).
	after, err := codexRolloutFilesSince(snap.codexHome, codexRolloutScanFloor(snap.startedAt))
	if err != nil {
		return nil, captureChannelUnavailable(fmt.Errorf("scan of the Codex rollout store failed: %w", err))
	}
	var candidates []string
	for path := range after {
		if _, existed := snap.codexBefore[path]; existed {
			continue
		}
		// The stat is bounded (fsprobe, #7378): the store may sit on a mount
		// that stopped answering. One that did not answer, or failed, leaves
		// the rollout's age unknown, so capture waits on it rather than adopt
		// a rollout that may predate the snapshot. A rollout gone since the
		// scan stays a candidate, as before: its header read reports it.
		info, err := fsprobe.Stat(path)
		switch {
		case err == nil && info.ModTime().Before(snap.startedAt.Add(-time.Second)):
			continue
		case err != nil && !pathutil.IsAbsent(err):
			return nil, captureChannelUnavailable(fmt.Errorf("%w: af could not read the age of new rollout %s: %w",
				errCodexCaptureUndecided, path, err))
		}
		candidates = append(candidates, path)
	}
	return candidates, nil
}

// resolveCaptureWorkingDir resolves the working directory a capture binds
// discovery to, bounded (fsprobe, #7292): it is the session's launch dir,
// which may sit on a mount that no longer answers. scopeErr is the lookup
// that did not answer or failed, with workingDir then only cleaned; "" stays
// "", an unbound capture.
func resolveCaptureWorkingDir(workingDir string) (resolved string, scopeErr error) {
	if workingDir == "" {
		return "", nil
	}
	resolved, err := fsprobe.ResolveForCompare(workingDir)
	if err != nil {
		return filepath.Clean(workingDir), fmt.Errorf("af could not resolve the launch directory %s to bind conversation capture to: %w", workingDir, err)
	}
	return resolved, nil
}

// scopeDirFor is the launch dir a snapshot keeps to retry its resolution: the
// cleaned dir while scopeErr stands, else "".
func scopeDirFor(workingDir string, scopeErr error) string {
	if scopeErr == nil {
		return ""
	}
	return workingDir
}

// rescoped is s with its working directory resolved again when the last
// resolution did not answer or failed (#7298 review): the daemon keeps one
// snapshot for a runtime's life, and a recorded row's follow never retakes
// it, so a transient non-answer must not decide nothing forever. A retry
// while the stuck lookup is still blocked is refused at once by its latch;
// one that answers clears the error.
func (s ConversationCaptureSnapshot) rescoped() ConversationCaptureSnapshot {
	if s.scopeErr == nil {
		return s
	}
	resolved, err := resolveCaptureWorkingDir(s.scopeDir)
	if err != nil {
		s.scopeErr = err
		return s
	}
	s.workingDir, s.scopeErr, s.scopeDir = resolved, nil, ""
	if s.poolScopePending {
		// Judge the pool against the scope that now stands, as a pool
		// tracked under it would have been (#7299). A new slice: the
		// caller's copy shares the old one's backing array.
		kept := make([]trackedReceiptPath, 0, len(s.receiptPaths))
		for _, tracked := range s.receiptPaths {
			if !tracked.unpinned || resolved == "" {
				kept = append(kept, tracked)
				continue
			}
			membership, why, _ := classifyReceiptPoolMember(tracked.path, resolved)
			if membership == poolMemberForeign {
				continue
			}
			tracked.membership, tracked.memberWhy, tracked.memberChecked = membership, why, time.Now()
			kept = append(kept, tracked)
		}
		s.receiptPaths, s.poolScopePending = kept, false
	}
	return s
}

// ScopeErr is why the working directory this snapshot binds discovery to did
// not resolve, or nil (#7292). While it stands the snapshot decides nothing;
// a swap that armed it refuses before anything is torn down.
func (s *ConversationCaptureSnapshot) ScopeErr() error {
	if s == nil {
		return nil
	}
	return s.scopeErr
}
