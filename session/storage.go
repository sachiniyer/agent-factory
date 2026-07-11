package session

import (
	"encoding/json"
	"fmt"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"time"
)

// InstanceData represents the serializable data of an Instance
type InstanceData struct {
	// ID is the instance's stable identity (#1195), minted at NewInstance and
	// used as the reconcile identity key. omitempty + additive: records written
	// before #1195 simply have no id, and the reconcile falls back to
	// title+CreatedAt for them (rollforward, mirroring the BranchCreatedByUs
	// precedent).
	ID     string `json:"id,omitempty"`
	Title  string `json:"title"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	// Status is the legacy single-axis status int (#1195). Still written for one
	// release for rollback safety and read as the fallback source for records
	// that predate the `liveness` field. New code should read Liveness.
	Status Status `json:"status"`
	// Liveness is the daemon-owned health axis (#1195), the new canonical
	// persisted state. omitempty + additive: records written before #1195 have
	// no `liveness` key and decode to LivenessUnset, signaling FromInstanceData
	// to fall back to the legacy `status` int (rollforward).
	Liveness Liveness `json:"liveness,omitempty"`
	// InFlightOp is the transient operation axis (#1195/#1436) carried by the
	// daemon Snapshot so secondary TUIs can reconstruct non-round-trippable ops
	// exactly (OpArchiving vs OpKilling; OpRestoring vs plain Lost). It is scrubbed
	// at disk write/load boundaries: in-flight operations are process-local and
	// must not be resurrected after a daemon restart.
	InFlightOp InFlightOp `json:"in_flight_op,omitempty"`
	// LimitResetAt is the parsed usage-limit reset time (#1146), display-only:
	// written (and carried in the daemon snapshot to the read-only TUI) only for a
	// LiveLimitReached row so the sidebar [limit] badge can show "resets <t>" and
	// survive a restart, and so PR3's auto-resume scheduler can read it. omitempty
	// drops it for every normal session; additive + rollforward, mirroring the
	// Liveness precedent.
	LimitResetAt time.Time `json:"limit_reset_at,omitempty"`
	Height       int       `json:"height"`
	Width        int       `json:"width"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	AutoYes      bool      `json:"auto_yes"`
	Prompt       string    `json:"prompt,omitempty"`

	Program string `json:"program"`
	// UserKilled is the kill-intent tombstone (#1108): persisted by
	// Manager.KillSession before teardown begins. Present only in the crash
	// window between tombstone write and record deletion — a surviving
	// tombstoned record means "finish this kill", never "restore this".
	UserKilled bool      `json:"user_killed,omitempty"`
	TmuxName   string    `json:"tmux_name,omitempty"`
	Tabs       []TabData `json:"tabs,omitempty"`
	// AgentConversation mirrors the Agent tab's provider conversation id for
	// API/CLI consumers. The per-tab source of truth is TabData.Conversation.
	AgentConversation *AgentConversationData `json:"agent_conversation,omitempty"`
	Worktree          GitWorktreeData        `json:"worktree"`
	PRInfo            PRInfoData             `json:"pr_info,omitempty"`
	BackendType       string                 `json:"backend_type,omitempty"`
	RemoteMeta        map[string]interface{} `json:"remote_meta,omitempty"`
}

// IsRemoteHook reports whether this serialized record is a remote hook session,
// reading the persisted BackendType discriminator. It centralizes the raw-data
// remote check (#1592 Phase 1 PR3) so daemon logic that iterates []InstanceData
// — where no backend is reconstructed and Capabilities() is unavailable — never
// hard-codes the "remote" magic string. The load-time factory
// (NewInstanceFromData) remains the one place that maps the discriminator to a
// concrete backend.
func (d InstanceData) IsRemoteHook() bool {
	return d.BackendType == "remote"
}

// ForStorage returns data suitable for instances.json. InstanceData is also the
// daemon Snapshot payload, so it can carry transient in-flight operation state;
// disk persistence must not.
func (d InstanceData) ForStorage() InstanceData {
	lv := livenessFromData(d)
	d.Status = composeStatus(lv, OpNone)
	d.Liveness = lv
	d.InFlightOp = OpNone
	return d
}

// TabData is the serializable form of a session.Tab. The full list is persisted
// (and restored by exact TmuxName) so every tab — agent and shell alike —
// reconnects to its tmux session across an af/daemon restart (#930). The field
// is omitempty + additive, mirroring the BranchCreatedByUs back-compat
// precedent: instances.json written before #930 PR 2 simply has no Tabs, and
// FromInstanceData synthesizes [agent, shell] from the legacy TmuxName/Program.
type TabData struct {
	Name     string  `json:"name"`
	Kind     TabKind `json:"kind"`
	Command  string  `json:"command,omitempty"`
	TmuxName string  `json:"tmux_name,omitempty"`
	// Conversation is the provider-specific conversation id for this tab, when
	// the underlying agent exposes a durable resume id. Omitted for legacy rows
	// and providers where af can only resume "latest".
	Conversation *AgentConversationData `json:"conversation,omitempty"`
}

// PRInfoData represents the serializable data of a PRInfo
type PRInfoData struct {
	Number int    `json:"number,omitempty"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
}

// GitWorktreeData represents the serializable data of a GitWorktree.
//
// BranchCreatedByUs indicates whether the session created the underlying
// branch itself (vs. reused a pre-existing one). It is serialized via a
// pointer so that "missing" (nil, for data written before this field was
// added) can be distinguished from an explicit false. Missing values are
// treated as true to preserve the prior behavior for sessions that existed
// before this flag was introduced.
type GitWorktreeData struct {
	RepoPath          string `json:"repo_path"`
	WorktreePath      string `json:"worktree_path"`
	SessionName       string `json:"session_name"`
	BranchName        string `json:"branch_name"`
	BaseCommitSHA     string `json:"base_commit_sha"`
	ExternalWorktree  bool   `json:"external_worktree,omitempty"`
	BranchCreatedByUs *bool  `json:"branch_created_by_us,omitempty"`
}

// Storage handles saving and loading instances using the state interface.
// When repoID is set (TUI mode), operations are scoped to that repo.
// When repoID is empty (daemon mode), operations span all repos.
type Storage struct {
	state  config.InstanceStorage
	repoID string
}

// NewStorage creates a new storage instance.
// Pass a non-empty repoID for TUI (repo-scoped) mode, or "" for daemon (all-repo) mode.
func NewStorage(state config.InstanceStorage, repoID string) (*Storage, error) {
	return &Storage{
		state:  state,
		repoID: repoID,
	}, nil
}

// dedupeInstanceData collapses records that share a title, keeping the one
// with the newest UpdatedAt (ties keep the earliest occurrence, so in-memory
// records — which both save paths place ahead of disk-only records — win).
// Titles are unique per repo (the daemon's findTitleConflictLocked enforces
// this on create), so two same-title records in one repo's list are always
// the same logical session written twice (#808). Deduping at the save/load
// chokepoints prevents new duplicates from persisting and collapses any
// existing on-disk duplicate on the next clean save.
func dedupeInstanceData(data []InstanceData) []InstanceData {
	if len(data) < 2 {
		return data
	}
	index := make(map[string]int, len(data))
	out := make([]InstanceData, 0, len(data))
	for _, d := range data {
		if i, ok := index[d.Title]; ok {
			if d.UpdatedAt.After(out[i].UpdatedAt) {
				out[i] = d
			}
			continue
		}
		index[d.Title] = len(out)
		out = append(out, d)
	}
	return out
}

// SaveInstances persists the daemon's authoritative in-memory instances to
// disk, grouped by repo. As of #960 PR 4 the daemon is the SOLE writer of
// instances.json, so this is a straight marshal of the manager's per-repo
// state, NOT a merge: there is no competing full-list writer to reconcile
// against, so the old mergeInstancesWithDisk rule-zoo
// (#551/#766/#808/#819/#844/#959) is gone. With one writer a clobber is
// impossible by construction.
//
// Only repos with at least one persistable in-memory instance are rewritten;
// repos the daemon holds nothing for are left untouched — their records were
// already removed by the targeted DeleteInstance on kill, or were never loaded.
// Loading/Deleting/non-started instances are skipped: their worktree is not yet
// populated (Loading) or is mid-teardown (Deleting), so FromInstanceData cannot
// restore them.
//
// The targeted writers (appendInstanceData / persistInstanceData /
// DeleteInstance) keep the disk current on every mutation; this full save is the
// shutdown checkpoint. Records are deduped by title (#808) before marshaling.
// Because the manager's memory is the source of truth, the save deliberately
// does NOT read disk first: the file is overwritten with authoritative state, so
// a corrupt or momentarily-stale file on disk is simply replaced, not merged.
func (s *Storage) SaveInstances(instances []*Instance) error {
	// Group persistable in-memory instances by repo root. Prefer the worktree's
	// resolved repo path so we share a repo ID with the TUI even for a session
	// created from a symlinked path; fall back to Path for remote backends where
	// Worktree.RepoPath is empty. This mirrors CollectRepoRoots (#667).
	grouped := make(map[string][]InstanceData)
	for _, inst := range instances {
		status := inst.GetStatus()
		if status == Loading || status == Deleting {
			continue
		}
		// The !Started() skip drops transient never-started junk (a create that
		// hasn't run Start, a discarded duplicate). It must NOT drop an Archived
		// instance (#1028): archived sessions load deliberately inert
		// (started=false — tmux torn down, worktree relocated), yet the record is
		// the ONLY pointer to the relocated worktree. Dropping it on a wholesale
		// per-repo checkpoint save — triggered whenever ANY started instance in
		// the same repo is saved — would silently orphan the archived worktree.
		// (Lost is unaffected: it loads started=true, so it already survives.)
		if !inst.Started() && status != Archived {
			continue
		}
		root := inst.GetRepoPath()
		if root == "" {
			root = inst.Path
		}
		rid := config.RepoIDFromRoot(root)
		grouped[rid] = append(grouped[rid], inst.ToInstanceData().ForStorage())
	}

	for rid, group := range grouped {
		path, pathErr := config.RepoInstancesPath(rid)
		if pathErr != nil {
			return pathErr
		}
		if err := config.WithFileLock(path, func() error {
			jsonData, err := json.Marshal(dedupeInstanceData(group))
			if err != nil {
				return fmt.Errorf("failed to marshal instances for repo %s: %w", rid, err)
			}
			return s.state.SaveInstances(rid, jsonData)
		}); err != nil {
			return err
		}
	}

	return nil
}

// LoadInstances loads the list of instances from disk.
func (s *Storage) LoadInstances() ([]*Instance, error) {
	var allJSON map[string]json.RawMessage
	if s.repoID != "" {
		// TUI mode: load just this repo. Surface read errors so startup can
		// report "couldn't read your sessions" instead of silently showing
		// an empty list that looks like a fresh install (#766).
		raw, err := s.state.GetInstances(s.repoID)
		if err != nil {
			return nil, err
		}
		allJSON = map[string]json.RawMessage{s.repoID: raw}
	} else {
		// Daemon mode: load all repos. Surface a directory-level read error so
		// the daemon reports "couldn't read your sessions" instead of silently
		// presenting an empty list that looks like a fresh install while live
		// sessions sit unreadable on disk (#868).
		all, err := s.state.GetAllInstances()
		if err != nil {
			return nil, err
		}
		allJSON = all
	}

	var instances []*Instance
	for _, jsonData := range allJSON {
		if jsonData == nil || string(jsonData) == "[]" || string(jsonData) == "null" {
			continue
		}
		var instancesData []InstanceData
		if err := json.Unmarshal(jsonData, &instancesData); err != nil {
			return nil, fmt.Errorf("failed to unmarshal instances: %w", err)
		}
		// Collapse duplicate records written before the dedup-on-save fix
		// (#808) so a dup-containing file yields one sidebar row per session
		// immediately, not just after the next save rewrites the file.
		instancesData = dedupeInstanceData(instancesData)
		for _, data := range instancesData {
			data = data.ForStorage()
			instance, err := FromInstanceData(data)
			if err != nil {
				// Instance's tmux session or worktree may have been
				// destroyed externally. Log and skip rather than
				// failing the entire load.
				log.WarningLog.Printf("skipping instance %q: %v", data.Title, err)
				continue
			}
			instances = append(instances, instance)
		}
	}

	return instances, nil
}

// DeleteInstance removes an instance from storage by filtering raw JSON
// directly, avoiding the need to reconstruct live Instance objects (which
// may fail if tmux/worktree has already been destroyed).
func (s *Storage) DeleteInstance(title string) error {
	deleted, err := s.DeleteInstanceByStableID(title, "")
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("instance not found: %s", title)
	}
	return nil
}

// DeleteInstanceByStableID removes an instance from storage only when the
// record still matches the stable session identity captured by the caller. A
// false nil result means a same-titled record exists but belongs to a different
// instance, so the caller must treat the delete as stale and leave it alone.
// Empty IDs are legacy-compatible and fall back to title matching.
func (s *Storage) DeleteInstanceByStableID(title, id string) (bool, error) {
	path, pathErr := config.RepoInstancesPath(s.repoID)
	if pathErr != nil {
		return false, pathErr
	}
	deleted := false
	sameTitleDifferentID := false
	if err := config.WithFileLock(path, func() error {
		raw, err := s.state.GetInstances(s.repoID)
		if err != nil {
			return err
		}
		if raw == nil || string(raw) == "[]" || string(raw) == "null" {
			return fmt.Errorf("instance not found: %s", title)
		}

		var data []InstanceData
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("failed to parse instances: %w", err)
		}

		filtered := make([]InstanceData, 0, len(data))
		found := false
		for _, d := range data {
			if d.Title == title {
				if stableIDMatches(d.ID, id) {
					found = true
					deleted = true
					continue
				}
				sameTitleDifferentID = true
			}
			filtered = append(filtered, d)
		}

		if !found {
			if sameTitleDifferentID {
				return nil
			}
			return fmt.Errorf("instance not found: %s", title)
		}

		out, err := json.Marshal(filtered)
		if err != nil {
			return fmt.Errorf("failed to marshal instances: %w", err)
		}
		return s.state.SaveInstances(s.repoID, out)
	}); err != nil {
		return false, err
	}
	return deleted, nil
}

func stableIDMatches(recordID, expectedID string) bool {
	return expectedID == "" || recordID == "" || recordID == expectedID
}

// LoadInstanceData reads and unmarshals instance data from disk without
// constructing live Instance objects (no tmux session restoration).
// Used for lightweight comparison against in-memory state.
func (s *Storage) LoadInstanceData() ([]InstanceData, error) {
	raw, err := s.state.GetInstances(s.repoID)
	if err != nil {
		return nil, err
	}
	if raw == nil || string(raw) == "[]" || string(raw) == "null" {
		return nil, nil
	}
	var data []InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("failed to unmarshal instances: %w", err)
	}
	return dedupeInstanceData(data), nil
}

// DeleteAllInstances removes all stored instances
func (s *Storage) DeleteAllInstances() error {
	return s.state.DeleteAllInstances()
}

// CollectRepoRoots returns the set of unique repo root paths referenced by
// stored instances across all repos. This is used by operations whose scope
// must span every repo with persisted state (e.g. `af reset` cleaning
// worktrees in all repos before deleting global instance storage).
//
// Instances without a usable repo path (e.g. certain remote backends where
// Worktree.RepoPath is empty and Path is not a local filesystem path) are
// skipped. Callers should treat the result as best-effort.
func (s *Storage) CollectRepoRoots() (map[string]struct{}, error) {
	roots := make(map[string]struct{})
	// A directory-level read failure (permission denied, I/O error) is
	// surfaced so `af reset` aborts with a clear message instead of treating
	// an unreadable instances directory as "no sessions" and silently
	// skipping worktree cleanup for every repo (#868). A missing directory
	// (first run) still comes back as an empty map with a nil error.
	allJSON, err := s.state.GetAllInstances()
	if err != nil {
		return nil, fmt.Errorf("failed to read stored instances: %w", err)
	}
	for repoID, jsonData := range allJSON {
		if jsonData == nil || string(jsonData) == "[]" || string(jsonData) == "null" {
			continue
		}
		var instancesData []InstanceData
		if err := json.Unmarshal(jsonData, &instancesData); err != nil {
			// One repo's corrupted instances.json must not abort the whole
			// reset: skip-and-warn (naming the repo) and continue collecting
			// roots for the others, matching the daemon's per-repo recovery.
			// Reset is exactly when corruption recovery is needed (#869).
			log.WarningLog.Printf("skipping repo %s: corrupted instances.json: %v", repoID, err)
			continue
		}
		for _, data := range instancesData {
			// Prefer the worktree's repo path; fall back to the
			// instance path (the repo the instance was created in).
			root := data.Worktree.RepoPath
			if root == "" {
				root = data.Path
			}
			if root == "" {
				continue
			}
			roots[root] = struct{}{}
		}
	}
	return roots, nil
}
