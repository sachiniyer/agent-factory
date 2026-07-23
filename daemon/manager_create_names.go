// Title admission for session creation: what names a create may claim, and the
// refusals that decide it. Split out of manager_create.go, which holds the
// creation FLOW; this file holds the naming RULES that flow consults.

package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// runtimeNameNamespace is the extra (non-title) name a backend claims during
// create admission. The enum makes the alternatives exclusive: a create cannot
// accidentally be treated as both a local tmux runtime and a remote hook, which
// two independent booleans would permit.
type runtimeNameNamespace uint8

const (
	runtimeNamespaceSandbox runtimeNameNamespace = iota
	runtimeNamespaceLocalTmux
	runtimeNamespaceRemoteHook
)

func runtimeNamespaceForKind(kind session.BackendKind) runtimeNameNamespace {
	switch kind {
	case session.BackendLocal:
		return runtimeNamespaceLocalTmux
	case session.BackendHook:
		return runtimeNamespaceRemoteHook
	default:
		return runtimeNamespaceSandbox
	}
}

type titleConflictKind int

const (
	titleConflictNone titleConflictKind = iota
	titleConflictReserved
	titleConflictLive
	titleConflictDisk
)

type titleNamespace int

const (
	titleNamespaceNone titleNamespace = iota
	titleNamespaceBranch
	titleNamespaceTmux
)

// titleCollisionNamespace asks every name encoder a pair of local sessions will
// actually claim. Git and tmux intentionally have different grammars, so either
// one may collide while the other does not. The tmux half is enabled only when
// both records use the host-local runtime.
func (m *Manager) titleCollisionNamespace(repoPath, a, b string, bothUseLocalTmux bool) titleNamespace {
	if m.titlesCollide(a, b) {
		return titleNamespaceBranch
	}
	if bothUseLocalTmux && tmux.SanitizedNameForRepo(a, repoPath) == tmux.SanitizedNameForRepo(b, repoPath) {
		return titleNamespaceTmux
	}
	return titleNamespaceNone
}

// findTitleConflictLocked returns the existing title that conflicts with the
// given candidate, along with the source and namespace of the conflict. An empty
// result means the title is available. Local sessions claim both a git branch and
// a tmux session name; admission rejects either collision before creating a
// worktree or launching a runtime.
func (m *Manager) findTitleConflictLocked(repoID, repoPath, title string, localTmux bool, diskData []session.InstanceData) (string, titleConflictKind, titleNamespace) {
	for key := range m.reservedTitles {
		rid, existing := splitDaemonInstanceKey(key)
		if rid == repoID && m.titlesCollide(existing, title) {
			return existing, titleConflictReserved, titleNamespaceBranch
		}
	}
	if localTmux {
		nameKey := daemonInstanceKey(repoID, tmux.SanitizedNameForRepo(title, repoPath))
		if existing, reserved := m.reservedTmuxNames[nameKey]; reserved {
			return existing, titleConflictReserved, titleNamespaceTmux
		}
	}
	for key, inst := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil {
			continue
		}
		bothUseLocalTmux := localTmux && inst.Capabilities().Workspace == session.WorkspaceLocalWorktree
		if namespace := m.titleCollisionNamespace(repoPath, inst.Title, title, bothUseLocalTmux); namespace != titleNamespaceNone {
			return inst.Title, titleConflictLive, namespace
		}
	}
	for _, data := range diskData {
		bothUseLocalTmux := localTmux && data.UsesLocalTmux()
		namespace := m.titleCollisionNamespace(repoPath, data.Title, title, bothUseLocalTmux)
		if namespace == titleNamespaceNone {
			continue
		}
		// Loading entries are transient TUI state with an empty worktree
		// path and cannot be restored. Older TUI binaries (#551) could
		// persist them to disk on quit, where they would block title
		// reuse forever. Treat them as ghosts that the next save will
		// reap rather than as live reservations.
		if data.Status == session.Loading {
			continue
		}
		return data.Title, titleConflictDisk, namespace
	}
	return "", titleConflictNone, titleNamespaceNone
}

// titlesCollide reports whether two session titles cannot coexist in the same
// repo because they would derive the same git branch. It delegates to the shared
// git.TitlesCollide helper so the daemon's authoritative validation and the
// TUI's naming pre-check stay in lockstep (#936).
func (m *Manager) titlesCollide(a, b string) bool {
	return git.TitlesCollide(a, b, m.cfg.BranchPrefix)
}

// validateTitleAvailableLocked refuses a title the create cannot have. It is
// composed of three groups, kept in this order because their error precedence is
// observable: the title's own SHAPE, then a collision with an af record, then the
// EXTERNAL namespaces (hook slugs, a live tmux session) the title would claim.
//
// The shape and namespace halves are separately callable as
// validateTitleClaimableLocked, which is what lets reserveCreate run every
// record-independent refusal BEFORE the archived-name-reuse rename mutates
// anything (#2415). Any new check belongs in one of the two halves rather than
// inline here, so the pre-rename path picks it up automatically — a check added
// only to this function is exactly how #2415 happened.
func (m *Manager) validateTitleAvailableLocked(repoID, repoPath, title, program string, namespace runtimeNameNamespace, allowReserved bool, diskData []session.InstanceData) error {
	if err := m.validateTitleShapeLocked(title, allowReserved); err != nil {
		return err
	}
	if err := m.findTitleRecordConflictLocked(repoID, repoPath, title, namespace, diskData); err != nil {
		return err
	}
	return m.validateTitleNamespacesLocked(repoID, repoPath, title, program, namespace, diskData, nil)
}

// validateTitleClaimableLocked is every refusal that does NOT depend on af's own
// record for this title — the ones an archived-name-reuse rename cannot clear, so
// a create that trips one is doomed no matter what the rename does.
//
// ignore is the archived instance the caller is about to rename out of the way.
// It is excluded from the hook-slug scans, which would otherwise report the very
// row being freed as the collision and refuse a reuse that would have succeeded.
// It is not excluded from the tmux probe: archiving kills the session's pane, so
// an archived row never owns a live tmux name, and anything the probe finds is a
// genuine orphan the rename has no effect on.
func (m *Manager) validateTitleClaimableLocked(repoID, repoPath, title, program string, namespace runtimeNameNamespace, allowReserved bool, diskData []session.InstanceData, ignore *session.Instance) error {
	if err := m.validateTitleShapeLocked(title, allowReserved); err != nil {
		return err
	}
	return m.validateTitleNamespacesLocked(repoID, repoPath, title, program, namespace, diskData, ignore)
}

// validateTitleShapeLocked rejects titles that are malformed or reserved,
// independent of any existing session.
func (m *Manager) validateTitleShapeLocked(title string, allowReserved bool) error {
	// Whitespace-only titles (e.g. "   ") are non-empty and so slip past a bare
	// == "" check, creating sessions with effectively blank names (#973). Trim
	// before the emptiness gate; the TUI naming flow applies the same check.
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("session title is required")
	}
	// The "root" title belongs to the daemon-managed root agent (#1106).
	// Every creation path lands here — TUI, `af sessions create`, task
	// spawns, DeliverPrompt auto-creates — so this single gate reserves the
	// name everywhere. Only the daemon's own ensure loop passes
	// allowReserved; title-base derivation (nextAvailableTitleLocked) never
	// does, so a base of "root" skips to "root-2" instead of erroring.
	if !allowReserved && session.IsReservedTitle(title) {
		return fmt.Errorf("session title %q is reserved for the daemon-managed root agent; pick another name (to run a root agent on this repo, add it to root_agents in ~/.agent-factory/config.json)", title)
	}
	return nil
}

// findTitleRecordConflictLocked reports a collision with an existing af record —
// a reservation, a loaded instance, or a durable row. This is the ONE group the
// archived-name-reuse rename clears, which is why it is not part of
// validateTitleClaimableLocked.
func (m *Manager) findTitleRecordConflictLocked(repoID, repoPath, title string, namespace runtimeNameNamespace, diskData []session.InstanceData) error {
	// Titles are sanitized into git branch names (git.SanitizeBranchName
	// lowercases, turns spaces into dashes, strips unsafe chars, and collapses
	// dashes), so distinct titles can map to the same branch: "MyApp"/"myapp"
	// (#605) or "A B"/"a-b" (#741) both collide. The second worktree create
	// would otherwise fail with a cryptic git error, so reject the conflict
	// here, before any worktree or tmux setup runs.
	if existing, kind, collisionNamespace := m.findTitleConflictLocked(repoID, repoPath, title, namespace == runtimeNamespaceLocalTmux, diskData); existing != "" {
		switch {
		case existing == title:
			if kind == titleConflictReserved {
				return fmt.Errorf("session with title %q is already reserved: %w", title, errConcurrentCreate)
			}
			return fmt.Errorf("session with title %q already exists: %w", title, errConcurrentCreate)
		case collisionNamespace == titleNamespaceTmux:
			return fmt.Errorf("session titled %q conflicts with existing session %q: both map to tmux session %q", title, existing, tmux.SanitizedNameForRepo(title, repoPath))
		default:
			return fmt.Errorf("session titled %q conflicts with existing session %q: both sanitize to the same git branch %q", title, existing, m.branchForTitle(title))
		}
	}
	return nil
}

// validateTitleNamespacesLocked refuses a title whose EXTERNAL name claims are
// already taken: the global hook-slug namespace external provisioners key on, and
// a live tmux session of the same name. See validateTitleClaimableLocked for what
// ignore excludes and why.
func (m *Manager) validateTitleNamespacesLocked(repoID, repoPath, title, program string, namespace runtimeNameNamespace, diskData []session.InstanceData, ignore *session.Instance) error {
	if namespace == runtimeNamespaceRemoteHook {
		candidate := session.Slugify(title)
		var ignoreTitle string
		if ignore != nil {
			ignoreTitle = ignore.Title
		}
		// Hook names are the ONE namespace that stays global while titles go
		// per-repo: launch_cmd/delete_cmd receive `--name <slug>` verbatim, with
		// no repo component, and external provisioners tag and reap real
		// VMs/containers by it. Two repos handing scripts the same name would
		// clobber one sandbox and let either delete reap the other's. So every
		// check below spans ALL repos, unlike the per-repo title rules above.
		if _, ok := m.reservedRemoteNames[candidate]; ok {
			return fmt.Errorf("remote hook name %q is already reserved", candidate)
		}
		// Guard against in-memory remote sessions that are not (yet) on disk.
		// refreshDaemonInstances preserves a running remote instance in
		// m.instances even after its repo directory is deleted externally (a
		// recoverable inconsistency), yet loadRepoInstanceData returns nothing
		// for it — so a disk-only slug check would let a second title that
		// slugifies to the same hook name through. The branch-collision check
		// above misses this pair because Slugify drops underscores while branch
		// sanitization keeps them as dashes ("My_App"->branch "my-app"/slug
		// "myapp" vs "MyApp"->branch "myapp"/slug "myapp"). The TUI pre-check
		// (FindSlugCollision over Snapshot()) catches it, but the HTTP
		// CreateSession path bypasses that, so the daemon-side check must be
		// complete (#1636).
		for _, inst := range m.instances {
			// Pointer identity, not title: this scan spans every repo, and two
			// repos may legitimately hold the same title. Ignoring by name would
			// suppress a real cross-repo hook collision.
			if inst == nil || inst == ignore {
				continue
			}
			data := inst.ToInstanceData()
			if !data.IsRemoteHook() {
				continue
			}
			if session.Slugify(data.Title) == candidate {
				return fmt.Errorf("remote session titled %q already maps to hook name %q", data.Title, candidate)
			}
		}
		for _, data := range diskData {
			// diskData is this repo's rows only, where a title is unique, so the
			// name identifies the row being renamed unambiguously.
			if ignoreTitle != "" && data.Title == ignoreTitle {
				continue
			}
			if !data.IsRemoteHook() {
				continue
			}
			if session.Slugify(data.Title) == candidate {
				return fmt.Errorf("remote session titled %q already maps to hook name %q", data.Title, candidate)
			}
		}
		// diskData holds only THIS repo's rows, so a settled hook session in
		// another repo would otherwise slip through — the hole that let two repos
		// create the same hook name sequentially (they just could not race).
		owner, ownerRepo, err := hookSlugOwnerInOtherRepos(candidate, repoID)
		if err != nil {
			return err
		}
		if owner != "" {
			return fmt.Errorf("remote session titled %q in project %s already maps to hook name %q; remote hook names are shared across projects because the hook scripts receive them verbatim as --name — pick another title for this remote session", owner, ownerRepo, candidate)
		}
		return nil
	}
	if namespace != runtimeNamespaceLocalTmux {
		return nil
	}
	tmuxSession := tmux.NewTmuxSessionForRepo(title, repoPath, program)
	// Existence gates the create here, so read the tri-state, not the lossy bool
	// (#1962): only a CONFIRMED existing session (known && exists) blocks. A
	// wedged/timed-out has-session is NOT proof of a name collision — reporting
	// "exists" through ExistsOrUnknown would refuse a legitimate create against a
	// merely-wedged server. An unanswered probe (!known) falls through and lets the
	// create proceed. That never silently clobbers a real orphan: the create's own
	// `tmux new-session -s <name>` fails on a duplicate name, and Start's existence
	// check (now ProbeSession too) surfaces it as "already exists" once the server
	// answers, or as ErrTmuxTimeout while it stays wedged — either way non-
	// destructive. Blocking here on a guess is the only unsafe option.
	if exists, known := tmuxSession.ProbeSession(); known && exists {
		// A tmux session exists with no daemon reservation, in-memory instance,
		// or disk record — an orphan left by a crash or an external process.
		// No creator will ever finish it, so this stays a plain error (not
		// errConcurrentCreate): DeliverPrompt must fail fast with cleanup
		// guidance rather than wait out waitForTargetSession's timeout (#916).
		return fmt.Errorf("conflicting tmux session %q is already running; no agent-factory session owns it. Clean it up with: %s", title, shellsuggest.Command("tmux", "kill-session", "-t", tmuxSession.SanitizedName()))
	}
	return nil
}

// hookSlugOwnerInOtherRepos reports the title (and repo path) of a persisted
// remote-hook session in ANY repo other than repoID whose slug equals candidate,
// or "" when the hook name is free.
//
// The caller already scans this repo's rows and every in-memory instance; this
// covers the remaining case — a settled hook session in a different repo, whose
// rows loadRepoInstanceData(repoID) never returns. Without it the global
// hook-name namespace is only enforced against concurrent creates (the in-flight
// reservation), so two repos could take the same name sequentially and hand both
// sandboxes the identical --name.
//
// Corrupted per-repo files are surfaced rather than skipped: a hidden hook
// session would otherwise let a colliding name through, and the cost of a false
// refusal here is a clear error, while the cost of a miss is two provisioned
// sandboxes fighting over one name.
func hookSlugOwnerInOtherRepos(candidate, repoID string) (string, string, error) {
	allInstances, err := config.LoadAllRepoInstances()
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", errTitleCheckFatal, err)
	}
	var corrupted []string
	for rid, raw := range allInstances {
		if rid == repoID {
			continue
		}
		var rows []session.InstanceData
		if err := json.Unmarshal(raw, &rows); err != nil {
			corrupted = append(corrupted, rid)
			continue
		}
		for i := range rows {
			if !rows[i].IsRemoteHook() {
				continue
			}
			if session.Slugify(rows[i].Title) == candidate {
				return rows[i].Title, rows[i].Path, nil
			}
		}
	}
	if len(corrupted) > 0 {
		sort.Strings(corrupted)
		return "", "", fmt.Errorf("%w: cannot verify remote hook name %q is free: %d repo(s) have a corrupted instances.json that may be hiding a session using it: %s",
			errTitleCheckFatal, candidate, len(corrupted), strings.Join(corrupted, ", "))
	}
	return "", "", nil
}
