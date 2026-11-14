package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
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
	matches, uncorrelated, _ := classifyCodexRolloutFiles(paths, workingDir)
	return matches, uncorrelated
}

// classifyCodexRolloutFiles is codexRolloutFilesForWorkingDir that also
// reports why af could not read the uncorrelated files' headers, when it could
// read none of them: an unreadable header is a channel af could not look
// through, while a readable one that does not parse is an answer (#7140). A
// window holding both is an answer, so it reports nil (#7160).
func classifyCodexRolloutFiles(paths []string, workingDir string) (matches []string, uncorrelated int, unreadable error) {
	if workingDir != "" && !filepath.IsAbs(workingDir) {
		return nil, len(paths), nil
	}
	unreadableCount := 0
	for _, path := range paths {
		cwd, subagent, ok, readErr := codexRolloutHeader(path)
		switch {
		case !ok:
			if workingDir == "" {
				// Unclassified is not proven a sub-agent: an unbound
				// snapshot treats every file as a candidate.
				matches = append(matches, path)
			} else {
				uncorrelated++
				if readErr != nil {
					unreadableCount++
					if unreadable == nil {
						unreadable = readErr
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
	return matches, uncorrelated, unreadable
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
	cwd, subagent, ok, _ = codexRolloutHeader(path)
	return cwd, subagent, ok
}

// codexRolloutHeader is codexRolloutSessionMeta that also says why the header
// could not be judged when af could not open or read it at all. A first record
// over the read bound was read, just too large to judge, so it reports none.
func codexRolloutHeader(path string) (cwd string, subagent, ok bool, unreadable error) {
	line, err := readCodexRolloutFirstLine(path)
	if err != nil {
		if errors.Is(err, errCodexSessionMetaOverBound) {
			return "", false, false, nil
		}
		return "", false, false, err
	}
	var metadata struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd    string          `json:"cwd"`
			Source json.RawMessage `json:"source"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &metadata); err != nil ||
		metadata.Type != "session_meta" || !filepath.IsAbs(metadata.Payload.Cwd) {
		return "", false, false, nil
	}
	return metadata.Payload.Cwd, codexRolloutIsSubagentSource(metadata.Payload.Source), true, nil
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

var errCodexSessionMetaOverBound = fmt.Errorf("first record exceeds %d-byte bound", codexSessionMetaReadCap)

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
			return nil, fmt.Errorf("read Codex rollout %s: %w", path, errCodexSessionMetaOverBound)
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
