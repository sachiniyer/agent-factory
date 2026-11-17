package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

const promptReceiptPollInterval = 50 * time.Millisecond

// codexReceiptQuietStatInterval is the re-stat cadence for tracked rollouts
// whose size has not passed their receipt boundary. An unpinned resume pool
// can cover the whole rollout store; statting quiet files once a second keeps
// poll cost at pool-size stats per second instead of per 50ms tick, while
// files that DO grow stay on the fast poll path for receipt latency (#5486
// review).
const codexReceiptQuietStatInterval = time.Second

// codexReceiptState is the receipt-check memory carried inside a snapshot.
// scans memoizes prompt scans by (size, mtime), because a rollout that has
// not grown cannot have recorded a new turn and need not be re-read on every
// poll (#5471 review).
type codexReceiptState struct {
	mu    sync.Mutex
	scans map[string]codexReceiptScan
}
type codexReceiptScan struct {
	size    int64
	modTime time.Time
	matched bool
	// checked records when the file was last statted; hot marks a file whose
	// size has advanced past its tracked boundary. A file that is not hot is
	// re-statted only once per codexReceiptQuietStatInterval, so an unpinned
	// resume pool's poll cost stays proportional to pool size per second —
	// not pool size per 50ms tick (#5486 review).
	checked time.Time
	hot     bool
}

// trackedReceiptPath is a rollout the receipt check watches despite it not
// being a new file. offset is the file's size when tracking began: only events
// appended AFTER it can be this launch's receipt — a resumed conversation may
// already hold the identical briefing from an earlier run, and matching that
// stale turn would suppress the very diagnostics the check exists for (#5471
// review).
type trackedReceiptPath struct {
	path   string
	offset int64
	// unpinned marks pool members (TrackCodexReceiptPool): a file the resume
	// MIGHT have appended to. Its read failure makes that file unobservable —
	// it can never produce a match, and an unpinned absence stays
	// indeterminate anyway — so the error must not abort the receipt check
	// the surviving pool members still deserve (#5486 review). A pinned
	// explicit-id target keeps the hard error because it retains negative
	// verdicts.
	unpinned bool
	// membership is an unpinned member's latest classification against the
	// pool's scope (#7470). Only a member positively classified as this
	// launch's can confirm a receipt; a match in an unclassified member is
	// inconclusive. Pinned targets leave it zero: the user named the file.
	membership poolMembership
	// memberWhy, on a member that is not positively classified, names why it
	// could not be judged — reported when it alone holds the match a wait
	// ends undecidable on, so a Codex format change or a lookup that keeps
	// failing is visible (#7470, #7475 review R3).
	memberWhy string
	// memberChecked is when the member's classification was last judged,
	// for members whose last judgement was a lookup failure: those are
	// re-judged no faster than the quiet cadence pool files restat on,
	// so a member whose lookup answers with a definitive error is not
	// walked every poll (#7475 review R3).
	memberChecked time.Time
}

// poolMembership is how a receipt-pool member last classified against the
// pool's scope (#7470). Member, foreign, and unclassifiable-by-contract are
// decided answers and memoize — a rollout's session_meta is written once, so
// its classification can only settle, never change. A classification lookup
// that did not answer or failed is transient and re-judged on each check
// (repooled), because only a positively classified member's match may
// confirm a receipt.
type poolMembership uint8

const (
	// poolMemberUnjudged marks a member of a pool with no scope to classify
	// against — unscoped (--all, the picker, an unprovable selector or launch
	// directory), or a scoped pool on an unbound snapshot. Its match confirms
	// by that pool's contract: af does not predict the file.
	poolMemberUnjudged poolMembership = iota
	// poolMemberPositive classified as this launch's: the recorded cwd
	// resolved to the pool's working directory. Its match can confirm.
	poolMemberPositive
	// poolMemberUnclassifiable can never be judged — not session_meta,
	// unparseable, or no absolute cwd recorded. Unknown forever, not foreign:
	// it stays in the pool, but its match can never confirm.
	poolMemberUnclassifiable
	// poolMemberFailed had a classification lookup not answer or fail:
	// unknown for now. It stays in the pool and is re-judged on each check;
	// its match can never confirm while it stands.
	poolMemberFailed
	// poolMemberForeign classified as another directory's — provably not this
	// launch's. Never stored on a member: the classification drops it.
	poolMemberForeign
)

func newCodexReceiptState() *codexReceiptState {
	return &codexReceiptState{scans: make(map[string]codexReceiptScan)}
}

// scanPrompt returns whether path records prompt after fromOffset, reusing
// the previous result whenever the file has not changed since the last scan.
// A file whose last observed size has not passed its boundary cannot hold
// post-boundary bytes, so it is re-statted on the slow quiet cadence instead
// of on every poll; once it grows it stays on the fast path (#5486 review).
func (s *codexReceiptState) scanPrompt(path, prompt string, fromOffset int64) (bool, error) {
	s.mu.Lock()
	prev, seen := s.scans[path]
	s.mu.Unlock()
	if seen && !prev.hot && time.Since(prev.checked) < codexReceiptQuietStatInterval {
		return prev.matched, nil
	}
	info, err := fsprobe.Stat(path)
	if err != nil {
		return false, fmt.Errorf("inspect Codex rollout %s: %w", path, err)
	}
	if seen && prev.size == info.Size() && prev.modTime.Equal(info.ModTime()) {
		s.mu.Lock()
		prev.checked = time.Now()
		s.scans[path] = prev
		s.mu.Unlock()
		return prev.matched, nil
	}
	if info.Size() <= fromOffset && info.Mode().IsRegular() {
		// Nothing appended past the boundary — skip the open entirely
		// (#5486 review). A shrunken file also lands here; when it grows
		// back past the offset the fast path reads from the boundary.
		// Non-regular files (a fifo named like a rollout) still take the
		// open path so openCodexRollout can reject them explicitly.
		s.mu.Lock()
		s.scans[path] = codexReceiptScan{
			size:    info.Size(),
			modTime: info.ModTime(),
			checked: time.Now(),
		}
		s.mu.Unlock()
		return false, nil
	}
	matched, err := codexRolloutHasPrompt(path, prompt, fromOffset)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	s.scans[path] = codexReceiptScan{
		size:    info.Size(),
		modTime: info.ModTime(),
		matched: matched,
		checked: time.Now(),
		hot:     true,
	}
	s.mu.Unlock()
	return matched, nil
}

// TrackCodexReceiptRollout adds path to the set of rollouts every receipt
// check scans and reports whether the file could be bounded. The new-rollout
// set cannot contain a `codex resume` target — it exists before the snapshot
// by definition — so a resumed config agent's receipt would read as never
// observed without it (#5471 review). The current file size is recorded as
// the turn boundary: a resumed transcript can already contain the identical
// briefing from an earlier run, and only turns appended after this point are
// this launch's receipt. When the file cannot be statted there is no honest
// boundary — tracking it unbounded would let a pre-launch turn pass for this
// launch's receipt — so it reports false and the caller treats the resume
// target as unidentified.
//
// The stat is bounded (fsprobe, #7378); one that does not answer gives no
// boundary either. Unlike a stat that fails, it leaves a rollout the resume
// may append to unseen, so it also marks the wait: no check may confirm a
// lone match from another rollout (#7384 review R5).
func (s *ConversationCaptureSnapshot) TrackCodexReceiptRollout(path string) bool {
	info, err := fsprobe.Stat(path)
	if errors.Is(err, fsprobe.ErrProbeTimedOut) {
		s.receiptUntracked = true
	}
	if err != nil {
		return false
	}
	s.receiptPaths = append(s.receiptPaths, trackedReceiptPath{path: path, offset: info.Size()})
	return true
}

// TrackCodexReceiptTarget pins an explicit `codex resume <SESSION_ID>`
// target: it resolves id to its rollout (CodexRolloutPathByID) and tracks it
// (TrackCodexReceiptRollout), reporting whether the target is pinned and
// bounded. An id the store answers for with no rollout, or with several, is
// unpinned, as before, and the verdict stays positive-only. A store listing
// that does not answer, or fails, is not that: the target may be in it, so it
// marks the wait as a stat of the target that does not answer does
// (#7384 review R5).
func (s *ConversationCaptureSnapshot) TrackCodexReceiptTarget(id string) bool {
	path, err := codexRolloutPathByID(s.codexHome, id)
	switch {
	case err != nil:
		s.receiptUntracked = true
		return false
	case path == "":
		return false
	}
	return s.TrackCodexReceiptRollout(path)
}

// TrackCodexReceiptPool bounds every rollout a resume WITHOUT an explicit id
// could append to, recording each file's current size as its receipt
// boundary. Codex picks `--last` (or a picker selection) by rules that move
// between releases, so af does not predict the file: the briefing can only be
// CONFIRMED from bytes appended past one of these offsets, and its absence
// stays indeterminate rather than "never arrived" (#5486 direction change).
//
// The scoped pool is the snapshot's working-directory set plus files whose
// session metadata cannot be classified — unclassifiable is unknown, not
// foreign. unscoped (--all, the picker, an unprovable selector, or an
// unprovable launch directory) covers the whole store and is stat-only: it
// never reads transcript contents to decide membership.
func (s *ConversationCaptureSnapshot) TrackCodexReceiptPool(unscoped bool) {
	*s = s.rescoped()
	files, err := codexRolloutFiles(s.codexHome)
	if err != nil {
		// A listing that did not answer, or failed, tracks nothing: every
		// rollout the resume may append to is unseen, so no check of this
		// wait may confirm a lone match (#7384 review R5).
		s.receiptUntracked = true
		return
	}
	// A working directory that did not resolve scopes nothing (#7292), until
	// it resolves: then the pool is scoped after all (#7299).
	if !unscoped && s.scopeErr != nil {
		unscoped, s.poolScopePending = true, true
	}
	for path := range files {
		// A scoped member's classification decides whether its match may
		// confirm (#7470): unscoped pools and unbound snapshots keep every
		// member unjudged, where a match confirms by the pool's contract.
		membership, why, checked := poolMemberUnjudged, "", time.Time{}
		if !unscoped && s.workingDir != "" {
			membership, why, _ = classifyReceiptPoolMember(path, s.workingDir)
			checked = time.Now()
			if membership == poolMemberForeign {
				continue
			}
		}
		// Bounded (fsprobe, #7378): a member whose stat does not answer has
		// no honest boundary, so it cannot be tracked. Unlike one whose stat
		// fails, it may still hold the receipt, so the pool is marked
		// untracked and no check of this wait confirms a lone match. The
		// rest of the pool is still tracked, so two matches among them stay
		// ambiguous (#7384 review R2).
		info, err := fsprobe.Stat(path)
		if errors.Is(err, fsprobe.ErrProbeTimedOut) {
			s.receiptUntracked = true
			continue
		}
		if err == nil {
			s.receiptPaths = append(s.receiptPaths, trackedReceiptPath{
				path:          path,
				offset:        info.Size(),
				unpinned:      true,
				membership:    membership,
				memberWhy:     why,
				memberChecked: checked,
			})
		}
	}
}

// WaitForPromptReceipt waits for the agent's own durable conversation store to
// record prompt as a user turn. This is deliberately receiver-side: successful
// tmux load-buffer, paste-buffer and send-keys calls establish only that af's
// input path did not error, not that a modal/composer accepted a turn (#2220).
//
// Codex is currently the only provider with a local receipt af can correlate to
// a just-spawned process. Callers must take snap before spawning the pane. Other
// providers return ErrPromptReceiptUnavailable rather than manufacturing an
// acknowledgement from pane pixels.
//
// A snapshot taken with a launch working directory correlates the receipt to
// that directory's rollouts, so another Codex's concurrently created rollout
// cannot force ErrPromptReceiptAmbiguous — the same rule capture applies
// (#5471: a receipt wait outliving its spawn window otherwise turns unanswerable
// on any box where Codex sessions come and go).
func WaitForPromptReceipt(
	ctx context.Context,
	agent string,
	snap ConversationCaptureSnapshot,
	prompt string,
	timeout time.Duration,
) error {
	if agent != tmux.ProgramCodex {
		return fmt.Errorf("%w for agent %q", ErrPromptReceiptUnavailable, agent)
	}
	// Codex trims leading and trailing whitespace from a submitted turn before
	// recording it (codex-rs/tui chat_composer.rs prepare_submission_text_*),
	// so the matcher compares a trailing-whitespace-stripped prompt against the
	// recorded text. A prompt that is empty after that trim would degrade
	// strings.Contains to an empty needle (which matches every rollout), so it
	// is refused as undecidable up front rather than polling to a timeout or
	// falsely confirming (#5488).
	if normalizedReceiptPrompt(prompt) == "" {
		return fmt.Errorf("%w: prompt is empty after trimming whitespace", ErrPromptReceiptUndecidable)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(timeout)
	for {
		// Kept across polls: a scope that resolves scopes its pool once, not
		// on every poll (#7299); a pool member whose classification lookup
		// did not answer or failed is re-judged once per check, at most one
		// bound spent and no faster than the quiet cadence (#7470, #7475
		// review R1/R3).
		snap = snap.rescoped()
		snap = snap.repooled(ctx)
		received, undecided, soft, err := codexPromptReceiptOnce(snap, prompt)
		if err != nil {
			return err
		}
		if received {
			return nil
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			if undecided {
				switch {
				case soft != nil && soft.membership == poolMemberUnclassifiable:
					log.WarningLog.Printf(
						"prompt receipt: the only rollout that recorded the briefing can never be classified against the launch directory — %s: %s (a Codex format change would look exactly like this)",
						soft.memberWhy, soft.path)
				case soft != nil && soft.membership == poolMemberFailed:
					log.WarningLog.Printf(
						"prompt receipt: the only rollout that recorded the briefing was never classified against the launch directory — %s: %s",
						soft.memberWhy, soft.path)
				}
				return fmt.Errorf("%w within %s: a Codex rollout that could hold it never became classifiable, or af's read of one did not answer", ErrPromptReceiptUndecidable, timeout)
			}
			return fmt.Errorf("%w in Codex rollout within %s", ErrPromptReceiptNotObserved, timeout)
		}

		wait := promptReceiptPollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// codexPromptReceiptOnce reports one observation of the receipt store:
// whether exactly one candidate rollout recorded prompt as a user turn.
// Candidates are the NEW rollout files correlated to the snapshot's launch
// working directory — the same fail-closed rule capture uses — plus any
// tracked paths: a pinned `codex resume` target or the unpinned resume pool
// (TrackCodexReceiptPool), each bounded at its size when tracking began
// because the files predate the snapshot and so can never appear among new
// rollouts (#5471 review, #5486 direction). Attribution
// is the prompt match itself, not a previously pinned path: a candidate can
// only be proven by recording the briefing, so a same-cwd sibling appearing
// first cannot steal the identity (two config agents share AF home), and a
// sibling appearing later cannot re-widen a settled answer. Zero matches means
// the briefing exists in no candidate — absence, not ambiguity. More than one
// match means two rollouts both recorded the prompt and cannot be told apart,
// which stays fail-closed ambiguous. undecided reports a new rollout whose
// session_meta is not yet classifiable: it could be the awaited one mid-write,
// so neither absence nor ambiguity may be concluded yet.
//
// A lone match also stays inconclusive when its only producer is a scoped
// pool member never positively classified as this launch's (#7470): it may
// be this launch's rollout or a foreign one af could not prove foreign, and
// on a slow mount that classification is exactly what did not answer — the
// #7298/#7299 wrong-conversation shape. soft names that member so a wait
// ending undecidable on it can say what held the match back. A member whose
// classification failed is re-judged on each check (repooled): member, and
// its match confirms; foreign, and the match is discounted for good.
func codexPromptReceiptOnce(snap ConversationCaptureSnapshot, prompt string) (received, undecided bool, soft *trackedReceiptPath, err error) {
	snap = snap.rescoped()
	state := snap.receiptState
	if state == nil {
		state = newCodexReceiptState()
	}
	observed, err := newCodexRolloutFiles(snap)
	switch {
	case errors.Is(err, errCodexCaptureUndecided):
		// A new rollout whose age af could not read could be the receipt:
		// wait on it, as on a header still being written (#7378).
		return false, true, nil, nil
	case err != nil:
		return false, false, nil, err
	}
	candidates, uncorrelated := codexRolloutFilesForWorkingDir(observed, snap.workingDir)
	seen := make(map[string]struct{}, len(candidates)+len(snap.receiptPaths))
	matched := 0
	// proven counts matches from producers whose evidence can confirm a
	// receipt: a new-file candidate, a pinned target the user named, a
	// positively classified member, or a member of a pool with no scope to
	// classify against (#7470).
	proven := 0
	scan := func(path string, offset int64) (bool, error) {
		if _, dup := seen[path]; dup {
			return false, nil
		}
		seen[path] = struct{}{}
		return state.scanPrompt(path, prompt, offset)
	}
	// A scan that did not answer within its bound (#7378) could still be the
	// receipt, like a header still being written: the answer is undecided, so
	// the wait goes on, and neither ends nor confirms the receipt from the
	// rest. The poll stops there, since the rest are on the same mount and
	// each would wait out a bound of its own.
	for _, path := range candidates {
		has, err := scan(path, 0)
		if errors.Is(err, fsprobe.ErrProbeTimedOut) {
			return false, true, nil, nil
		} else if err != nil {
			return false, false, nil, err
		}
		if has {
			matched++
			proven++
		}
	}
	for i := range snap.receiptPaths {
		tracked := &snap.receiptPaths[i]
		has, err := scan(tracked.path, tracked.offset)
		if errors.Is(err, fsprobe.ErrProbeTimedOut) {
			return false, true, nil, nil
		}
		if err != nil {
			if tracked.unpinned {
				// A pool member that cannot be statted or read is
				// unobservable this round: it can never produce a match, and
				// an unpinned absence stays indeterminate at the verdict
				// layer regardless — so the failure must not deny the other
				// pool members their chance to confirm (#5486 review).
				continue
			}
			return false, false, nil, err
		}
		if has {
			matched++
			if !tracked.unpinned || tracked.membership == poolMemberPositive ||
				tracked.membership == poolMemberUnjudged {
				proven++
			} else {
				soft = tracked
			}
		}
	}
	switch {
	case matched > 1:
		return false, false, nil, fmt.Errorf("%w: %d Codex rollout files recorded the briefing", ErrPromptReceiptAmbiguous, matched)
	case matched == 1 && proven == 1 && uncorrelated == 0 && !snap.receiptUntracked:
		return true, false, nil, nil
	case matched == 1 && proven == 0:
		// The lone match is an unclassified pool member: inconclusive, so the
		// wait goes on while its classification is re-judged (#7470).
		return false, true, soft, nil
	default:
		// Zero matches is absence; a lone match while an unclassifiable
		// rollout could still settle into a second match is not yet a
		// unique attribution — keep waiting rather than confirm early
		// (#5471 review). So is one while a rollout af could not track may
		// hold a second (#7384 review R2, R5).
		return false, uncorrelated > 0 || snap.receiptUntracked, nil, nil
	}
}

// repooled is s with failed pool members re-judged against the pool's scope
// (#7470): membership decided by contract — member, foreign, unclassifiable —
// memoizes, but a lookup that did not happen must not freeze a member unknown
// forever, because only a positively classified member's match may confirm.
// A member that now classifies foreign drops out for good: it was never this
// launch's. One check spends at most one bound on re-judgement and stops at
// the first non-answer, as the scan does: the rest keep their stale judgement
// rather than each waiting out a bound of its own on what is plausibly the
// same mount (#7475 review R1). A member whose lookup answered is re-judged
// no faster than the quiet cadence pool files restat on, so a definitive
// error is not walked every poll (#7475 review R3); a cancelled ctx ends the
// pass the same way. It runs once per check, in the wait, so a verdict it
// reaches persists into the snapshot the next check reads. The judgements
// are bounded, and the flight latch refuses a retry whose lookup is still
// blocked. A new slice: the caller's copy shares the old one's backing
// array.
func (s ConversationCaptureSnapshot) repooled(ctx context.Context) ConversationCaptureSnapshot {
	if s.workingDir == "" {
		return s
	}
	failed := false
	for _, tracked := range s.receiptPaths {
		if tracked.unpinned && tracked.membership == poolMemberFailed {
			failed = true
			break
		}
	}
	if !failed {
		return s
	}
	kept := make([]trackedReceiptPath, 0, len(s.receiptPaths))
	i := 0
	for ; i < len(s.receiptPaths); i++ {
		tracked := s.receiptPaths[i]
		if ctx.Err() != nil {
			break
		}
		if !tracked.unpinned || tracked.membership != poolMemberFailed ||
			time.Since(tracked.memberChecked) < codexReceiptQuietStatInterval {
			kept = append(kept, tracked)
			continue
		}
		membership, why, err := classifyReceiptPoolMember(tracked.path, s.workingDir)
		if membership == poolMemberForeign {
			continue
		}
		tracked.membership, tracked.memberWhy, tracked.memberChecked = membership, why, time.Now()
		kept = append(kept, tracked)
		if errors.Is(err, fsprobe.ErrProbeTimedOut) {
			i++
			break
		}
	}
	kept = append(kept, s.receiptPaths[i:]...)
	s.receiptPaths = kept
	return s
}

// classifyReceiptPoolMember judges a rollout against a receipt pool's scope:
// poolMemberPositive when its recorded cwd resolves to workingDir,
// poolMemberForeign when it resolves to another directory, and unclassified
// otherwise — unclassifiable is unknown, not foreign. The unknowns split on
// whether the file can ever answer (#7470): poolMemberUnclassifiable names a
// header that read whole and can never be judged (not session_meta,
// unparseable, no absolute cwd — permanent), while poolMemberFailed is a
// lookup that did not answer or failed (the header read or the cwd's bounded
// resolution, #7292), including a first record still mid-write
// (#7475 review R2) — transient, so the member is re-judged. Only a
// poolMemberPositive match may confirm a receipt. On a failure it returns
// the lookup's error so the caller can tell a non-answer (ErrProbeTimedOut)
// from a definitive error, and why names what failed for the diagnostic a
// wait ending undecidable emits (#7475 review R3).
func classifyReceiptPoolMember(path, workingDir string) (poolMembership, string, error) {
	cwd, _, ok, readErr, unjudged := codexRolloutHeader(path)
	switch {
	case errors.Is(readErr, fsprobe.ErrProbeTimedOut):
		return poolMemberFailed, "its session_meta read did not answer", readErr
	case errors.Is(readErr, errCodexSessionMetaIncomplete):
		return poolMemberFailed, "its session_meta record is still incomplete", readErr
	case readErr != nil:
		return poolMemberFailed, "its session_meta read failed", readErr
	case !ok:
		return poolMemberUnclassifiable, unjudged, nil
	}
	resolved, err := fsprobe.ResolveForCompare(cwd)
	switch {
	case errors.Is(err, fsprobe.ErrProbeTimedOut):
		return poolMemberFailed, "its launch-directory lookup did not answer", err
	case err != nil:
		return poolMemberFailed, "its launch-directory lookup failed", err
	case resolved == workingDir:
		return poolMemberPositive, "", nil
	default:
		return poolMemberForeign, "", nil
	}
}
