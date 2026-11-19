package daemon

import (
	"errors"
	"fmt"
)

// This file is the project-delete fence a session create samples and a project
// delete stamps: the transition counter, the per-repo last-seen sample, and the
// single refusal every fence arm returns. Split from manager_create.go (#1145
// file-length lint) along that concern — the fence's consumers live in
// deleteproject.go, restore.go, rootagent_carry.go, prune_apply.go, and
// archive_restore.go, not in create alone.

// stampProjectDeleteLocked records that this repo's delete fence just changed
// state. The caller MUST already hold m.mu — the stamp has to be atomic with the
// fence mutation it describes, or a create could observe one without the other.
func (m *Manager) stampProjectDeleteLocked(repoID string) {
	if m.projectDeleteLastSeq == nil {
		m.projectDeleteLastSeq = make(map[string]uint64)
	}
	m.projectDeleteSeq++
	m.projectDeleteLastSeq[repoID] = m.projectDeleteSeq
}

// projectDeleteSeqNow samples the fence-transition counter for a caller that
// does not hold m.mu.
//
// It takes NO repo id, which is the entire point: reserveCreate must sample
// before config.RepoFromPath, and that call is what produces the id (#2947).
func (m *Manager) projectDeleteSeqNow() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.projectDeleteSeq
}

// projectDeleteMovedLocked reports whether this repo's fence has changed state
// since the given sample. The caller MUST already hold m.mu.
//
// Strictly greater, deliberately. A delete whose last transition is EQUAL to the
// sample completed before the sample was taken, which is ordinary history rather
// than a race — treating it as one would strand every project that is ever
// deleted and re-created.
func (m *Manager) projectDeleteMovedLocked(repoID string, since uint64) bool {
	return m.projectDeleteLastSeq[repoID] > since
}

// projectDeleteRefusal builds the refusal every delete-fence arm returns, so the
// three of them cannot drift in wording or in what they promise the caller.
//
// inProgress distinguishes a delete still running from one that finished while
// this create was outside m.mu; the guidance differs, and a create told to
// "retry after deletion finishes" about a delete that already finished would be
// telling the user to wait for nothing.
func projectDeleteRefusal(repoID string, inProgress bool) error {
	err := fmt.Errorf("project %s is being deleted; retry the session create after deletion finishes", repoID)
	if !inProgress {
		err = fmt.Errorf("project %s was being deleted while this session create resolved its backend; nothing was created — retry if the project still exists", repoID)
	}
	return projectDeleteFenceError{err}
}

// errProjectDeleteFenced is what every projectDeleteRefusal Is, wording
// unchanged, so a caller can tell a delete's fence from a failed create — the
// root heal must not answer it with a fresh agent (PR #7190 review F3).
var errProjectDeleteFenced = errors.New("refused by a project delete's fence")

type projectDeleteFenceError struct{ error }

func (e projectDeleteFenceError) Is(target error) bool { return target == errProjectDeleteFenced }

// projectDeleteStateFor answers, in ONE acquisition, whether this repo has a
// delete running now or has had one transition since the given sample. Both
// readings must come from the same acquisition: taken separately they could
// describe different instants and a delete could slip between them.
func (m *Manager) projectDeleteStateFor(repoID string, since uint64) (active, moved bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, active = m.projectDeletes[repoID]
	return active, m.projectDeleteMovedLocked(repoID, since)
}
