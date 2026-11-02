package planusage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// FileMark is how far one transcript has been metered.
type FileMark struct {
	// Offset counts bytes of complete lines read.
	Offset int64 `json:"offset"`
	// Last is the key of the last message counted, so a message whose lines
	// straddle two reads counts once even without a shared Seen set.
	Last string `json:"last,omitempty"`
	// ModTime is the file's modification time, in Unix nanoseconds, when it
	// was last read; an unchanged file is not opened again.
	ModTime int64 `json:"mtime,omitempty"`
	// Size is the file's size when it was last read. A file with an unchanged
	// modification time but a different size has grown: coarse-timestamp
	// filesystems do not move the time on every append.
	Size int64 `json:"size,omitempty"`
	// Skipped lists (comma-joined, sorted) the models whose messages in this
	// file were read past because they had no price. When one of them gets a
	// price the file is read again from the start; Seen keeps what was already
	// counted from counting twice.
	Skipped string `json:"skipped,omitempty"`
}

// Entry is the cost of one assistant message.
type Entry struct {
	// Key identifies the message across transcript copies; empty when the
	// message has no id.
	Key  string
	At   time.Time
	Cost float64
}

// ReadOptions says how ReadTranscript treats what it reads.
type ReadOptions struct {
	Overrides map[string]Price
	// SkipBad reads past a line that cannot be parsed instead of stopping.
	SkipBad bool
	// SkipUnpriced reads past a message whose model has no price instead of
	// stopping, and lists the model in ReadResult.Skipped.
	SkipUnpriced bool
	// Seen holds the keys of the messages already counted, from any file,
	// with the message time in epoch seconds. Claude Code copies messages into
	// a new transcript on resume, fork and account swap; a key counts once.
	// ReadTranscript adds what it counts. Nil counts per read only.
	Seen map[string]int64
	// Foreign reports whether a message key was counted for ANOTHER account.
	// Claude Code copies a conversation into the destination account's config
	// dir on an account handoff; those copied messages consumed the source
	// account's plan, so they must not count again here.
	Foreign func(key string) bool
	// Within, when set, reads past the messages outside every one of its spans
	// without counting them.
	Within []Span
	// Dups, when set, makes ReadTranscript report each priced message it reads
	// past because Seen already holds its key, within Within, in
	// ReadResult.Dups. The account scan uses them to move a message to backlog
	// spend when its first copy scanned was not under a backlog path (#6618).
	Dups bool
}

// Span is a stretch of time, from From until Until; a zero bound is open.
type Span struct {
	From  time.Time `json:"from,omitzero"`
	Until time.Time `json:"until,omitzero"`
}

// Holds is whether at falls in the span.
func (s Span) Holds(at time.Time) bool {
	return (s.From.IsZero() || !at.Before(s.From)) && (s.Until.IsZero() || at.Before(s.Until))
}

// within is whether at falls in one of spans; with none, every time does.
func within(at time.Time, spans []Span) bool {
	if len(spans) == 0 {
		return true
	}
	for _, s := range spans {
		if s.Holds(at) {
			return true
		}
	}
	return false
}

// ReadResult is what one read of a transcript found.
type ReadResult struct {
	Mark    FileMark
	Entries []Entry
	// Unpriced is the model of the message reading stopped at: it has no price.
	Unpriced string
	// Skipped lists the models with no price that SkipUnpriced read past.
	Skipped []string
	// Bad is set when reading stopped at a line that could not be parsed.
	Bad bool
	// Dups lists the copies of already counted messages read past, when
	// ReadOptions.Dups is set. They are not in Entries and not counted.
	Dups []Entry
}

// Cost sums the entries.
func (r ReadResult) Cost() float64 {
	total := 0.0
	for _, e := range r.Entries {
		total += e.Cost
	}
	return total
}

type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreation            *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	Speed string `json:"speed"`
}

func (u usage) cost(pr Price) float64 {
	w5, w1 := u.CacheCreationInputTokens, int64(0)
	if c := u.CacheCreation; c != nil && c.Ephemeral5m+c.Ephemeral1h > 0 {
		w5, w1 = c.Ephemeral5m, c.Ephemeral1h
	}
	return (float64(u.InputTokens)*pr.Input + float64(w5)*pr.CacheWrite5m + float64(w1)*pr.CacheWrite1h +
		float64(u.CacheReadInputTokens)*pr.CacheRead + float64(u.OutputTokens)*pr.Output) / 1e6
}

func (u usage) prompt() int64 {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

func (u usage) empty() bool {
	return u.prompt()+u.OutputTokens == 0
}

type transcriptLine struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	RequestID string    `json:"requestId"`
	Message   *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *usage `json:"usage"`
	} `json:"message"`
}

// messageKey identifies one API response across files: a hash of the message
// id and request id, short so a week of keys stays small on disk.
func messageKey(id, requestID string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(requestID))
	return strconv.FormatUint(h.Sum64(), 36)
}

var usageKey = []byte(`"usage"`)

// ReadTranscript meters a Claude Code transcript from mark: complete lines
// only, each assistant message once by message and request id. It stops
// before a line it cannot price or parse unless told to skip it, so the next
// read starts there again. A file shorter than the mark was rewritten and is
// read from the start; Seen keeps its messages from counting twice.
func ReadTranscript(path string, mark FileMark, opts ReadOptions) (ReadResult, error) {
	// O_NOFOLLOW: a transcript that is a symlink is never read, so a link planted
	// in a Claude project directory cannot make the meter open another file.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ReadResult{Mark: mark}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ReadResult{Mark: mark}, err
	}
	if info.Size() < mark.Offset {
		mark = FileMark{}
	}
	res := ReadResult{Mark: mark}
	res.Mark.ModTime = info.ModTime().UnixNano()
	res.Mark.Size = info.Size()
	if info.Size() == mark.Offset {
		return res, nil
	}
	if _, err := f.Seek(mark.Offset, io.SeekStart); err != nil {
		return res, err
	}
	seen := opts.Seen
	if seen == nil {
		seen = map[string]int64{}
	}
	if _, ok := seen[mark.Last]; mark.Last != "" && !ok {
		seen[mark.Last] = 0
		defer delete(seen, mark.Last)
	}
	skipped := map[string]bool{}
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, err
		}
		next := res.Mark.Offset + int64(len(line))
		if !bytes.Contains(line, usageKey) {
			res.Mark.Offset = next
			continue
		}
		var l transcriptLine
		if err := json.Unmarshal(line, &l); err != nil {
			if !opts.SkipBad {
				res.Bad = true
				break
			}
			res.Mark.Offset = next
			continue
		}
		msg := l.Message
		if l.Type != "assistant" || msg == nil || msg.Usage == nil || msg.Usage.empty() {
			res.Mark.Offset = next
			continue
		}
		key := ""
		if msg.ID != "" {
			key = messageKey(msg.ID, l.RequestID)
			if _, dup := seen[key]; dup {
				if opts.Dups {
					at := l.Timestamp
					if at.IsZero() {
						at = info.ModTime()
					}
					if pr, ok := priceForUsage(msg.Model, *msg.Usage, opts.Overrides); ok && within(at, opts.Within) {
						res.Dups = append(res.Dups, Entry{Key: key, At: at, Cost: msg.Usage.cost(pr)})
					}
				}
				res.Mark.Offset = next
				continue
			}
			if opts.Foreign != nil && opts.Foreign(key) {
				res.Mark.Offset = next
				continue
			}
		}
		at := l.Timestamp
		if at.IsZero() {
			at = info.ModTime()
		}
		// Read past a message outside Within without counting it, before its
		// price is looked up: one that is not counted cannot stop the read.
		if !within(at, opts.Within) {
			res.Mark.Offset = next
			continue
		}
		pr, ok := priceForUsage(msg.Model, *msg.Usage, opts.Overrides)
		if !ok {
			if !opts.SkipUnpriced {
				res.Unpriced = msg.Model
				break
			}
			if !skipped[msg.Model] {
				skipped[msg.Model] = true
				res.Skipped = append(res.Skipped, msg.Model)
			}
			res.Mark.Offset = next
			continue
		}
		res.Entries = append(res.Entries, Entry{Key: key, At: at, Cost: msg.Usage.cost(pr)})
		if key != "" {
			seen[key] = at.Unix()
			res.Mark.Last = key
		}
		res.Mark.Offset = next
	}
	res.Mark.Skipped = mergeSkipped(mark.Skipped, res.Skipped)
	return res, nil
}

// mergeSkipped joins the models a file has skipped so far with new ones.
func mergeSkipped(have string, more []string) string {
	set := map[string]bool{}
	for _, m := range strings.Split(have, ",") {
		if m != "" {
			set[m] = true
		}
	}
	for _, m := range more {
		set[m] = true
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// TranscriptFiles lists the transcripts in Claude project directories: each
// conversation file and its subagents' files.
func TranscriptFiles(projectDirs []string) []string {
	var out []string
	for _, dir := range projectDirs {
		for _, pattern := range []string{"*.jsonl", filepath.Join("*", "subagents", "*.jsonl")} {
			matches, _ := filepath.Glob(filepath.Join(dir, pattern))
			for _, m := range matches {
				// Lstat, not Stat: a symlink is skipped, not followed.
				if info, err := os.Lstat(m); err == nil && info.Mode().IsRegular() {
					out = append(out, m)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// ConversationMains are the main transcript files a conversation id has in
// Claude project directories: <dir>/<id>.jsonl for each, whether it exists yet
// or not. An id that is not a plain file name has none.
func ConversationMains(projectDirs []string, id string) []string {
	if !conversationIDOK(id) {
		return nil
	}
	var out []string
	for _, dir := range projectDirs {
		if dir != "" {
			out = append(out, filepath.Join(dir, id+".jsonl"))
		}
	}
	return out
}

// ConversationMain is the main transcript file of the conversation path
// belongs to: path itself, or for a subagent's file
// (<dir>/<id>/subagents/<name>.jsonl), <dir>/<id>.jsonl.
func ConversationMain(path string) string {
	if sub := filepath.Dir(path); filepath.Base(sub) == "subagents" {
		return filepath.Dir(sub) + ".jsonl"
	}
	return path
}

// ConversationTranscripts are the transcript files of the conversations whose
// main files are mains: each main file, listed whether it exists yet or not so
// a reader keeps its place from the start, and its subagents' files that exist.
func ConversationTranscripts(mains []string) []string {
	var out []string
	for _, m := range mains {
		out = append(out, m)
		subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(m, ".jsonl"), "subagents", "*.jsonl"))
		for _, f := range subs {
			if info, err := os.Stat(f); err == nil && info.Mode().IsRegular() {
				out = append(out, f)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ConversationPaths are the paths that hold the conversations whose main files
// are mains, for the backlog paths of a scan: each main file and the directory
// of its subagents. Another session's transcript in the same project directory
// is not under any of them.
func ConversationPaths(mains []string) []string {
	var out []string
	for _, m := range mains {
		out = append(out, m, strings.TrimSuffix(m, ".jsonl"))
	}
	return out
}

func conversationIDOK(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\*?[`)
}

// PruneSeen drops the keys of messages older than the weekly window and a day.
func PruneSeen(seen map[string]int64, now time.Time) bool {
	cutoff := now.Add(-keepFor).Unix()
	changed := false
	for k, at := range seen {
		if at < cutoff {
			delete(seen, k)
			changed = true
		}
	}
	return changed
}
