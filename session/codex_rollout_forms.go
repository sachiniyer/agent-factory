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

// codexRolloutFormRE names every file Codex keeps a thread under: group 1 is
// the thread id, group 2 the revert's rollout id ("" for the thread's own
// rollout), group 3 ".zst" when compressed. A plain thread rollout is the one
// codexRolloutFileRE already reads.
var codexRolloutFormRE = regexp.MustCompile(`(?i)^rollout-.+-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})` +
	`(?:_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}))?\.jsonl(\.zst)?$`)

const (
	codexRolloutMissingReason    = "the recorded conversation's rollout is missing from the Codex home this session uses"
	codexRolloutDuplicatedReason = "af could not confirm the recorded conversation is still the newest one in this worktree: two rollouts record it"
)

// codexRolloutOtherForms returns the files beneath codexHome's store that
// hold thread id in a form other than its plain rollout: compressed, a
// thread/revert rollout, or a compressed revert rollout. Like the plain walk
// it fails closed: a directory it cannot traverse is an error, never an
// empty-looking store (#5471).
func codexRolloutOtherForms(codexHome, id string) ([]string, error) {
	if strings.TrimSpace(codexHome) == "" {
		return nil, nil
	}
	var out []string
	err := filepath.WalkDir(filepath.Join(codexHome, "sessions"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d == nil || d.IsDir() {
			return nil
		}
		m := codexRolloutFormRE.FindStringSubmatch(d.Name())
		if len(m) != 4 || !strings.EqualFold(m[1], id) || (m[2] == "" && m[3] == "") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out, err
}

// codexRolloutCompressed reports whether path names a compressed rollout.
func codexRolloutCompressed(path string) bool {
	m := codexRolloutFormRE.FindStringSubmatch(filepath.Base(path))
	return len(m) == 4 && m[3] != ""
}

// recordedCodexRolloutInAnotherForm is the restore guard's answer when no
// plain rollout names the recorded thread: the file that stands for it among
// the forms Codex still resumes — the newest, since a revert rollout is where
// the thread continued and a compressed rollout keeps its mtime — or why there
// is none. Two compressed copies of the thread's own rollout are the
// duplicated case the plain walk refuses; any number of revert rollouts are
// one thread.
func recordedCodexRolloutInAnotherForm(codexHome, id string) (string, resumeDecline) {
	forms, err := codexRolloutOtherForms(codexHome, id)
	if err != nil {
		return "", declineResumeUnverified("af could not verify the recorded conversation against the Codex home this session uses")
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
// "" when the source holds none (or cannot be walked, which the carry's own
// enumeration reports).
func codexCarryOtherFormsReason(srcHome, id string) string {
	if forms, err := codexRolloutOtherForms(srcHome, id); err == nil && len(forms) > 0 {
		return "its rollout is stored compressed or as a thread/revert rollout, which af does not carry yet"
	}
	return ""
}
