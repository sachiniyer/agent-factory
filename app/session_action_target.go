package app

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// sessionActionTarget is the immutable identity captured when a retained TUI
// action begins. A title is display text and may be reused while a picker,
// confirmation, or daemon call is in flight; current records therefore resolve
// only by stable ID. CreatedAt keeps pre-ID records usable without letting a
// zero timestamp turn title reuse back into identity.
type sessionActionTarget struct {
	id        string
	title     string
	repoID    string
	createdAt time.Time
}

func captureSessionActionTarget(inst *session.Instance, repoID string) sessionActionTarget {
	return sessionActionTarget{
		id: inst.ID, title: inst.Title, repoID: repoID,
		createdAt: inst.CreatedAt,
	}
}

func (target sessionActionTarget) isZero() bool {
	return target.id == "" && target.title == "" && target.repoID == "" && target.createdAt.IsZero()
}

// sameIdentity compares the immutable part of two retained targets. A stable ID
// is authoritative; pre-ID records fall back to the same non-zero CreatedAt
// policy resolveSessionActionTarget uses.
func (target sessionActionTarget) sameIdentity(other sessionActionTarget) bool {
	if target.repoID != other.repoID {
		return false
	}
	if target.id != "" || other.id != "" {
		return target.id != "" && target.id == other.id
	}
	return target.title == other.title && !target.createdAt.IsZero() && target.createdAt.Equal(other.createdAt)
}

// resolveSessionActionTarget resolves target only inside the project that
// captured it. A non-empty ID is authoritative and never falls back to title.
// Legacy records use the same non-zero CreatedAt fallback as snapshot
// reconciliation.
func (m *home) resolveSessionActionTarget(target sessionActionTarget) *session.Instance {
	if target.repoID == "" || target.repoID != m.repoID {
		return nil
	}
	if target.id != "" {
		for _, inst := range m.store.GetInstances() {
			if inst.ID == target.id {
				return inst
			}
		}
		return nil
	}
	if target.createdAt.IsZero() {
		return nil
	}
	for _, inst := range m.store.GetInstances() {
		if inst.Title == target.title && inst.CreatedAt.Equal(target.createdAt) {
			return inst
		}
	}
	return nil
}

func (target sessionActionTarget) killRequest() daemon.KillSessionRequest {
	return daemon.KillSessionRequest{ID: target.id, Title: target.title, RepoID: target.repoID}
}

func (target sessionActionTarget) archiveRequest() daemon.ArchiveSessionRequest {
	return daemon.ArchiveSessionRequest{ID: target.id, Title: target.title, RepoID: target.repoID}
}

func (target sessionActionTarget) restoreRequest() daemon.RestoreSessionRequest {
	return daemon.RestoreSessionRequest{ID: target.id, Title: target.title, RepoID: target.repoID}
}

func (target sessionActionTarget) setPRInfoRequest(info session.PRInfoData) daemon.SetPRInfoRequest {
	return daemon.SetPRInfoRequest{ID: target.id, Title: target.title, RepoID: target.repoID, PRInfo: info}
}

// statusPollHolder identifies THIS TUI process as a holder of the daemon's
// pause lease (#3027). Per PROCESS, not per attach: a TUI attaches to one session
// full-screen at a time, so one id per process is enough to tell two TUIs apart,
// and it keeps a pause and its later resume matched without threading an id
// through the attach lifecycle. Random rather than the pid, because a pid is
// reused and a recycled one would let a new process release the lease a dead one
// took — the revocation this exists to prevent, with extra steps.
var statusPollHolder = "tui-" + randomStatusPollHolder()

func randomStatusPollHolder() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Never fatal: an empty holder is the legacy shared slot, which is exactly
		// the pre-#3027 behaviour rather than a broken one.
		return ""
	}
	return hex.EncodeToString(b[:])
}

func (target sessionActionTarget) pauseStatusPollRequest() daemon.PauseStatusPollRequest {
	return daemon.PauseStatusPollRequest{
		ID: target.id, Title: target.title, RepoID: target.repoID, Holder: statusPollHolder,
	}
}

func (target sessionActionTarget) resumeStatusPollRequest() daemon.ResumeStatusPollRequest {
	return daemon.ResumeStatusPollRequest{
		ID: target.id, Title: target.title, RepoID: target.repoID, Holder: statusPollHolder,
	}
}

func (target sessionActionTarget) handoffRequest(to string) daemon.HandoffSessionRequest {
	return daemon.HandoffSessionRequest{
		ID: target.id, Title: target.title, RepoID: target.repoID, To: to,
	}
}

func (target sessionActionTarget) resumeFromLimitRequest() daemon.ResumeFromLimitRequest {
	return daemon.ResumeFromLimitRequest{ID: target.id, Title: target.title, RepoID: target.repoID}
}

func (target sessionActionTarget) createTabRequest(kind session.TabKind) (daemon.CreateTabRequest, bool) {
	switch kind {
	case session.TabKindShell:
		return daemon.CreateTabRequest{
			ID: target.id, Title: target.title, RepoID: target.repoID, Shell: true,
		}, true
	case session.TabKindVSCode:
		return daemon.CreateTabRequest{
			ID: target.id, Title: target.title, RepoID: target.repoID, Kind: "vscode",
		}, true
	default:
		return daemon.CreateTabRequest{}, false
	}
}

func (target sessionActionTarget) closeTabRequest(tabID, tabName string) daemon.CloseTabRequest {
	return daemon.CloseTabRequest{
		ID: target.id, Title: target.title, RepoID: target.repoID,
		TabID: tabID, TabName: tabName,
	}
}
