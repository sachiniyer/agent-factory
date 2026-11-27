package daemon

// #7854: a legacy registry row whose recorded root is ABSENT decides which
// sessions belong to it from the row's path-hash identity and the sessions'
// own paths. A session created from a sibling worktree of the repository the
// root belonged to is keyed under that repo's identity, and none of its paths
// touch the recorded root — so the delete deregistered the row while the
// session stayed live. The issue frames the bare-repo shape, but a non-bare
// clone holds linked worktrees the same way and escapes identically; the scan
// covers both.
//
// It answers "could this live session be the row's?" without git's worktree
// ledger, which `git worktree prune` erases on every archive (#7853's
// lesson). The evidence is what af itself wrote and never deletes: the
// session's canonical repo identity root, and inside that repo's common
// directory the checkout marker registration minted there — a recorded
// checkout id on the row means exactly one common dir holds it.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session"
)

// absentBelongingProbeTimeout bounds one identity-root classification —
// filesystem stats only — and one marker read. The scan runs outside every
// daemon lock, so the bound exists for the filesystem alone: a repo parked on
// a dead mount must not freeze a delete.
const absentBelongingProbeTimeout = 3 * time.Second

// liveRepoIdentity buckets the live sessions keyed under one repository
// identity: their titles and, when any source can supply it, the canonical
// identity root on disk — the bare common directory for a session of a bare
// repository (#3358), the main root otherwise.
type liveRepoIdentity struct {
	titles       map[string]struct{}
	identityRoot string
	rootKnown    bool
}

// liveRepoIdentitiesLocked snapshots every live repository identity and the
// sessions keyed under it from the same four sources repoSessionTitlesLocked
// reads — reserved titles, pending creates, in-flight restores and the
// instance map — but bucketed by EACH session's own repo identity rather than
// the identity under deletion. That is the difference that sees a session
// created from a sibling worktree: it is keyed under the parent repo's
// identity, invisible to a scan of only the row's ids (#7854). The caller
// holds m.mu.
func (m *Manager) liveRepoIdentitiesLocked() map[string]*liveRepoIdentity {
	sets := make(map[string]*liveRepoIdentity)
	add := func(repoID, title, identityRoot string) {
		if repoID == "" || title == "" {
			return
		}
		set := sets[repoID]
		if set == nil {
			set = &liveRepoIdentity{titles: make(map[string]struct{})}
			sets[repoID] = set
		}
		set.titles[title] = struct{}{}
		if identityRoot != "" {
			set.identityRoot = identityRoot
			set.rootKnown = true
		}
	}
	for key := range m.reservedTitles {
		repoID, title := splitDaemonInstanceKey(key)
		add(repoID, title, "")
	}
	for key, pending := range m.pendingCreates {
		repoID, title := splitDaemonInstanceKey(key)
		add(repoID, title, pending.Worktree.RepoPath)
	}
	for key := range m.restoresInFlight {
		repoID, title := splitDaemonInstanceKey(key)
		add(repoID, title, "")
	}
	for key, inst := range m.instances {
		repoID, title := splitDaemonInstanceKey(key)
		if inst == nil || inst.GetLiveness() == session.LiveArchived {
			continue
		}
		add(repoID, title, inst.GetRepoIdentityPath())
	}
	return sets
}

// liveSessionTitlesByRepoLocked is the titles-only half of
// liveRepoIdentitiesLocked for the post-deregister re-scan: which repository
// identities any live session is keyed under, without resolving roots. The
// caller holds m.mu.
func (m *Manager) liveSessionTitlesByRepoLocked() map[string][]string {
	titles := make(map[string][]string)
	add := func(repoID, title string) {
		if repoID != "" && title != "" {
			titles[repoID] = append(titles[repoID], title)
		}
	}
	for key := range m.reservedTitles {
		add(splitDaemonInstanceKey(key))
	}
	for key := range m.pendingCreates {
		add(splitDaemonInstanceKey(key))
	}
	for key := range m.restoresInFlight {
		add(splitDaemonInstanceKey(key))
	}
	for key, inst := range m.instances {
		repoID, title := splitDaemonInstanceKey(key)
		if inst == nil || inst.GetLiveness() == session.LiveArchived {
			continue
		}
		add(repoID, title)
	}
	return titles
}

// absentRootSuspect is one live session the delete could not clear of
// belonging to the absent-rooted row it would deregister.
type absentRootSuspect struct {
	title string
	// root is the session's repository identity root; empty when no source
	// could name it.
	root string
	// owner is whether the row's own checkout marker was found inside this
	// repo's common directory — proof the recorded root was its checkout.
	owner bool
}

// absentRootBelonging is the verdict of the absent-branch ownership scan:
// cleared names every live repository identity the evidence proved could NOT
// own the row's recorded root, and suspects names the live sessions nothing
// cleared.
type absentRootBelonging struct {
	cleared  map[string]struct{}
	suspects []absentRootSuspect
}

// unvettedTitles reports the titles keyed under live repository identities
// this scan never cleared — the post-deregister form of "could belong": a
// session that materialized during the deregister under an identity the
// evidence cannot clear is suspect whether it arrived before or after the
// row came off.
func (b *absentRootBelonging) unvettedTitles(byRepo map[string][]string) []string {
	var titles []string
	for repoID, ts := range byRepo {
		if _, ok := b.cleared[repoID]; ok {
			continue
		}
		titles = append(titles, ts...)
	}
	return titles
}

// identityRootClassFlights bounds an unresponsive identity-root
// classification — filesystem stats only — to one parked observation per
// root. Callers time out independently, and a later classify joins the parked
// probe instead of stranding a second goroutine on the same dead filesystem
// (the checkoutMarkerReadFlights contract, applied to stats).
var identityRootClassFlights sync.Map

type identityRootClassObservation struct {
	done      chan struct{}
	commonDir string
	proven    bool
}

// classifyIdentityRoot answers "where is this repository identity root's
// common directory?", parking the stats so an unresponsive filesystem bounds
// the wait, not the work. For a bare repo the identity root IS the common
// dir; for a non-bare repo it is the main root and the common dir is the
// .git inside it. proven reports whether either could be established: a
// vanished root, or a present directory that is neither a checkout root nor
// bare-shaped, leaves the question open — and open is suspect, the delete
// fails closed (#7854).
func classifyIdentityRoot(root string) *identityRootClassObservation {
	obs := &identityRootClassObservation{done: make(chan struct{})}
	actual, loaded := identityRootClassFlights.LoadOrStore(root, obs)
	if loaded {
		return actual.(*identityRootClassObservation)
	}
	go func() {
		obs.commonDir, obs.proven = probeIdentityRootCommonDir(root)
		identityRootClassFlights.CompareAndDelete(root, obs)
		close(obs.done)
	}()
	return obs
}

func awaitIdentityRootClass(root string) (commonDir string, proven bool) {
	obs := classifyIdentityRoot(root)
	timer := time.NewTimer(absentBelongingProbeTimeout)
	defer timer.Stop()
	select {
	case <-obs.done:
		return obs.commonDir, obs.proven
	case <-timer.C:
		select {
		case <-obs.done:
			return obs.commonDir, obs.proven
		default:
			return "", false
		}
	}
}

func probeIdentityRootCommonDir(root string) (commonDir string, proven bool) {
	// First establish the root is there at all: a vanished identity root
	// could be the vanished owner of the recorded root, so it is unproven
	// rather than "not a bare repo".
	if _, err := os.Stat(root); err != nil {
		return "", false
	}
	if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
		return filepath.Join(root, ".git"), true
	}
	// A bare repository's common dir sits at the root itself: HEAD next to
	// objects/ and refs/. The shape is not ownership evidence — it only
	// decides where the marker would live.
	if identityRootBareShaped(root) {
		return root, true
	}
	return "", false
}

// identityRootBareShaped reports whether root has the directory shape of a
// bare repository — HEAD next to objects/ and refs/. It is not ownership
// evidence (the ledger is never consulted); it only locates the common
// directory the marker read targets, and it keeps a bare repo whose metadata
// is unreadable in the suspect class rather than cleared.
func identityRootBareShaped(root string) bool {
	head, err := os.Lstat(filepath.Join(root, "HEAD"))
	if err != nil || head.IsDir() {
		return false
	}
	objects, err := os.Stat(filepath.Join(root, "objects"))
	if err != nil || !objects.IsDir() {
		return false
	}
	refs, err := os.Stat(filepath.Join(root, "refs"))
	if err != nil || !refs.IsDir() {
		return false
	}
	return true
}

// absentRootBelongingScan is the absent-recorded-root ownership scan: every
// live repository identity is cleared, or its sessions are suspects. The
// evidence is strict — nothing git's worktree ledger could supply is
// consulted, because af itself erases it on every archive. For each identity
// root the scan finds the common directory and reads af's own checkout marker
// there — the marker registration minted into whichever clone owned the
// recorded root, which nothing in af deletes:
//
//   - a root no source could name, or one whose common directory cannot be
//     established, is unproven — suspect;
//   - a row recorded before checkout ids has nothing to discriminate on —
//     suspect;
//   - a marker read that fails is unproven — suspect;
//   - a marker equal to the row's own is the clone that owned the recorded
//     root — suspect;
//   - a marker that differs, or no marker at all, is a DIFFERENT clone —
//     cleared. Registration would have minted the row's marker into whatever
//     owned its root; whatever holds a different marker, or none, provably
//     is not that clone.
func (m *Manager) absentRootBelongingScan(recordedCheckoutID string) *absentRootBelonging {
	m.mu.Lock()
	sets := m.liveRepoIdentitiesLocked()
	m.mu.Unlock()

	belonging := &absentRootBelonging{cleared: make(map[string]struct{})}
	repoIDs := make([]string, 0, len(sets))
	for repoID := range sets {
		repoIDs = append(repoIDs, repoID)
	}
	sort.Strings(repoIDs)
	for _, repoID := range repoIDs {
		set := sets[repoID]
		suspect := func(root string, owner bool) {
			for title := range set.titles {
				belonging.suspects = append(belonging.suspects, absentRootSuspect{title: title, root: root, owner: owner})
			}
		}
		if !set.rootKnown {
			// Key-only presence — a reserved title or in-flight restore no
			// source could hand an identity root. Nothing disproves it could
			// be the row's, so it fails closed.
			suspect("", false)
			continue
		}
		commonDir, proven := awaitIdentityRootClass(set.identityRoot)
		if !proven {
			suspect(set.identityRoot, false)
			continue
		}
		if recordedCheckoutID == "" {
			// The row predates checkout ids: there is nothing durable to
			// discriminate this repo on.
			suspect(set.identityRoot, false)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), absentBelongingProbeTimeout)
		markerID, markerExists, markerErr := config.CheckoutIDInCommonDir(ctx, commonDir)
		cancel()
		switch {
		case markerErr != nil:
			// An unreadable marker proves nothing either way.
			suspect(set.identityRoot, false)
		case markerExists && markerID == recordedCheckoutID:
			// The row's own marker lives inside this repo: its recorded
			// root was a checkout of this clone, and a session created from
			// a sibling worktree could be the row's.
			suspect(set.identityRoot, true)
		default:
			// A different clone, or one never registered at all: a recorded
			// checkout id means the row's marker was minted into whichever
			// common directory owned its root, and this is provably not it.
			belonging.cleared[repoID] = struct{}{}
		}
	}
	sort.Slice(belonging.suspects, func(i, j int) bool {
		if belonging.suspects[i].title != belonging.suspects[j].title {
			return belonging.suspects[i].title < belonging.suspects[j].title
		}
		return belonging.suspects[i].root < belonging.suspects[j].root
	})
	return belonging
}

// absentRootBelongingRefusal renders the delete refusal for live sessions the
// absent-root scan could not clear. It names each session, the evidence
// class that made it suspect, and the command that resolves it.
func absentRootBelongingRefusal(repoID, repoPath string, suspects []absentRootSuspect) error {
	titles := make([]string, 0, len(suspects))
	ownerRoots := make(map[string]struct{})
	unknownRoots := make(map[string]struct{})
	unnamed := 0
	for _, s := range suspects {
		titles = append(titles, s.title)
		switch {
		case s.owner && s.root != "":
			ownerRoots[s.root] = struct{}{}
		case s.root != "":
			unknownRoots[s.root] = struct{}{}
		default:
			unnamed++
		}
	}
	titles = session.DedupeSorted(titles)
	cmds := make([]string, 0, len(titles))
	for _, title := range titles {
		cmds = append(cmds, shellsuggest.PositionalCommand("af", []string{"sessions", "archive"}, title))
	}
	var clauses []string
	if roots := sortedKeys(ownerRoots); len(roots) > 0 {
		clauses = append(clauses, "the repository "+strings.Join(roots, " and ")+", whose common directory holds this record's own checkout marker")
	}
	if roots := sortedKeys(unknownRoots); len(roots) > 0 {
		clauses = append(clauses, "the repository "+strings.Join(roots, " and ")+", which af could not describe")
	}
	if unnamed > 0 {
		clauses = append(clauses, "a repository af could not name")
	}
	holding := strings.Join(clauses, ", and ") + " — any could have owned that root as a worktree"
	return fmt.Errorf(
		"delete project %s: its recorded root %s is absent and the record predates af's repository identities, so af cannot establish which live sessions it owned — and it cannot clear session(s) %v, live under %s; nothing was changed — archive the named session(s) with %s, then delete the project again",
		repoID, repoPath, titles, holding, strings.Join(cmds, "; "),
	)
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
