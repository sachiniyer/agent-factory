package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

var codexRolloutFileRE = regexp.MustCompile(`(?i)^rollout-.+-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

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

const promptReceiptPollInterval = 50 * time.Millisecond

// codexReceiptQuietStatInterval is the re-stat cadence for tracked rollouts
// whose size has not passed their receipt boundary. An unpinned resume pool
// can cover the whole rollout store; statting quiet files once a second keeps
// poll cost at pool-size stats per second instead of per 50ms tick, while
// files that DO grow stay on the fast poll path for receipt latency (#5486
// review).
const codexReceiptQuietStatInterval = time.Second

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
	// can no longer distinguish "new" from "pre-existing", and every check
	// against this snapshot must surface that failure instead of guessing.
	codexBeforeErr error
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
}

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
}

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
	info, err := os.Stat(path)
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
func (s *ConversationCaptureSnapshot) TrackCodexReceiptRollout(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	s.receiptPaths = append(s.receiptPaths, trackedReceiptPath{path: path, offset: info.Size()})
	return true
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
	files, err := codexRolloutFiles(s.codexHome)
	if err != nil {
		return
	}
	for path := range files {
		if !unscoped && s.workingDir != "" {
			if cwd, ok := codexRolloutWorkingDir(path); ok &&
				pathutil.ResolveForCompare(cwd) != s.workingDir {
				continue
			}
		}
		if info, err := os.Stat(path); err == nil {
			s.receiptPaths = append(s.receiptPaths, trackedReceiptPath{
				path:     path,
				offset:   info.Size(),
				unpinned: true,
			})
		}
	}
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
	if workingDir != "" {
		workingDir = pathutil.ResolveForCompare(workingDir)
	}
	before, beforeErr := codexRolloutFiles(home)
	return ConversationCaptureSnapshot{
		startedAt:      time.Now(),
		codexHome:      home,
		workingDir:     workingDir,
		codexBefore:    before,
		codexBeforeErr: beforeErr,
		receiptState:   newCodexReceiptState(),
	}
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
		received, undecided, err := codexPromptReceiptOnce(snap, prompt)
		if err != nil {
			return err
		}
		if received {
			return nil
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			if undecided {
				return fmt.Errorf("%w within %s: a new Codex rollout's session metadata never became classifiable", ErrPromptReceiptUndecidable, timeout)
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

func codexHomeDir() string {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return home
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".codex")
	}
	return ""
}

func captureCodexConversation(snap ConversationCaptureSnapshot, timeout time.Duration) (AgentConversationData, error) {
	if snap.codexHome == "" {
		return AgentConversationData{}, nil
	}
	if snap.carried.HasID() {
		return captureCarriedCodexConversation(snap)
	}
	start := time.Now()
	absentDeadline := start.Add(timeout)
	settleDeadline := start.Add(timeout + codexSessionMetaSettleTimeout)
	for {
		conv, retry, err := captureCodexConversationOnce(snap)
		if conv.HasID() || !retry || timeout <= 0 {
			return conv, err
		}
		// Expiry while a concrete candidate is still unclassifiable is not
		// absence (#3266): the file already appeared inside the base window, so
		// the extended bound only waits for Codex to finish writing a header
		// that classification — unchanged and fail-closed — will then rule on.
		deadline := absentDeadline
		if errors.Is(err, errCodexCaptureUndecided) {
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
// than waiting out the absence timeout. If a future Codex forks on resume
// instead, the fork is the one new rollout for this directory, and capture
// follows it. It runs after the readiness wait, when a startup fork would
// already exist.
//
// The returned record is freshly stamped even when the id is unchanged: the
// caller commits it through SetAgentConversationForRuntime, which reports
// "no change" as a lost runtime.
func captureCarriedCodexConversation(snap ConversationCaptureSnapshot) (AgentConversationData, error) {
	settleDeadline := time.Now().Add(codexSessionMetaSettleTimeout)
	for {
		conv, _, err := captureCodexConversationOnce(snap)
		if conv.HasID() {
			return conv, nil
		}
		if err == nil {
			carried := snap.carried
			carried.CapturedAt = time.Now()
			return carried, nil
		}
		if !errors.Is(err, errCodexCaptureUndecided) || time.Now().After(settleDeadline) {
			return AgentConversationData{}, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func captureCodexConversationOnce(snap ConversationCaptureSnapshot) (AgentConversationData, bool, error) {
	observed, err := newCodexRolloutFiles(snap)
	if err != nil {
		// A store that cannot be enumerated is retried inside the caller's
		// bound — the same grace mid-write headers get — then surfaces as the
		// error it is rather than as absence.
		return AgentConversationData{}, true, err
	}
	candidates, uncorrelated := codexRolloutFilesForWorkingDir(observed, snap.workingDir)
	if uncorrelated > 0 {
		// Codex can create each file before its first JSONL record is visible.
		// Retry unknowns instead of treating an incomplete session_meta as either
		// a match or proof that the rollout belongs to another process. The error
		// is returned only if the caller's bounded retry window expires.
		return AgentConversationData{}, true, fmt.Errorf(
			"%w: %d new rollout files appeared and %d had uncorrelatable session metadata",
			errCodexCaptureUndecided, len(observed), uncorrelated)
	}
	switch len(candidates) {
	case 0:
		return AgentConversationData{}, true, nil
	case 1:
		return AgentConversationData{
			Agent:       tmux.ProgramCodex,
			ID:          codexConversationIDFromPath(candidates[0]),
			CapturedAt:  time.Now(),
			CaptureKind: ConversationCaptureCodexRollout,
		}, false, nil
	default:
		return AgentConversationData{}, false, fmt.Errorf(
			"codex conversation capture ambiguous: %d new rollout files matched launch working directory",
			len(candidates))
	}
}

func codexRolloutFilesForWorkingDir(paths []string, workingDir string) ([]string, int) {
	if workingDir == "" {
		return paths, 0
	}
	if !filepath.IsAbs(workingDir) {
		return nil, len(paths)
	}
	var matches []string
	uncorrelated := 0
	for _, path := range paths {
		cwd, ok := codexRolloutWorkingDir(path)
		if !ok {
			uncorrelated++
			continue
		}
		if pathutil.ResolveForCompare(cwd) == workingDir {
			matches = append(matches, path)
		}
	}
	return matches, uncorrelated
}

func codexRolloutWorkingDir(path string) (string, bool) {
	return codexRolloutSessionMeta(path)
}

func codexRolloutSessionMeta(path string) (cwd string, ok bool) {
	line, err := readCodexRolloutFirstLine(path)
	if err != nil {
		return "", false
	}
	var metadata struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &metadata); err != nil ||
		metadata.Type != "session_meta" || !filepath.IsAbs(metadata.Payload.Cwd) {
		return "", false
	}
	return metadata.Payload.Cwd, true
}

func newCodexRolloutFiles(snap ConversationCaptureSnapshot) ([]string, error) {
	if snap.codexBeforeErr != nil {
		return nil, fmt.Errorf("the snapshot's scan of the Codex rollout store failed: %w", snap.codexBeforeErr)
	}
	after, err := codexRolloutFiles(snap.codexHome)
	if err != nil {
		return nil, fmt.Errorf("scan of the Codex rollout store failed: %w", err)
	}
	var candidates []string
	for path := range after {
		if _, existed := snap.codexBefore[path]; existed {
			continue
		}
		info, err := os.Stat(path)
		if err == nil && info.ModTime().Before(snap.startedAt.Add(-time.Second)) {
			continue
		}
		candidates = append(candidates, path)
	}
	return candidates, nil
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
func codexPromptReceiptOnce(snap ConversationCaptureSnapshot, prompt string) (received, undecided bool, err error) {
	state := snap.receiptState
	if state == nil {
		state = newCodexReceiptState()
	}
	observed, err := newCodexRolloutFiles(snap)
	if err != nil {
		return false, false, err
	}
	candidates, uncorrelated := codexRolloutFilesForWorkingDir(observed, snap.workingDir)
	seen := make(map[string]struct{}, len(candidates)+len(snap.receiptPaths))
	matched := 0
	scan := func(path string, offset int64) error {
		if _, dup := seen[path]; dup {
			return nil
		}
		seen[path] = struct{}{}
		has, err := state.scanPrompt(path, prompt, offset)
		if err != nil {
			return err
		}
		if has {
			matched++
		}
		return nil
	}
	for _, path := range candidates {
		if err := scan(path, 0); err != nil {
			return false, false, err
		}
	}
	for _, tracked := range snap.receiptPaths {
		if err := scan(tracked.path, tracked.offset); err != nil {
			if tracked.unpinned {
				// A pool member that cannot be statted or read is
				// unobservable this round: it can never produce a match, and
				// an unpinned absence stays indeterminate at the verdict
				// layer regardless — so the failure must not deny the other
				// pool members their chance to confirm (#5486 review).
				continue
			}
			return false, false, err
		}
	}
	switch {
	case matched > 1:
		return false, false, fmt.Errorf("%w: %d Codex rollout files recorded the briefing", ErrPromptReceiptAmbiguous, matched)
	case matched == 1 && uncorrelated == 0:
		return true, false, nil
	default:
		// Zero matches is absence; a lone match while an unclassifiable
		// rollout could still settle into a second match is not yet a
		// unique attribution — keep waiting rather than confirm early
		// (#5471 review).
		return false, uncorrelated > 0, nil
	}
}

// CodexRolloutPathByID resolves an explicit `codex resume <SESSION_ID>`
// selector to its rollout file. Only an explicit id can pin the resumed file
// at all: `--last`, the bare picker, and unmodelled selectors resolve by
// Codex-side rules that move between releases (working-directory and source
// filtering, provider, empty-thread exclusion, recorded recency), and a wrong
// prediction only ever produces a false "never arrived" on a briefing that
// did land. Those callers must instead bound the resume pool —
// TrackCodexReceiptPool — and keep the verdict positive-only (#5486 direction
// change). An id matching no rollout, or several, returns "": the target is
// then unpinned, so a missing receipt can never be disproven and verdicts
// must stay indeterminate rather than claiming never-arrived (#5471 review).
func CodexRolloutPathByID(codexHome, id string) string {
	files, err := codexRolloutFiles(codexHome)
	if err != nil {
		return ""
	}
	var path string
	for file := range files {
		if strings.EqualFold(codexConversationIDFromPath(file), id) {
			if path != "" {
				return ""
			}
			path = file
		}
	}
	return path
}

func codexRolloutHasPrompt(path, prompt string, fromOffset int64) (bool, error) {
	file, err := openCodexRollout(path)
	if err != nil {
		return false, err
	}
	defer file.Close()

	// Only turns appended after the boundary can be this launch's receipt: a
	// tracked `codex resume` rollout can already hold the identical briefing
	// from an earlier run (#5471 review). Events are appended as whole JSONL
	// lines, so a size snapshot at track time lands on a line boundary —
	// seek to it rather than reading the whole transcript, whose pre-launch
	// history can be arbitrarily large (#5486 review). A shrunken file leaves
	// the seek past the end and reads empty, the same "no receipt" answer
	// the length check gave.
	if _, err := file.Seek(fromOffset, io.SeekStart); err != nil {
		return false, fmt.Errorf("seek Codex rollout %s: %w", path, err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return false, fmt.Errorf("read Codex rollout %s: %w", path, err)
	}
	normalizedPrompt := normalizedReceiptPrompt(prompt)
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			// Codex appends JSONL while af polls it. A read can land between the
			// write and newline of the newest event; an incomplete line is not a
			// negative receipt and will be retried on the next poll.
			continue
		}
		var payload struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		isUserTurn := event.Type == "event_msg" && payload.Type == "user_message"
		isUserItem := event.Type == "response_item" && payload.Type == "message" && payload.Role == "user"
		if !isUserTurn && !isUserItem {
			continue
		}
		var value any
		if err := json.Unmarshal(event.Payload, &value); err != nil {
			continue
		}
		if jsonValueContainsPrompt(value, normalizedPrompt) {
			return true, nil
		}
	}
	return false, nil
}

func openCodexRollout(path string) (*os.File, error) {
	file, err := os.OpenFile(
		path,
		os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open Codex rollout %s: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect opened Codex rollout %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("read Codex rollout %s: not a regular file (%s)", path, info.Mode().Type())
	}
	return file, nil
}

// codexSessionMetaReadCap bounds the bytes working-directory classification
// reads for a rollout's session_meta — always the first JSONL record. The read
// streams the record fragment-wise, so a short header costs a short read while
// a legitimately large session_meta (a fully populated base_instructions/tools
// payload can exceed 1 MiB) still parses whole instead of truncating into an
// unclassifiable file. The cap stays a hard bound against unbounded memory on
// a pathological first record (#5471 review, #5486 review).
const codexSessionMetaReadCap = 8 << 20

func readCodexRolloutFirstLine(path string) ([]byte, error) {
	file, err := openCodexRollout(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		line = append(line, fragment...)
		if len(line) > codexSessionMetaReadCap {
			return nil, fmt.Errorf("read Codex rollout %s: first record exceeds %d-byte bound", path, codexSessionMetaReadCap)
		}
		switch {
		case err == nil, errors.Is(err, io.EOF):
			return bytes.TrimSuffix(line, []byte{'\n'}), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, fmt.Errorf("read Codex rollout %s: %w", path, err)
		}
	}
}

func jsonValueContainsPrompt(value any, prompt string) bool {
	switch v := value.(type) {
	case string:
		return prompt != "" && strings.Contains(normalizePromptReceiptText(v), prompt)
	case []any:
		for _, item := range v {
			if jsonValueContainsPrompt(item, prompt) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if jsonValueContainsPrompt(item, prompt) {
				return true
			}
		}
	}
	return false
}

func normalizePromptReceiptText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

// normalizedReceiptPrompt prepares the prompt for use as the needle of a
// substring match against a recorded Codex user turn. Codex strips trailing
// whitespace from a submitted turn before it is ever recorded
// (codex-rs/tui/src/bottom_pane/chat_composer.rs prepare_submission_text_*:
// `text = text.trim().to_string()`), so a prompt that ends in whitespace —
// which configagent.BuildBriefing's output does ("\n\n") — can never appear as a
// contiguous substring of the trimmed recorded text. Trimming the prompt's
// trailing whitespace restores the symmetry: the trimmed prompt is a suffix of
// what Codex recorded. Only the trailing edge is trimmed: the matcher uses
// strings.Contains, so the prompt's leading edge must be preserved to keep it
// anchored to its actual position within the recorded message (#5488).
func normalizedReceiptPrompt(prompt string) string {
	return strings.TrimRightFunc(normalizePromptReceiptText(prompt), unicode.IsSpace)
}

func codexRolloutFiles(codexHome string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if codexHome == "" {
		return out, nil
	}
	root := filepath.Join(codexHome, "sessions")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// An entry vanishing mid-walk is normal; a store that cannot be
			// traversed is NOT an empty store. Swallowing the failure would let
			// a receipt check report never-arrived for a directory it never
			// inspected (#5471 review).
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d == nil || d.IsDir() {
			return nil
		}
		if codexConversationIDFromPath(path) == "" {
			return nil
		}
		out[path] = struct{}{}
		return nil
	})
	return out, err
}

func codexConversationIDFromPath(path string) string {
	matches := codexRolloutFileRE.FindStringSubmatch(filepath.Base(path))
	if len(matches) != 2 {
		return ""
	}
	return strings.ToLower(matches[1])
}
