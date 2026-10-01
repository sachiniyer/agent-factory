// Title admission for session creation: what names a create may claim, and the
// refusals that decide it. Split out of manager_create.go, which holds the
// creation FLOW; this file holds the naming RULES that flow consults.

package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

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

// branchNaming is the branch_prefix ONE create resolved for its project (#4539).
// Every title rule that derives a branch takes it as a value instead of reading
// config itself, and the create hands the same value to NewInstance. That
// keeps the collision check and `git worktree add` on one read, so they cannot
// name different branches.
type branchNaming struct {
	// prefix is the prefix this create's own branch gets.
	prefix string
	// global is the global branch_prefix from the same snapshot. A pair with an
	// off-box side is judged under it (git.ClaimCollision).
	global string
	// local reports whether this create builds a host-local worktree. Only such a
	// create takes a project's override.
	local bool
}

// branchFor derives the branch a session titled title gets under this prefix.
func (n branchNaming) branchFor(title string) string {
	return git.BranchForTitle(n.prefix, title)
}

// collision reports whether a create titled title cannot coexist with claim, and
// the branch they share when that is why. It delegates to git.ClaimCollision so
// the daemon's authoritative validation and the TUI's naming pre-check stay in
// lockstep (#936).
func (n branchNaming) collision(title string, claim git.BranchClaim) (string, bool) {
	return git.ClaimCollision(title, git.TitleNaming{Prefix: n.prefix, GlobalPrefix: n.global, Local: n.local}, claim)
}

// titlesCollide reports whether the two titles alone collide under the prefix
// their pair shares — the title half of naming.collision, without the
// recorded-branch defense. The archived-name-reuse rename frees only the
// TITLE, so a claim that collides solely because its recorded branch is
// defended is not a rename candidate: renaming it would leave the create
// deriving the same defended ref anyway (#4562 CI). Such claims are left for
// the ordinary record-conflict refusal.
func (n branchNaming) titlesCollide(title string, claim git.BranchClaim) bool {
	prefix := n.prefix
	if !n.local || !claim.Local {
		prefix = n.global
	}
	return git.TitlesCollide(title, claim.Title, prefix)
}

// reservationClaim is what an in-flight create reserved under key: the branch it
// derived when admitted, so a later create is judged against that and not against
// a re-derivation under whatever prefix is current now. A reservation with no
// recorded claim is judged as a same-kind claim with no branch yet.
func (m *Manager) reservationClaimLocked(naming branchNaming, key, title string) git.BranchClaim {
	if claim, ok := m.reservedTitleClaims[key]; ok {
		return claim
	}
	return git.BranchClaim{Title: title, Local: naming.local}
}

// reservedClaim is the claim an admitted create records for its reservation: the
// branch the create derived at admission, so a later create is judged against
// that and not against a re-derivation under whatever prefix is live then. An
// off-box reservation pins it too — for one, naming.prefix IS the global prefix —
// because this commit makes a saved branch_prefix change take effect without a
// restart, and re-deriving an in-flight off-box reservation under a newer global
// value can refuse a pair admission already cleared or clear one it refused
// (#4562 review). An in-place create adopts whatever branch the target worktree
// already has, so it records none and is judged by derivation, as it was before
// claims existed.
func reservedClaim(naming branchNaming, title string, inPlace bool) git.BranchClaim {
	claim := git.BranchClaim{Title: title, Local: naming.local}
	if !inPlace {
		claim.Branch = naming.branchFor(title)
		claim.Pinned = true
	}
	return claim
}

// branchNamingForCreate resolves the branch_prefix a create in repo uses. A
// host-local create takes the project's personal override when it sets one,
// otherwise the global value in cfg, the create's op-entry snapshot (#2480). The
// override file is read on every call, so a saved change, global or per
// project, names the next session's branch without a daemon restart.
//
// An off-box create (Docker, SSH, hook, sandbox) never takes the override: its
// branch is made inside the sandbox from that machine's config, which the host's
// personal override does not reach. It keeps the global prefix, as it did before
// per-project prefixes existed, so the override cannot change whether an off-box
// session may be created.
//
// Call it once per create, before the manager lock (#2931): it reads the project
// registry and the override file.
//
// If resolution fails, the create falls back to cfg's global prefix and logs a
// warning, like every other create-time per-project read (defaultProgramFor, the
// backend kind, the post-worktree hooks). The resolver also fails on an invalid
// checked-in config, which cannot even set this key, and refusing the create
// there would block a project that creates sessions today.
func (m *Manager) branchNamingForCreate(cfg *config.Config, repo *config.RepoContext, namespace runtimeNameNamespace) branchNaming {
	naming := branchNaming{prefix: cfg.BranchPrefix, global: cfg.BranchPrefix, local: namespace == runtimeNamespaceLocalTmux}
	if !naming.local {
		return naming
	}
	resolved, err := config.ResolveConfigForRepoInspectionWithGlobal(repo, cfg)
	if err != nil {
		m.warn().Printf("could not resolve branch_prefix for %s, so this session's branch uses the global prefix %q: %v",
			repo.WorkspacePath(), cfg.BranchPrefix, err)
		return naming
	}
	naming.prefix = resolved.BranchPrefix
	return naming
}

// titleCollisionNamespace asks every name encoder a create and an existing claim
// will actually both hold. Git and tmux intentionally have different grammars,
// so either one may collide while the other does not. The tmux half is enabled
// only when both records use the host-local runtime. The returned branch is the
// one they share, for the refusal message.
func (m *Manager) titleCollisionNamespace(naming branchNaming, repoPath, title string, claim git.BranchClaim, bothUseLocalTmux bool, diskData []session.InstanceData) (titleNamespace, string) {
	if branch, ok := naming.collision(title, claim); ok &&
		!m.recordedCollisionHeldByLiveLane(naming, repoPath, title, claim, branch, diskData) {
		return titleNamespaceBranch, branch
	}
	if bothUseLocalTmux && tmux.SanitizedNameForRepo(claim.Title, repoPath) == tmux.SanitizedNameForRepo(title, repoPath) {
		return titleNamespaceTmux, ""
	}
	return titleNamespaceNone, ""
}

// recordedCollisionHeldByLiveLane reports whether a collision exists ONLY
// because title derives the branch a claim recorded — and a live lane's
// worktree currently has that branch checked out. Such a collision defers to
// refuseLiveHeldBranchLocked, which names the lane and offers the handoff
// command; reporting it here as a record conflict would mask the actionable
// refusal (#4562 review). The TitlesCollide gate keeps every pair master
// already flagged on the record-conflict path it had before claims recorded
// branches: only the purely-new defense defers.
func (m *Manager) recordedCollisionHeldByLiveLane(naming branchNaming, repoPath, title string, claim git.BranchClaim, branch string, diskData []session.InstanceData) bool {
	if claim.Branch == "" || branch == "" {
		return false
	}
	prefix := naming.prefix
	if !naming.local || !claim.Local {
		prefix = naming.global
	}
	if git.TitlesCollide(title, claim.Title, prefix) {
		return false
	}
	for _, holder := range m.worktreeHeldBranchesLocked(repoPath, false)[branch] {
		if m.liveLaneHoldingWorktreeLocked(holder, diskData) != "" {
			return true
		}
	}
	return false
}

// findTitleConflictLocked returns the existing title that conflicts with the
// given candidate, along with the source and namespace of the conflict. An empty
// result means the title is available. Local sessions claim both a git branch and
// a tmux session name; admission rejects either collision before creating a
// worktree or launching a runtime.
func (m *Manager) findTitleConflictLocked(naming branchNaming, repoID, repoPath, title string, localTmux bool, diskData []session.InstanceData) (string, titleConflictKind, titleNamespace, string) {
	for key := range m.reservedTitles {
		rid, existing := splitDaemonInstanceKey(key)
		if rid != repoID {
			continue
		}
		claim := m.reservationClaimLocked(naming, key, existing)
		if branch, ok := naming.collision(title, claim); ok && !m.recordedCollisionHeldByLiveLane(naming, repoPath, title, claim, branch, diskData) {
			return existing, titleConflictReserved, titleNamespaceBranch, branch
		}
	}
	if localTmux {
		nameKey := daemonInstanceKey(repoID, tmux.SanitizedNameForRepo(title, repoPath))
		if existing, reserved := m.reservedTmuxNames[nameKey]; reserved {
			return existing, titleConflictReserved, titleNamespaceTmux, ""
		}
	}
	for key, inst := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil {
			continue
		}
		bothUseLocalTmux := localTmux && inst.Capabilities().Workspace == session.WorkspaceLocalWorktree
		if namespace, branch := m.titleCollisionNamespace(naming, repoPath, title, inst.BranchClaim(), bothUseLocalTmux, diskData); namespace != titleNamespaceNone {
			return inst.Title, titleConflictLive, namespace, branch
		}
	}
	for _, data := range diskData {
		bothUseLocalTmux := localTmux && data.UsesLocalTmux()
		namespace, branch := m.titleCollisionNamespace(naming, repoPath, title, data.BranchClaim(), bothUseLocalTmux, diskData)
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
		return data.Title, titleConflictDisk, namespace, branch
	}
	return "", titleConflictNone, titleNamespaceNone, ""
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
func (m *Manager) validateTitleAvailableLocked(naming branchNaming, repoID, repoPath, title, program string, namespace runtimeNameNamespace, allowReserved bool, diskData []session.InstanceData, inPlace bool, remedyPath ...string) error {
	refusalPath := repoPath
	if len(remedyPath) > 0 {
		refusalPath = remedyPath[0]
	}
	if err := m.validateTitleShapeLocked(refusalPath, title, namespace, allowReserved); err != nil {
		return err
	}
	if err := m.findTitleRecordConflictLocked(naming, repoID, repoPath, title, namespace, diskData); err != nil {
		return err
	}
	return m.validateTitleNamespacesLocked(repoID, repoPath, title, program, namespace, diskData, nil, inPlace)
}

// validateTitleClaimableLocked is every refusal that does NOT depend on af's own
// record for this title — the ones an archived-name-reuse rename cannot clear, so
// a create that trips one is doomed no matter what the rename does.
//
// ignore is the archived instance the caller is about to rename out of the way.
// It is excluded from the archive-directory and hook-slug scans, which would
// otherwise report the row being freed and refuse a valid reuse.
// It is not excluded from the tmux probe: archiving kills the session's pane, so
// an archived row never owns a live tmux name, and anything the probe finds is a
// genuine orphan the rename has no effect on.
func (m *Manager) validateTitleClaimableLocked(repoID, repoPath, title, program string, namespace runtimeNameNamespace, allowReserved bool, diskData []session.InstanceData, ignore *session.Instance, inPlace bool, remedyPath ...string) error {
	refusalPath := repoPath
	if len(remedyPath) > 0 {
		refusalPath = remedyPath[0]
	}
	if err := m.validateTitleShapeLocked(refusalPath, title, namespace, allowReserved); err != nil {
		return err
	}
	return m.validateTitleNamespacesLocked(repoID, repoPath, title, program, namespace, diskData, ignore, inPlace)
}

// validateTitleShapeLocked rejects titles that are malformed for the selected
// runtime namespace or reserved, independent of any existing session.
func (m *Manager) validateTitleShapeLocked(repoPath, title string, namespace runtimeNameNamespace, allowReserved bool) error {
	// Whitespace-only titles (e.g. "   ") are non-empty and so slip past a bare
	// == "" check, creating sessions with effectively blank names (#973). Trim
	// before the emptiness gate; the TUI naming flow applies the same check.
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("session title is required")
	}
	// Titles render as one sidebar row in every TUI client. Reject control
	// characters at the authoritative create boundary so CLI/API callers cannot
	// bypass the TUI's pasted-rune sanitization and create multi-line rows (#2640).
	if strings.IndexFunc(title, unicode.IsControl) >= 0 {
		return fmt.Errorf("session title must be a single line and contain no control characters")
	}
	// Hook scripts receive one globally shared --name derived by Slugify. A title
	// whose bounded sanitized form retains no ASCII alphanumeric content collapses
	// to the generic "session" fallback, so reject it before collision checks or
	// provisioning. Docker and SSH may use the same slug helper for repo-scoped
	// paths or labels, but do not claim this global hook namespace and therefore
	// keep accepting Unicode titles.
	if namespace == runtimeNamespaceRemoteHook && !session.RemoteHookTitleHasSpecificSlug(title) {
		return fmt.Errorf("remote hook session title %q must retain at least one ASCII letter or digit after hook-name sanitization and truncation; add an ASCII component so it derives a specific hook name", title)
	}
	// The "root" title belongs to the daemon-managed root agent (#1106).
	// Every creation path lands here — TUI, `af sessions create`, task
	// spawns, DeliverPrompt auto-creates — so this single gate reserves the
	// name everywhere. Only the daemon's own ensure loop passes
	// allowReserved; title-base derivation (nextAvailableTitleLocked) never
	// does, so a base of "root" skips to "root-2" instead of erroring.
	//
	// What is reserved is the NAME, not the spelling (#3732). The refusal asks
	// session.ReservedTitleCollision, which also catches a title deriving the
	// reserved tmux session name without looking like "root" — "ro ot", whose
	// interior space toTmuxName deletes. That check belongs HERE, in the shape
	// half, rather than in findTitleConflictLocked below: it compares against a
	// constant rather than against any af record, so it is a refusal the
	// archived-name-reuse rename can never clear, and validateTitleClaimableLocked
	// must therefore see it too (#2415). The record scan still owns the other
	// axis — a derived-name collision with an existing session, reserved or not.
	if !allowReserved {
		if err := session.ReservedTitleRefusalFor(title, repoPath); err != nil {
			return err
		}
	}
	return nil
}

// findTitleRecordConflictLocked reports a collision with an existing af record —
// a reservation, a loaded instance, or a durable row. This is the ONE group the
// archived-name-reuse rename clears, which is why it is not part of
// validateTitleClaimableLocked.
func (m *Manager) findTitleRecordConflictLocked(naming branchNaming, repoID, repoPath, title string, namespace runtimeNameNamespace, diskData []session.InstanceData) error {
	// Titles are sanitized into git branch names (git.SanitizeBranchName
	// lowercases, turns spaces into dashes, strips unsafe chars, and collapses
	// dashes), so distinct titles can map to the same branch: "MyApp"/"myapp"
	// (#605) or "A B"/"a-b" (#741) both collide. The second worktree create
	// would otherwise fail with a cryptic git error, so reject the conflict
	// here, before any worktree or tmux setup runs.
	if existing, kind, collisionNamespace, branch := m.findTitleConflictLocked(naming, repoID, repoPath, title, namespace == runtimeNamespaceLocalTmux, diskData); existing != "" {
		switch {
		case existing == title:
			if kind == titleConflictReserved {
				return fmt.Errorf("session with title %q is already reserved: %w", title, errConcurrentCreate)
			}
			return fmt.Errorf("session with title %q already exists: %w", title, errConcurrentCreate)
		case collisionNamespace == titleNamespaceTmux:
			return fmt.Errorf("session titled %q conflicts with existing session %q: both map to tmux session %q", title, existing, tmux.SanitizedNameForRepo(title, repoPath))
		case branch == "":
			return fmt.Errorf("session titled %q conflicts with existing session %q: titles that differ only in case cannot coexist", title, existing)
		default:
			return fmt.Errorf("session titled %q conflicts with existing session %q: both sanitize to the same git branch %q", title, existing, branch)
		}
	}
	return nil
}

// validateTitleNamespacesLocked refuses a title whose EXTERNAL name claims are
// already taken: the per-repo archive directory, the global hook-slug namespace
// external provisioners key on, and a live tmux session of the same name.
// See validateTitleClaimableLocked for what ignore excludes and why.
func (m *Manager) validateTitleNamespacesLocked(repoID, repoPath, title, program string, namespace runtimeNameNamespace, diskData []session.InstanceData, ignore *session.Instance, inPlace bool) error {
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
	}
	if namespace != runtimeNamespaceLocalTmux {
		return nil
	}
	if err := m.validateArchiveTitleLocked(repoID, title, diskData, ignore, inPlace); err != nil {
		return err
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
		tmuxName := tmuxSession.SanitizedName()
		return fmt.Errorf("conflicting tmux session %q is already running; no agent-factory session owns it. Clean it up with: %s", tmuxName, shellsuggest.Command("tmux", "kill-session", "-t", tmuxName))
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
// Per-repo files this cannot READ and per-repo files it cannot PARSE are both
// surfaced rather than skipped: a hidden hook session would otherwise let a
// colliding name through, and the cost of a false refusal here is a clear error,
// while the cost of a miss is two provisioned sandboxes fighting over one name.
//
// The two are the same epistemic state (#3476). This function exists to prove a
// name is FREE, and neither a file it could not parse nor one it could not open
// is evidence of that — so it takes the loader form that REPORTS unread repos.
// config.LoadAllRepoInstances drops them silently, which made one unreadable
// file in an unrelated project answer "no collision" and admit the duplicate.
//
// Two things keep the fail-closed cost bounded. A collision found in a repo that
// DID load short-circuits above, so an unrelated unread file never downgrades a
// specific "taken by X in Y" into a generic refusal. And only the remote-hook
// namespace consults this scan at all — local titles are per-repo — so a stale
// unreadable project file cannot block ordinary session creation. What is left
// is a refusal that names the file and the I/O error, which is repairable.
func hookSlugOwnerInOtherRepos(candidate, repoID string) (string, string, error) {
	allInstances, skipped, err := config.LoadAllRepoInstancesReportingSkipDetails()
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
	// Past the scan, so nothing that loaded holds the name. Everything below is a
	// hole in the evidence, not an answer.
	var unreadable []config.RepoInstancesSkip
	for _, skip := range skipped {
		// This repo's own rows are the caller's job — the create path loads them
		// via loadRepoInstanceData, which propagates its read error — and this
		// scan excludes them by contract, so refusing on them here would be a
		// second, differently-worded refusal for a case already covered.
		if skip.RepoID == repoID {
			continue
		}
		unreadable = append(unreadable, skip)
	}
	var problems []string
	if len(unreadable) > 0 {
		problems = append(problems, config.DescribeRepoInstancesSkips(unreadable))
	}
	if len(corrupted) > 0 {
		sort.Strings(corrupted)
		problems = append(problems, fmt.Sprintf("%d repo(s) have a corrupted instances.json: %s",
			len(corrupted), strings.Join(corrupted, ", ")))
	}
	if len(problems) > 0 {
		return "", "", fmt.Errorf("%w: cannot verify remote hook name %q is free — %s; any of them may be hiding a session already using it, so %s",
			errTitleCheckFatal, candidate, strings.Join(problems, "; "), config.RepoInstancesSkipRemedy(unreadable))
	}
	return "", "", nil
}

// findArchivedOnlyCollisionLocked returns the ONE loaded archived instance whose
// title collides with `title`, together with its manager-map key — but only when
// it is the sole claim across reservations, loaded instances, and durable rows,
// and only when no exclusive operation is already running against it (#2779).
// A live/reserved collision returns nil so ordinary availability validation
// reports it. Multiple claims return an error immediately: renaming an arbitrary
// loaded winner would mutate user state and still leave the requested runtime
// name unavailable.
// Runs under m.mu.
func (m *Manager) findArchivedOnlyCollisionLocked(naming branchNaming, repoID, repoPath, title string, namespace runtimeNameNamespace, diskData []session.InstanceData) (*session.Instance, string, error) {
	for key := range m.reservedTitles {
		rid, existing := splitDaemonInstanceKey(key)
		if rid != repoID {
			continue
		}
		claim := m.reservationClaimLocked(naming, key, existing)
		if branch, ok := naming.collision(title, claim); ok && !m.recordedCollisionHeldByLiveLane(naming, repoPath, title, claim, branch, diskData) {
			// A concurrent create is reserving a colliding name; let the
			// availability check reject with errConcurrentCreate.
			return nil, "", nil
		}
	}
	if namespace == runtimeNamespaceLocalTmux {
		nameKey := daemonInstanceKey(repoID, tmux.SanitizedNameForRepo(title, repoPath))
		if _, reserved := m.reservedTmuxNames[nameKey]; reserved {
			return nil, "", nil
		}
	}
	var archived *session.Instance
	var archivedKey string
	for key, inst := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil {
			continue
		}
		bothUseLocalTmux := namespace == runtimeNamespaceLocalTmux && inst.Capabilities().Workspace == session.WorkspaceLocalWorktree
		claim := inst.BranchClaim()
		collisionNamespace, _ := m.titleCollisionNamespace(naming, repoPath, title, claim, bothUseLocalTmux, diskData)
		if collisionNamespace == titleNamespaceNone {
			continue
		}
		// A claim colliding only through its defended recorded branch is not a
		// title this rename can free — renaming it would leave the create
		// deriving the same ref the record still owns (#4562 CI). Leave it to
		// the record-conflict refusal.
		if collisionNamespace == titleNamespaceBranch && !naming.titlesCollide(title, claim) {
			continue
		}
		if inst.GetLiveness() != session.LiveArchived {
			// A live session still holds the name — do not rename around it.
			return nil, "", nil
		}
		if archived != nil {
			return nil, "", fmt.Errorf("cannot reuse session name %q: archived sessions %q and %q both claim its runtime namespace; rename or permanently delete one before retrying",
				title, archived.Title, inst.Title)
		}
		archived = inst
		archivedKey = key
	}
	if archived == nil {
		// A disk-only claim will be rejected by the ordinary availability check.
		// With no loaded archived row there is nothing this helper could mutate,
		// so leave that path's established diagnostic in charge.
		return nil, "", nil
	}

	// An exclusive lifecycle operation already owns this archived session, so it
	// is not a free name to rename around (#2779). killsInFlight is that fence:
	// restore, kill, archive and the root-kill path each claim it under m.mu
	// before touching a session, and the reuse rename — which relocates a
	// worktree, rewrites a durable record and re-keys the manager map — is every
	// bit as exclusive, yet it was the one such mutation that never asked.
	//
	// What that cost: RestoreArchived claims the fence, takes the per-session op
	// lock, and then RELEASES m.mu before moving the worktree — it has to, because
	// the move is a bounded git subprocess and no manager-wide lock may be held
	// across it. reserveCreate holds m.mu across the rename, but m.mu was never
	// what the two contended on. Both ended up inside
	// GitWorktree.relocateWorktreeTo on the SAME worktree object, which has no
	// internal synchronization: one call reads the source path the other is
	// rewriting, and their filesystem steps interleave. In the observed ordering
	// the create won, the restoring session was renamed to "<title> (archived)"
	// with its worktree moved out from under the restore, and the restore then
	// failed against a source that no longer existed — the user's uncommitted work
	// left in an archive directory nothing had asked to move.
	//
	// Reading the fence HERE is what makes the check sound rather than
	// approximate. m.mu is the only thing ordering the two: reserveCreate holds it
	// unbroken from this read through the rename, so a claim either lands before
	// this read — and refuses the create — or cannot land until the rename has
	// finished and re-keyed the row, where the claimant's own
	// `m.instances[key] != instance` recheck catches it. There is no third
	// interleaving.
	//
	// It lives in this shared helper rather than in renameArchivedForReuseLocked
	// for the same reason #2415 moved the record-independent checks up: all three
	// callers ask this function whether an archived row may be renamed, and the
	// two that run BEFORE the rename are what turn this into a side-effect-free
	// refusal instead of one discovered after the worktree had already moved.
	//
	// Refusing, rather than quietly returning "no collision": the ordinary
	// availability error would say the title is taken without saying that
	// something is actively doing something about it. Naming the in-flight
	// operation is what makes "retry in a moment" the obvious next step.
	if _, busy := m.killsInFlight[archivedKey]; busy {
		return nil, "", fmt.Errorf("cannot reuse session name %q: an operation is already in progress for the archived session %q; retry once it finishes",
			title, archived.Title)
	}

	// diskData contains the persisted copy of the loaded archived row as well as
	// rows refreshLocked could not materialize. Consume exactly ONE matching copy
	// of the loaded row; every other colliding non-Loading record is an independent
	// namespace claim. Checking it before RenameArchived is load-bearing: the
	// later availability check also sees disk-only rows, but by then the archive's
	// worktree, title, manager key, and storage row have already been rewritten.
	matchedPersistedCopy := false
	for _, data := range diskData {
		bothUseLocalTmux := namespace == runtimeNamespaceLocalTmux && data.UsesLocalTmux()
		claim := data.BranchClaim()
		collisionNamespace, _ := m.titleCollisionNamespace(naming, repoPath, title, claim, bothUseLocalTmux, diskData)
		if collisionNamespace == titleNamespaceNone || data.Status == session.Loading {
			continue
		}
		// Same discrimination as the loaded-instance scan above: a branch-only
		// defense is a refusal, not a claim this rename frees.
		if collisionNamespace == titleNamespaceBranch && !naming.titlesCollide(title, claim) {
			continue
		}
		if !matchedPersistedCopy && data.Title == archived.Title && data.ID == archived.ID {
			matchedPersistedCopy = true
			continue
		}
		return nil, "", fmt.Errorf("cannot reuse session name %q: archived session %q and stored session %q both claim its runtime namespace; rename or permanently delete one before retrying",
			title, archived.Title, data.Title)
	}
	return archived, archivedKey, nil
}
