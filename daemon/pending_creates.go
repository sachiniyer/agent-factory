package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// pendingCreateDirName names the per-home journal of in-flight create attempts.
const pendingCreateDirName = "pending-creates"

// pendingCreateMarker is the durable record of ONE in-flight create attempt:
// the instance id af minted for it plus the tmux session name and program the
// attempt will spawn with. The create writes it BEFORE session.NewInstance —
// and therefore before `tmux new-session` — and clears it once the outcome is
// settled: a committed instance row takes over as the durable record, or a
// conclusively-answered teardown leaves nothing to prove. A return that may
// leave a live, unrecorded session behind deliberately does NOT clear, and a
// process death (SIGKILL, or a panic that skips the clears) can never clear —
// so a surviving marker is exactly the set of attempts a crash could have
// stranded between `new-session` and the record write (#6301).
//
// The marker is what makes that stranded session PROVABLE later: the leftover
// carries the minted id in its environment (AF_INSTANCE_ID is stamped at
// new-session since #6260), and the journal is the only evidence the id was
// af's for a create that never committed. Without the journal the id on an
// unrecorded session is unsourced — it proves nothing, and the session is left
// for af doctor.
type pendingCreateMarker struct {
	InstanceID  string `json:"instance_id"`
	RepoID      string `json:"repo_id"`
	Title       string `json:"title"`
	SessionName string `json:"session_name"`
	Program     string `json:"program"`
	MintedAt    string `json:"minted_at"`
}

func pendingCreateDir() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pendingCreateDirName), nil
}

// pendingCreateIDPattern restricts a marker's filename stem to the characters
// an instance id mints (an RFC-4122 UUID), so a corrupted or tampered marker
// can never name a path outside the journal directory.
var pendingCreateIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func pendingCreateMarkerPath(dir, instanceID string) (string, bool) {
	if !pendingCreateIDPattern.MatchString(instanceID) {
		return "", false
	}
	return filepath.Join(dir, instanceID+".json"), true
}

// notePendingCreate journals one in-flight create attempt. The write is a
// rename off a temp file so a crash mid-write can never leave a half-formed
// marker for the sweep to misread.
func notePendingCreate(marker pendingCreateMarker) error {
	dir, err := pendingCreateDir()
	if err != nil {
		return err
	}
	if err := config.MkdirAllUnderAFHome(dir, 0o755); err != nil {
		return err
	}
	payload, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".journal-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	path, ok := pendingCreateMarkerPath(dir, marker.InstanceID)
	if !ok {
		return fmt.Errorf("pending create journal: invalid instance id %q", marker.InstanceID)
	}
	return os.Rename(tmpName, path)
}

// clearPendingCreate drops an attempt's journal entry. Missing is fine — the
// entry may already have been consumed by an earlier clear or by the sweep.
func clearPendingCreate(instanceID string) {
	dir, err := pendingCreateDir()
	if err != nil {
		return
	}
	path, ok := pendingCreateMarkerPath(dir, instanceID)
	if !ok {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.WarningLog.Printf("pending create journal: cannot clear %s: %v", instanceID, err)
	}
}

// readPendingCreateMarkers returns every parseable journal entry. A marker
// that cannot be read or is missing its proof fields is skipped with a
// warning: unreadable evidence must never author a kill, and it must never be
// deleted either — the session it may name is left for af doctor.
func readPendingCreateMarkers() ([]pendingCreateMarker, error) {
	dir, err := pendingCreateDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var markers []pendingCreateMarker
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			log.WarningLog.Printf("pending create journal: cannot read %s: %v", entry.Name(), err)
			continue
		}
		var marker pendingCreateMarker
		if err := json.Unmarshal(payload, &marker); err != nil || marker.InstanceID == "" ||
			marker.RepoID == "" || marker.SessionName == "" ||
			!pendingCreateIDPattern.MatchString(marker.InstanceID) {
			log.WarningLog.Printf("pending create journal: skipping unreadable marker %s", entry.Name())
			continue
		}
		markers = append(markers, marker)
	}
	return markers, nil
}

// sweepUnrecordedCreateSessions reaps tmux sessions a previous daemon
// generation spawned but never recorded — the #6301 crash window between
// `tmux new-session` and the instance-record write. It is the generic-create
// twin of the task-fire reconcile (#6260) and shares its rule: a session may
// be killed only on proof, never by name.
//
// The chain for one marker is: the recorded session name is live, no committed
// instance row owns the journaled id, and the live session's environment still
// carries that id — re-verified atomically against the exact tmux incarnation
// inside ReapProvenIncarnation, so a same-name replacement can never be killed
// in the leftover's place. Every doubtful outcome — an unreadable journal or
// record, a missing or foreign stamp, a failed re-verification — leaves both
// the marker and the session for af doctor to report.
//
// It runs while the daemon is still starting, before the readiness barrier, so
// no create of this generation can interleave: every stamped live session a
// marker could name belongs to a previous one.
func sweepUnrecordedCreateSessions() {
	markers, err := readPendingCreateMarkers()
	if err != nil {
		log.WarningLog.Printf("unrecorded create sweep: cannot read the pending-create journal: %v", err)
		return
	}
	if len(markers) == 0 {
		return
	}
	home, err := config.GetConfigDir()
	if err != nil {
		log.WarningLog.Printf("unrecorded create sweep: cannot resolve the AF home: %v", err)
		return
	}
	exec := cmd.MakeExecutor()
	names, err := tmux.ListSessionNames(exec)
	if err != nil {
		log.WarningLog.Printf("unrecorded create sweep: cannot list tmux sessions; leaving %d journaled attempt(s) unverified: %v", len(markers), err)
		return
	}
	for _, marker := range markers {
		sweepOneUnrecordedCreate(exec, home, names, marker)
	}
}

// sweepOneUnrecordedCreate settles one journaled attempt against the live
// session listing. "Settled" means either the leftover is reaped by proof and
// the marker is consumed, or the marker is stale — nothing live can still
// carry the journaled id — and is cleared without touching any session.
func sweepOneUnrecordedCreate(exec cmd.Executor, home string, names []string, marker pendingCreateMarker) {
	if !slices.Contains(names, marker.SessionName) {
		// Nothing answers under the recorded name, so nothing live carries the
		// journaled id: the create died before its spawn finished, or the
		// leftover is already gone.
		clearPendingCreate(marker.InstanceID)
		return
	}
	rows, err := loadRepoInstanceData(marker.RepoID)
	if err != nil {
		// "Unrecorded" cannot be proven while the committed record cannot be
		// read — leave the session and the marker.
		log.WarningLog.Printf("unrecorded create sweep: cannot check %q against repo %s records: %v",
			marker.SessionName, marker.RepoID, err)
		return
	}
	if slices.ContainsFunc(rows, func(row session.InstanceData) bool {
		return row.ID == marker.InstanceID ||
			taskFireRowOwnsTmuxName(&row, marker.SessionName)
	}) {
		// The session is recorded, either because this attempt committed after
		// all (a row owns the journaled id) or because a row claims the tmux
		// name under another identity — e.g. a fire-replay quarantine row with
		// a fresh id whose StartupStateUnknown preserves the session for
		// inspection. Either way the session is not unrecorded: clear the
		// marker and never reap.
		clearPendingCreate(marker.InstanceID)
		return
	}
	stamped, present, err := tmux.SessionEnvVar(exec, marker.SessionName, tmux.EnvMarkerInstance)
	if err != nil {
		log.WarningLog.Printf("unrecorded create sweep: %q environment is unreadable; leaving it for af doctor: %v",
			marker.SessionName, err)
		return
	}
	if !present || stamped != marker.InstanceID {
		// Whatever answers under the recorded name is not this attempt's
		// spawn: the leftover is conclusively gone, the marker is stale, and
		// the live session was never ours to begin with.
		clearPendingCreate(marker.InstanceID)
		return
	}
	reap := tmux.ReapProvenIncarnation(exec, marker.Program, tmux.IncarnationProof{
		SessionName: marker.SessionName,
		Home:        home,
		InstanceID:  marker.InstanceID,
	})
	if !reap.Stopped {
		log.WarningLog.Printf("unrecorded create sweep: %q carries journaled id %s but was not reaped (%s); leaving it for af doctor",
			marker.SessionName, marker.InstanceID, reap.Why)
		return
	}
	log.InfoLog.Printf("unrecorded create sweep: reaped %q (create %s committed no record)",
		marker.SessionName, marker.InstanceID)
	clearPendingCreate(marker.InstanceID)
}
