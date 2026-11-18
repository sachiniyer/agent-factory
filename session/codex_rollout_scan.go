package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
)

var codexRolloutFileRE = regexp.MustCompile(`(?i)^rollout-.+-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// codexRolloutStoreLookback is how far behind a capture snapshot the bounded
// scan still descends. Codex files a rollout under sessions/YYYY/MM/DD named
// from the recorder's LOCAL calendar date (codex-rs rollout recorder
// precompute_new_rollout_path: OffsetDateTime::now_local), while the snapshot
// stamps af's own clock — the same instant can land on dates ~26h apart
// across the two, and the clocks themselves can be skewed. 48h covers the
// worst combination with margin while still excluding every older day
// directory in the store (#6845).
const codexRolloutStoreLookback = 48 * time.Hour

// codexRolloutScanFloor returns the earliest instant a rollout belonging to a
// capture armed at startedAt could possibly have been written.
func codexRolloutScanFloor(startedAt time.Time) time.Time {
	return startedAt.Add(-codexRolloutStoreLookback)
}

// codexRolloutFiles enumerates every rollout file under codexHome's sessions
// store — the unbounded form for callers that can legitimately touch a
// rollout of any age: pinning an explicit `codex resume` id
// (CodexRolloutPathByID), sizing the unpinned resume pool
// (TrackCodexReceiptPool), checking a recorded conversation for staleness, or
// locating a carried rollout across an account swap.
func codexRolloutFiles(codexHome string) (map[string]struct{}, error) {
	return codexRolloutFilesSince(codexHome, time.Time{})
}

// codexRolloutFilesSince enumerates the store like codexRolloutFiles but
// never descends into a sessions/YYYY/MM/DD directory whose date provably
// predates floor's date: the recorder names that directory from its local
// clock at mint time, so the date is a hard bound on how early anything
// beneath it was written. The daemon's capture loop re-runs this scan on
// every rearm for the life of an unrecorded session — the unbounded walk
// that made it ~12.5% of daemon allocations on a well-used store (#6845).
// A zero floor walks the whole store.
//
// The fail-closed rule is unchanged: a directory the scan still needs but
// cannot traverse is an error, never an empty-looking store (#5471). A
// directory outside the window is never opened, so its unreadability cannot
// fail the scan either.
func codexRolloutFilesSince(codexHome string, floor time.Time) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if codexHome == "" {
		return out, nil
	}
	root := filepath.Join(codexHome, "sessions")
	err := walkConversationStore(root, false, func(path string, d os.DirEntry, err error) error {
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
		if d == nil {
			return nil
		}
		if d.IsDir() {
			if rel, relErr := filepath.Rel(root, path); relErr == nil &&
				codexRolloutDirBeforeFloor(rel, floor) {
				return filepath.SkipDir
			}
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

// codexRolloutDirBeforeFloor reports whether rel — a directory's path
// relative to the sessions root — is a complete YYYY/MM/DD date predating
// floor's date. Only the strict leaf proves it: the recorder writes
// zero-padded four/two/two-digit levels, so shallower levels, deeper
// descendants, unpadded fields, and malformed or impossible dates all prove
// nothing, and the walk descends into each of them rather than guess.
// Comparison is civil-day against civil-day so the floor's own date always
// stays inside the window.
func codexRolloutDirBeforeFloor(rel string, floor time.Time) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || len(parts[0]) != 4 || len(parts[1]) != 2 || len(parts[2]) != 2 {
		return false
	}
	day, err := time.ParseInLocation("2006/01/02", strings.Join(parts, "/"), floor.Location())
	if err != nil {
		return false
	}
	y, m, d := floor.Date()
	return day.Before(time.Date(y, m, d, 0, 0, 0, 0, floor.Location()))
}

func codexConversationIDFromPath(path string) string {
	matches := codexRolloutFileRE.FindStringSubmatch(filepath.Base(path))
	if len(matches) != 2 {
		return ""
	}
	return strings.ToLower(matches[1])
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
	path, _ := codexRolloutPathByID(codexHome, id)
	return path
}

// codexRolloutPathByID is CodexRolloutPathByID that also returns the listing's
// error, so a store af could not list (it did not answer, or failed) is told
// apart from one that answered with no rollout for id, or several.
func codexRolloutPathByID(codexHome, id string) (string, error) {
	files, err := codexRolloutFiles(codexHome)
	if err != nil {
		return "", err
	}
	var path string
	for file := range files {
		if strings.EqualFold(codexConversationIDFromPath(file), id) {
			if path != "" {
				return "", nil
			}
			path = file
		}
	}
	return path, nil
}

// codexRolloutFilesForWorkingDir selects the rollouts that can be the
// launch's own top-level conversation: correlated to workingDir when it is
// bound, and never a provable sub-agent thread — Codex writes one rollout per
// sub-agent in the parent's directory, marked by its session_meta source, and
// a sub-agent's file is not a capture candidate, receipt evidence, or a newer
// thread for carry staleness (#6766). uncorrelated counts files whose
// session_meta cannot be classified at all: a bound snapshot must keep
// waiting on them rather than decide around a header Codex has not finished
// writing. An unbound snapshot keeps its historical contract — every
// unclassifiable file stays a candidate — except for provable sub-agents,
// which no correlation rule can excuse.
func codexRolloutFilesForWorkingDir(paths []string, workingDir string) ([]string, int) {
	matches, uncorrelated, _, _ := classifyCodexRolloutFiles(paths, workingDir)
	return matches, uncorrelated
}

// classifyCodexRolloutFiles is codexRolloutFilesForWorkingDir that also
// reports why af could not read the uncorrelated files' headers, when it could
// read none of them: an unreadable header is a channel af could not look
// through, while a readable one that does not parse is an answer (#7140). A
// window holding both is an answer, so it reports nil (#7160). unresolved
// counts the uncorrelated files whose header read did not answer, or whose cwd
// did not resolve, within its bound: a lookup that did not happen rather than
// a header still being written (#7370 review R3).
func classifyCodexRolloutFiles(paths []string, workingDir string) (matches []string, uncorrelated, unresolved int, unreadable error) {
	if workingDir != "" && !filepath.IsAbs(workingDir) {
		return nil, len(paths), 0, nil
	}
	unreadableCount := 0
	for _, path := range paths {
		cwd, subagent, ok, readErr, _ := codexRolloutHeader(path)
		switch {
		case !ok:
			if workingDir == "" {
				// Unclassified is not proven a sub-agent: an unbound
				// snapshot treats every file as a candidate.
				matches = append(matches, path)
			} else {
				uncorrelated++
				// A first record still mid-write is a readable channel
				// whose answer has not landed — uncorrelated, never an
				// unreadable one (#7140; #7475 review R2).
				if readErr != nil && !errors.Is(readErr, errCodexSessionMetaIncomplete) {
					unreadableCount++
					if unreadable == nil {
						unreadable = readErr
					}
					if codexHeaderReadUnanswered(readErr) {
						unresolved++
					}
				}
			}
		case subagent:
			// Provably a sub-agent thread: never this session's own
			// top-level conversation, whatever the correlation.
		case workingDir == "":
			matches = append(matches, path)
		default:
			// The cwd a rollout records is usually the launch dir, which may
			// sit on a mount that no longer answers: the resolution is bounded
			// (fsprobe, #7279), and one that did not answer is a channel af
			// could not look through, never a rollout from another directory.
			resolved, err := fsprobe.ResolveForCompare(cwd)
			switch {
			case err != nil:
				uncorrelated++
				unresolved++
				unreadableCount++
				if unreadable == nil {
					unreadable = err
				}
			case resolved == workingDir:
				matches = append(matches, path)
			}
		}
	}
	if unreadableCount < uncorrelated {
		unreadable = nil
	}
	return matches, uncorrelated, unresolved, unreadable
}

func codexRolloutWorkingDir(path string) (string, bool) {
	cwd, _, ok := codexRolloutSessionMeta(path)
	return cwd, ok
}

// codexRolloutSessionMeta classifies a rollout's session_meta record — always
// the first JSONL line. ok reports only that the header is complete enough to
// judge; subagent reports whether its source PROVES the file is a sub-agent
// thread, which can never be the session's own top-level conversation (#6766).
func codexRolloutSessionMeta(path string) (cwd string, subagent, ok bool) {
	cwd, subagent, ok, _, _ = codexRolloutHeader(path)
	return cwd, subagent, ok
}

// codexRolloutHeader is codexRolloutSessionMeta that also says why the header
// could not be judged: unreadable when af could not open or read it at all OR
// when the first record is not yet a complete line — both transient — and
// unjudged naming why a header it read whole still cannot classify — not
// session_meta, unparseable, or recording no absolute cwd. A first record
// over the read bound was read, just too large to judge, so it reports its
// reason as unjudged rather than as a channel af could not look through.
// unjudged is a permanent property of the file, not a lookup that failed:
// callers that keep unclassifiable files warn with it so a Codex format
// change is visible (#7470).
func codexRolloutHeader(path string) (cwd string, subagent, ok bool, unreadable error, unjudged string) {
	rec, err := readCodexRolloutFirstLine(path)
	if err != nil {
		if errors.Is(err, errCodexSessionMetaOverBound) {
			return "", false, false, nil, "the first record exceeds the session_meta read bound"
		}
		return "", false, false, err, ""
	}
	if !rec.complete {
		// The first record's newline has not landed: the file is empty or
		// its header is still mid-write. Transient — the record can still
		// complete — so it rides the lookup-failure channel, never the
		// permanent unjudged one (#7475 review R2).
		return "", false, false, errCodexSessionMetaIncomplete, ""
	}
	line := rec.line
	var metadata struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd    string          `json:"cwd"`
			Source json.RawMessage `json:"source"`
		} `json:"payload"`
	}
	switch {
	case json.Unmarshal(line, &metadata) != nil:
		return "", false, false, nil, "the session_meta record is not parseable"
	case metadata.Type != "session_meta":
		return "", false, false, nil, "the first record is not session_meta"
	case !filepath.IsAbs(metadata.Payload.Cwd):
		return "", false, false, nil, "the session_meta records no absolute cwd"
	}
	return metadata.Payload.Cwd, codexRolloutIsSubagentSource(metadata.Payload.Source), true, nil, ""
}

// codexRolloutIsSubagentSource reports whether a session_meta source proves the
// rollout is a sub-agent thread. Codex serializes SessionSource as a bare
// string for top-level launches (cli, exec, vscode, …) and as a one-key object
// for data-bearing variants — a sub-agent's is {"subagent":…}. Only that shape
// is a positive verdict: an absent source (rollouts predate the field) or a
// shape af does not know stays top-level — unknown is not proven foreign
// (#6766). Deliberately NOT a check for source == "cli": exec and vscode
// launches are top-level conversations too, and af itself launches codex exec.
func codexRolloutIsSubagentSource(source json.RawMessage) bool {
	var variant map[string]json.RawMessage
	if err := json.Unmarshal(source, &variant); err != nil {
		return false
	}
	_, isSub := variant["subagent"]
	return isSub
}

// codexReceiptScanCap bounds the bytes one receipt scan reads past its
// boundary. The briefing is the first turn the launch submits, so it lies just
// past the boundary, after at most the session_meta record (itself capped at
// codexSessionMetaReadCap) and the context items Codex records before the
// first turn. A transcript that grows without limit must not make one poll
// read without limit.
const codexReceiptScanCap = 4 * codexSessionMetaReadCap

// codexReceiptScanCapOverride is a test's lower cap, 0 in production. Atomic,
// and loaded once in the caller, so an abandoned scan never reads a later
// test's value.
var codexReceiptScanCapOverride atomic.Int64

func codexReceiptScanLimit() int64 {
	if limit := codexReceiptScanCapOverride.Load(); limit > 0 {
		return limit
	}
	return codexReceiptScanCap
}

// codexRolloutHasPrompt reports whether the rollout at path records prompt as
// a user turn after fromOffset. The open, the seek and the read run in one
// fsprobe flight (#7378): the receipt scan runs on every poll of a receipt
// wait, which a wedged CODEX_HOME must not hang. A scan that does not answer
// is the ErrProbeTimedOut + context.DeadlineExceeded non-answer.
func codexRolloutHasPrompt(path, prompt string, fromOffset int64) (bool, error) {
	// The flight's question names everything the read depends on, so only a
	// scan for the same prompt from the same boundary joins it.
	digest := fnv.New64a()
	_, _ = digest.Write([]byte(prompt))
	question := fmt.Sprintf("Codex rollout receipt scan (from byte %d, prompt %016x)", fromOffset, digest.Sum64())
	limit := codexReceiptScanLimit()
	return fsprobe.ReadFileWithOpen(question, path, codexRolloutBoundedOpen(), func(file *os.File) (bool, error) {
		return scanCodexRolloutForPrompt(file, path, prompt, fromOffset, limit)
	})
}

// scanCodexRolloutForPrompt is codexRolloutHasPrompt's read, on the opened
// rollout.
func scanCodexRolloutForPrompt(file *os.File, path, prompt string, fromOffset, limit int64) (bool, error) {
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
	data, err := io.ReadAll(io.LimitReader(file, limit))
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

// codexRolloutOpenStandIn is a test's stand-in for the open behind every
// bounded rollout read, the header read (#7279) and the receipt scan (#7378);
// nil in production. It is atomic, and loaded
// once in the caller: a read that outlives its bound by design can still be
// running when its test puts the real open back (#7278 review).
var codexRolloutOpenStandIn atomic.Pointer[func(string) (*os.File, error)]

// codexRolloutBoundedOpen is the open a bounded rollout read runs inside its
// bound.
func codexRolloutBoundedOpen() func(string) (*os.File, error) {
	if open := codexRolloutOpenStandIn.Load(); open != nil {
		return *open
	}
	return openCodexRollout
}

// openCodexRollout opens a Codex rollout — fsprobe.OpenRegular, the one
// guarded open every bounded filesystem caller shares (#7527). O_NOFOLLOW
// rejects a symlink planted at the leaf, O_NONBLOCK keeps a FIFO planted
// there from hanging it, and a non-regular file swapped in after the listing
// is refused with *fsprobe.NonRegularError; all are no-ops on the regular
// file a real rollout is.
func openCodexRollout(path string) (*os.File, error) {
	file, _, err := fsprobe.OpenRegular(path)
	if err != nil {
		return nil, fmt.Errorf("open Codex rollout %s: %w", path, err)
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

var errCodexSessionMetaOverBound = fmt.Errorf("first record exceeds %d-byte bound", codexSessionMetaReadCap)

// errCodexSessionMetaIncomplete marks a first record that is not yet a
// complete line — an empty file, or a header still mid-write. It is transient
// like a lookup that did not answer, never a permanent unclassifiable
// (#7475 review R2).
var errCodexSessionMetaIncomplete = errors.New("the first record is not yet complete")

// codexRolloutHeaderQuestion is what readCodexRolloutFirstLine asks of a
// rollout: its bounded flights are joined by no other read of the file.
const codexRolloutHeaderQuestion = "Codex rollout header read"

// readCodexRolloutFirstLine reads the first line of the rollout at path — its
// session_meta record — inside fsprobe's per-path bound (#7279). The header
// read runs on the follow/capture loop's every rearm, on the set-conversation
// RPC handler, and on the restore and carry staleness checks, none of which a
// wedged CODEX_HOME may hang: the open and the read run in one bounded flight,
// and one that does not answer is the ErrProbeTimedOut +
// context.DeadlineExceeded non-answer (codexHeaderReadUnanswered).
func readCodexRolloutFirstLine(path string) (codexRolloutFirstRecord, error) {
	return fsprobe.ReadFileWithOpen(codexRolloutHeaderQuestion, path, codexRolloutBoundedOpen(), func(file *os.File) (codexRolloutFirstRecord, error) {
		reader := bufio.NewReader(file)
		var rec codexRolloutFirstRecord
		for {
			fragment, rerr := reader.ReadSlice('\n')
			rec.line = append(rec.line, fragment...)
			if len(rec.line) > codexSessionMetaReadCap {
				return rec, fmt.Errorf("read Codex rollout %s: %w", path, errCodexSessionMetaOverBound)
			}
			switch {
			case rerr == nil:
				rec.line = bytes.TrimSuffix(rec.line, []byte{'\n'})
				rec.complete = true
				return rec, nil
			case errors.Is(rerr, io.EOF):
				rec.line = bytes.TrimSuffix(rec.line, []byte{'\n'})
				return rec, nil
			case errors.Is(rerr, bufio.ErrBufferFull):
				continue
			default:
				return rec, fmt.Errorf("read Codex rollout %s: %w", path, rerr)
			}
		}
	})
}

// codexRolloutFirstRecord is the first line of a rollout — its session_meta
// record — with whether it is newline-terminated. A record whose newline has
// not landed (an empty file, or a line still mid-write) is not yet the
// session_meta it will be: it is transient, and only a complete record can
// memoize a classification verdict (#7475 review R2).
type codexRolloutFirstRecord struct {
	line     []byte
	complete bool
}

// codexHeaderReadUnanswered reports whether err, from a rollout header read,
// is a read that did not answer within its bound: a lookup that did not
// happen. Every caller counts it with the other bounded lookups that did not
// answer, never with a header that answered but is still being written (#7370
// review R3): the restore resumes, the carry refuses, and capture waits. A
// read that failed outright (EIO, EACCES) is an unreadable header, which keeps
// its own answer (#7140; #7287 review R5).
func codexHeaderReadUnanswered(err error) bool {
	return errors.Is(err, fsprobe.ErrProbeTimedOut)
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
