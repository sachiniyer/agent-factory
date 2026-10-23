package session

import (
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
	// existed whose session_meta never became classifiable. It is neither
	// absence nor receipt: the file could still be the launched process's own
	// rollout mid-write, so a never-arrived verdict must not claim it. It is a
	// recognized unconfirmed outcome — callers that keep observing on
	// ErrPromptReceiptNotObserved keep observing on this too (#5471 review).
	ErrPromptReceiptUndecidable = errors.New("prompt receipt undecidable")
)

// errCodexCaptureUndecided marks a retryable observation in which at least one
// new rollout file exists but its session_meta cannot be classified yet. It is
// what separates "no evidence appeared" (absence, bounded by the caller's
// timeout) from "evidence exists and is still being written" (bounded by
// codexSessionMetaSettleTimeout on top). Both refuse if their bound expires.
var errCodexCaptureUndecided = errors.New("codex conversation capture undecided")

const promptReceiptPollInterval = 50 * time.Millisecond

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
}

func newCodexReceiptState() *codexReceiptState {
	return &codexReceiptState{scans: make(map[string]codexReceiptScan)}
}

// scanPrompt returns whether path records prompt after fromOffset, reusing
// the previous result whenever the file has not changed since the last scan.
func (s *codexReceiptState) scanPrompt(path, prompt string, fromOffset int64) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("inspect Codex rollout %s: %w", path, err)
	}
	s.mu.Lock()
	if scan, ok := s.scans[path]; ok && scan.size == info.Size() && scan.modTime.Equal(info.ModTime()) {
		s.mu.Unlock()
		return scan.matched, nil
	}
	s.mu.Unlock()
	matched, err := codexRolloutHasPrompt(path, prompt, fromOffset)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	s.scans[path] = codexReceiptScan{size: info.Size(), modTime: info.ModTime(), matched: matched}
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
// explicitly tracked paths (a `codex resume` target, which predates the
// snapshot and so can never appear among new files, #5471 review). Attribution
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
			return false, false, err
		}
	}
	switch {
	case matched > 1:
		return false, false, fmt.Errorf("%w: %d Codex rollout files recorded the briefing", ErrPromptReceiptAmbiguous, matched)
	case matched == 1:
		return true, false, nil
	default:
		return false, uncorrelated > 0, nil
	}
}

// CodexResumedRolloutPath resolves the rollout file a `codex resume` command
// will append to — the explicit conversation id, or the newest rollout for
// --last. Codex scopes `--last` to the session's working directory (only
// --all widens it), so workingDir is the launch's effective cwd; when it is
// unknown the newest-file choice cannot be proven and the target stays
// unresolved. resume reports whether program is a resume at all: a true
// resume with an empty path means the target could not be identified (a bare
// `codex resume` opens Codex's own picker; an id may match no rollout or
// several), in which case a missing receipt can never be disproven and
// verdicts must stay indeterminate rather than claiming never-arrived (#5471
// review).
func CodexResumedRolloutPath(codexHome, program, workingDir string) (path string, resume bool) {
	selector, resume := tmux.CodexResumeSelection(program)
	if !resume {
		return "", false
	}
	// A selector with no identifiable target opens Codex's own picker —
	// resumable but unidentifiable; an unmodelled option is unprovable rather
	// than guessed (same "resume, target unknown" result either way).
	id, last, all, ok := tmux.CodexResumeTarget(selector)
	if !ok || (id == "" && !last) {
		return "", true
	}
	files, err := codexRolloutFiles(codexHome)
	if err != nil {
		return "", true
	}
	if id != "" {
		// An explicit SESSION_ID is the selector; Codex documents --last as the
		// fallback "if omitted", so the id wins when both are present.
		for file := range files {
			if strings.EqualFold(codexConversationIDFromPath(file), id) {
				if path != "" {
					return "", true
				}
				path = file
			}
		}
		return path, true
	}
	var pool []string
	if all {
		// --all disables --last's working-directory scope: the resumed
		// file is the globally newest rollout, so filtering by cwd would
		// track a different conversation's transcript (#5471 review).
		pool = mapKeys(files)
	} else {
		if workingDir == "" {
			return "", true
		}
		pool, _ = codexRolloutFilesForWorkingDir(mapKeys(files), pathutil.ResolveForCompare(workingDir))
	}
	var newestMod time.Time
	for _, file := range pool {
		if info, err := os.Stat(file); err == nil && (path == "" || info.ModTime().After(newestMod)) {
			path, newestMod = file, info.ModTime()
		}
	}
	return path, true
}

func mapKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	return keys
}

func codexRolloutHasPrompt(path, prompt string, fromOffset int64) (bool, error) {
	data, err := readCodexRollout(path)
	if err != nil {
		return false, err
	}
	// Only turns appended after the boundary can be this launch's receipt: a
	// tracked `codex resume` rollout can already hold the identical briefing
	// from an earlier run (#5471 review). Events are appended as whole JSONL
	// lines, so a size snapshot at track time lands on a line boundary.
	if fromOffset > int64(len(data)) {
		return false, nil
	}
	data = data[fromOffset:]
	normalizedPrompt := normalizePromptReceiptText(prompt)
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

func readCodexRollout(path string) ([]byte, error) {
	file, err := openCodexRollout(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read Codex rollout %s: %w", path, err)
	}
	return data, nil
}

// codexSessionMetaReadCap bounds the bytes working-directory classification
// reads for a rollout's session_meta — always the first JSONL record.
// Correlation inspects that header on every poll, so reading whole transcripts
// just to reach the newline was the bulk of a long receipt wait's I/O (#5471
// review); the cap keeps even a pathological first record inside the bound.
const codexSessionMetaReadCap = 1 << 20

func readCodexRolloutFirstLine(path string) ([]byte, error) {
	file, err := openCodexRollout(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, codexSessionMetaReadCap))
	if err != nil {
		return nil, fmt.Errorf("read Codex rollout %s: %w", path, err)
	}
	line, _, _ := bytes.Cut(data, []byte{'\n'})
	return line, nil
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
