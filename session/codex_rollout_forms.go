package session

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Codex files a thread under more than the one name codexRolloutFileRE reads
// (#7130). Its rollout compression (the opt-in local_thread_store_compression
// feature, codex-rs rollout/src/compression.rs) rewrites a rollout left
// unmodified for 7 days as the same name plus .zst, carrying over its mtime,
// and `codex resume` reads it back. thread/revert keeps the thread id and
// writes a new immutable rollout named rollout-<ts>-<thread>_<rollout>.jsonl
// (rollout/src/recorder.rs, rollout_file_name.rs). `codex resume <thread>`
// opens every one of these, so a recorded thread held only in them is not
// missing.
//
// af reads none of them anywhere else. A compressed header is zstd that af
// does not decode, so a compressed rollout can never be classified as another
// thread's; a revert rollout of another thread would need the thread's other
// rollouts to date it. Neither joins the store walk the staleness check reads
// its candidates from — exactly as before this change — and the forms are
// looked up only for the recorded id, and only when no plain rollout names it.
//
// The cost of that is accepted (#7134 review F-1): a newer thread the user
// moved to that Codex has also compressed is no candidate, so a compressed
// recorded thread is resumed over it where af used to call the record
// missing and start fresh. Seeing it needs a zstd decoder to read its header.

// codexRolloutFormRE names every file Codex keeps a thread under: group 1 is
// the thread id, group 2 the revert's rollout id ("" for the thread's own
// rollout), group 3 ".zst" when compressed. The timestamp is the exact
// YYYY-MM-DDThh-mm-ss Codex's resolver requires (rollout_file_name.rs), so a
// name `codex resume` would skip is never taken for the thread (#7134 review
// F-4).
var codexRolloutFormRE = regexp.MustCompile(`(?i)^rollout-[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}-` +
	`([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})` +
	`(?:_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}))?\.jsonl(\.zst)?$`)

const (
	codexRolloutMissingReason    = "the recorded conversation's rollout is missing from the Codex home this session uses"
	codexRolloutDuplicatedReason = "af could not confirm the recorded conversation is still the newest one in this worktree: two rollouts record it"
)

// codexRolloutForms returns the regular files beneath codexHome's store that
// hold thread id: plain, its plain rollouts, and other, the rest —
// compressed, a thread/revert rollout, or a compressed revert rollout. A
// symlink is skipped, as Codex's own filename scan skips it. Like the plain
// walk it fails closed: a directory it cannot traverse is an error, never an
// empty-looking store (#5471).
func codexRolloutForms(codexHome, id string) (plain, other []string, err error) {
	return codexRolloutFormsSince(codexHome, id, time.Time{})
}

// codexRolloutFormsSince is codexRolloutForms that never descends into a
// sessions/YYYY/MM/DD directory whose date provably predates floor's
// (codexRolloutDirBeforeFloor), so a directory outside the window is never
// opened and cannot fail the walk. A zero floor walks the whole store.
func codexRolloutFormsSince(codexHome, id string, floor time.Time) (plain, other []string, err error) {
	if strings.TrimSpace(codexHome) == "" {
		return nil, nil, nil
	}
	root := filepath.Join(codexHome, "sessions")
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d != nil && d.IsDir() {
			if rel, relErr := filepath.Rel(root, path); relErr == nil && codexRolloutDirBeforeFloor(rel, floor) {
				return filepath.SkipDir
			}
			return nil
		}
		if d == nil || !d.Type().IsRegular() {
			return nil
		}
		m := codexRolloutFormRE.FindStringSubmatch(d.Name())
		switch {
		case len(m) != 4 || !strings.EqualFold(m[1], id):
		case m[2] == "" && m[3] == "":
			plain = append(plain, path)
		default:
			other = append(other, path)
		}
		return nil
	})
	return plain, other, err
}

// codexRolloutCompressed reports whether path names a compressed rollout.
func codexRolloutCompressed(path string) bool {
	m := codexRolloutFormRE.FindStringSubmatch(filepath.Base(path))
	return len(m) == 4 && m[3] != ""
}

// recordedCodexRolloutInAnotherForm is the restore guard's answer when no
// plain rollout named the recorded thread: the file that stands for it among
// the forms Codex still resumes — the newest, since a revert rollout is where
// the thread continued and a compressed rollout keeps its mtime — or why there
// is none. Two compressed copies of the thread's own rollout are the
// duplicated case the plain walk refuses; any number of revert rollouts are
// one thread.
//
// A plain rollout found now is the thread itself: Codex writes a compressed
// rollout back out as plain when it is resumed, and publishes the plain file
// before it removes the compressed one, so a thread caught between the two
// walks is found, not missing (#7134 review F-3).
func recordedCodexRolloutInAnotherForm(codexHome, id string) (string, resumeDecline) {
	plain, forms, err := codexRolloutForms(codexHome, id)
	switch {
	case err != nil:
		return "", declineResumeUnverified("af could not verify the recorded conversation against the Codex home this session uses")
	case len(plain) == 1:
		return plain[0], resumeDecline{}
	case len(plain) > 1:
		return "", declineResume(codexRolloutDuplicatedReason)
	}
	var newest string
	var newestAt time.Time
	threadRollouts := 0
	for _, path := range forms {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if m := codexRolloutFormRE.FindStringSubmatch(filepath.Base(path)); len(m) == 4 && m[2] == "" {
			threadRollouts++
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest, newestAt = path, info.ModTime()
		}
	}
	switch {
	case threadRollouts > 1:
		return "", declineResume(codexRolloutDuplicatedReason)
	case newest == "":
		return "", declineResume(codexRolloutMissingReason)
	}
	return newest, resumeDecline{}
}

// codexCarryOtherFormsReason is why a carry does not take a thread Codex holds
// only compressed or in revert rollouts: it is not missing, but the carry
// copies one plain rollout, and these forms need their siblings or a decoder.
// "" when the source holds none.
func codexCarryOtherFormsReason(srcHome, id string) string {
	_, forms, err := codexRolloutForms(srcHome, id)
	switch {
	case err != nil:
		return "af could not check whether its rollout is stored compressed or as a thread/revert rollout"
	case len(forms) > 0:
		return "its rollout is stored compressed or as a thread/revert rollout, which af does not carry yet"
	}
	return ""
}
