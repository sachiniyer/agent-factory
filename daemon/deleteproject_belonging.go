package daemon

// #7854: a legacy registry row whose recorded root is ABSENT decides which
// sessions belong to it from the row's path-hash identity and the sessions'
// own paths. A session created from a sibling worktree of a BARE repository
// is keyed under the bare repo's identity, and none of its paths touch the
// recorded root — so the delete deregistered the row while that session
// stayed live.
//
// The scan here answers "could this live session be the row's?" without
// git's worktree ledger, which `git worktree prune` erases on every archive
// (#7853's lesson). For each repository identity a live session is keyed
// under it consults evidence af itself wrote and never deletes: the session's
// canonical repo identity root, and inside a bare root the checkout marker
// registration minted there. Only an identity the evidence cannot clear
// holds the delete up.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session"
)

// absentBelongingProbeTimeout bounds one identity-root classification — a git
// probe plus, when git cannot answer, filesystem stats — and one marker read.
// The scan runs outside every daemon lock, so the bound exists for the
// filesystem alone: a repo parked on a dead mount must not freeze a delete.
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
// the identity under deletion. That is the difference that sees a session of
// a bare repo created from a sibling worktree: it is keyed under the bare
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
	// bare is whether root was PROVEN to be a bare repository — the shape
	// that could have held the recorded root as a linked worktree.
	bare bool
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
// classification — a git child plus, when it cannot answer, filesystem stats
// — to one parked observation per root. Callers time out independently, and a
// later classify joins the parked probe instead of stranding a second
// goroutine on the same dead filesystem (the checkoutMarkerReadFlights
// contract, applied to a probe).
var identityRootClassFlights sync.Map

type identityRootClassObservation struct {
	done   chan struct{}
	bare   bool
	proven bool
}

// classifyIdentityRoot answers "is this repository identity root a bare
// repository?", parking the probe so an unresponsive filesystem bounds the
// wait, not the work. proven reports whether the answer is evidence at all:
// an unanswered probe, a vanished path and a bare-shaped directory git cannot
// describe all leave the question open, and open is suspect — the delete
// fails closed, never "no sessions could belong" (#7854).
func classifyIdentityRoot(root string) *identityRootClassObservation {
	obs := &identityRootClassObservation{done: make(chan struct{})}
	actual, loaded := identityRootClassFlights.LoadOrStore(root, obs)
	if loaded {
		return actual.(*identityRootClassObservation)
	}
	go func() {
		obs.bare, obs.proven = probeIdentityRootBare(root)
		identityRootClassFlights.CompareAndDelete(root, obs)
		close(obs.done)
	}()
	return obs
}

func awaitIdentityRootClass(root string) (bare, proven bool) {
	obs := classifyIdentityRoot(root)
	timer := time.NewTimer(absentBelongingProbeTimeout)
	defer timer.Stop()
	select {
	case <-obs.done:
		return obs.bare, obs.proven
	case <-timer.C:
		select {
		case <-obs.done:
			return obs.bare, obs.proven
		default:
			return false, false
		}
	}
}

func probeIdentityRootBare(root string) (bare, proven bool) {
	// First establish the root is there at all: `git --git-dir` reports a
	// vanished path with the same exit status as a dir that is not a
	// repository, and only the stat distinguishes "inspected and answered"
	// from "could not even look" — which leaves the question open.
	if _, err := os.Stat(root); err != nil {
		return false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), boundedRecordRootAbsentTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--git-dir", root, "rev-parse", "--is-bare-repository")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err == nil {
		switch strings.TrimSpace(string(out)) {
		case "true":
			return true, true
		case "false":
			return false, true
		default:
			return false, false
		}
	}
	if ctx.Err() != nil {
		// A probe the deadline killed answered nothing.
		return false, false
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// Git answered "this is not a gitdir it knows" — exactly what a
		// non-bare worktree root reports, since a session's identity root is
		// a gitdir only when the repo is bare. The one answer that must NOT
		// clear is a corrupt-but-bare common directory: it fails the probe
		// the same way and could still have held the recorded root, so the
		// bare shape keeps the question open.
		if identityRootBareShaped(root) {
			return false, false
		}
		return false, true
	}
	return false, false
}

// identityRootBareShaped reports whether root has the directory shape of a
// bare repository — HEAD next to objects/ and refs/. It is not ownership
// evidence (the ledger is never consulted); it only stops a bare repo whose
// metadata git can no longer read from being misread as "not a bare repo".
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
// evidence order is strict — nothing git's worktree ledger could supply is
// consulted, because af itself erases it on every archive:
//
//   - an identity root no source could name is unproven, and unproven is
//     suspect;
//   - a proven non-bare identity root is cleared — the row's recorded root
//     was itself a worktree, and only a bare common directory could have
//     held it as a linked worktree of this repo;
//   - a bare root whose checkout marker differs from the row's is a
//     DIFFERENT clone — registration minted the marker into whichever common
//     directory owned the recorded root, and af never deletes it — cleared;
//   - a bare root holding the row's own marker, no marker at all, an
//     unreadable one, or a row recorded before checkout ids are all
//     unproven, and unproven is suspect.
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
		suspect := func(root string, bare bool) {
			for title := range set.titles {
				belonging.suspects = append(belonging.suspects, absentRootSuspect{title: title, root: root, bare: bare})
			}
		}
		if !set.rootKnown {
			// Key-only presence — a reserved title or in-flight restore no
			// source could hand an identity root. Nothing disproves it could
			// be the row's, so it fails closed.
			suspect("", false)
			continue
		}
		bare, proven := awaitIdentityRootClass(set.identityRoot)
		if !proven {
			suspect(set.identityRoot, false)
			continue
		}
		if !bare {
			belonging.cleared[repoID] = struct{}{}
			continue
		}
		if recordedCheckoutID == "" {
			// The row predates checkout ids: there is nothing durable to
			// discriminate this bare repo on.
			suspect(set.identityRoot, true)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), absentBelongingProbeTimeout)
		markerID, markerExists, markerErr := config.CheckoutIDInCommonDir(ctx, set.identityRoot)
		cancel()
		switch {
		case markerErr != nil, !markerExists:
			// An unreadable marker proves nothing either way, and an absent
			// marker cannot disprove that this common dir held the recorded
			// root — a missing record is never evidence (#7853).
			suspect(set.identityRoot, true)
		case markerID == recordedCheckoutID:
			// The row's own marker lives inside this bare repo: its
			// registered root was a checkout of this clone, and a session
			// created from a sibling worktree could be the row's.
			suspect(set.identityRoot, true)
		default:
			// A different clone: registration would have minted the row's
			// marker into whatever common directory owned its root, and it
			// minted this repo's instead.
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
	bareRoots := make(map[string]struct{})
	unknownRoots := make(map[string]struct{})
	unnamed := 0
	for _, s := range suspects {
		titles = append(titles, s.title)
		switch {
		case s.bare && s.root != "":
			bareRoots[s.root] = struct{}{}
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
	if roots := sortedKeys(bareRoots); len(roots) > 0 {
		clauses = append(clauses, "the bare repository "+strings.Join(roots, " and "))
	}
	if roots := sortedKeys(unknownRoots); len(roots) > 0 {
		clauses = append(clauses, "the repository "+strings.Join(roots, " and ")+", which af could not describe")
	}
	if unnamed > 0 {
		clauses = append(clauses, "a repository af could not name")
	}
	holding := strings.Join(clauses, ", and ") + " — any could have held that root as a worktree"
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
