package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// The reason-bearing verdict behind every root-agent consumer refusal (#3264):
// one authority answers "will the ensure loop (re-)create this repo's root,
// and if not, why not", and one renderer turns a refusing verdict into the
// clause a consumer shows the user. Split from rootagent.go (#1145
// file-length lint) along that concern.

// rootAgentMaterializeReason names the answer repoRootAgentWillMaterialize
// gives — and, for a refusal, the cause a consumer can put in front of the
// user (#3264). A refusal without a reason reads as a bug: the pre-#3264
// messages guessed at causes ("unconfigured, unresolved, or its project may
// be deleted") that omitted every fail-closed state the daemon can actually
// be in.
type rootAgentMaterializeReason int

const (
	// rootAgentWillMaterialize: the ensure loop will (re-)create this root.
	rootAgentWillMaterialize rootAgentMaterializeReason = iota
	// rootAgentProjectDeleted: the project was deleted at runtime (#1735); the
	// ensure loop suppresses the root for the rest of this daemon's life.
	rootAgentProjectDeleted
	// rootAgentNotConfigured: no legacy root_agents entry and no registered
	// project — nothing opts this repo in.
	rootAgentNotConfigured
	// rootAgentRecordsUnreadable: no READABLE config covers this repo, and at
	// least one registry record could not be read — its repo is
	// unattributable, so it may be this one (#3297/#3316 review). Advising
	// "add a root_agents entry" here would misdirect toward the fail-open
	// residue instead of the record repair.
	rootAgentRecordsUnreadable
	// rootAgentAttributionPending: a re-attribution probe has resolved this
	// repo as some unresolved project's real identity but not yet delivered
	// its marker verdict — the project's layers still sit under the derived
	// ID, so the decision is unknowable for the moment (#3299 review
	// round 8).
	rootAgentAttributionPending
	// rootAgentProjectUnresolved: the repo's registered project is configured
	// and resolves to enabled, but its recorded root did not resolve to a git
	// repository at daemon start and no legacy entry exists, so nothing can
	// create the root this run (#3247 arm 2). Calling this "not configured"
	// would send the user to add config that already exists (#3264 review).
	rootAgentProjectUnresolved
	// rootAgentDisabled: the layered config resolved and says enabled=false.
	rootAgentDisabled
	// rootAgentRegistryUnreadable: the project registry could not be listed at
	// daemon start (#3247); no repo can be proven un-disabled.
	rootAgentRegistryUnreadable
	// rootAgentPersonalUnreadable: this repo's personal config exists but
	// could not be loaded at daemon start (#3241).
	rootAgentPersonalUnreadable
	// rootAgentLegacyProbeUnanswered: whether a root_agents entry names this
	// repo could not be established, because a key's repository probe never
	// answered (#3782 item 4). It is the ONLY reason here that is about the
	// daemon rather than about the configuration, and it is deliberately not
	// collapsed into rootAgentNotConfigured: "no root agent is configured for
	// this repo — add a root_agents entry" is advice a user cannot act on when
	// the entry is already there, and #3500's rule is that a subprocess that
	// did not answer establishes nothing about the user's config.
	//
	// Unlike every other refusing reason it is TRANSIENT: nothing about the
	// config has to change, and the next request re-probes.
	rootAgentLegacyProbeUnanswered
)

// rootAgentMaterializeVerdict pairs the reason with what a message needs to
// name it: the project whose personal config failed to load, or the layer
// that decided a disable.
type rootAgentMaterializeVerdict struct {
	reason    rootAgentMaterializeReason
	projectID string
	// enabledSource is the layer that supplied the effective `enabled` value,
	// set for rootAgentDisabled (which layer said false — the built-in source
	// means NO layer enabled the repo, a materially different remedy from an
	// explicit disable) and for rootAgentProjectUnresolved (which layer said
	// true — attributing a global enable to the project's own config would be
	// a false cause). #3304 review, both.
	enabledSource config.RootAgentSource
	// unreadableRecords names the registry record directories the snapshot
	// could not read, for rootAgentRecordsUnreadable messages.
	unreadableRecords []string
	// rootUnresolved marks that the repo's registered project root did not
	// resolve at daemon start. Carried on rootAgentDisabled too: a disable
	// remedy that only says "enable it and restart" is incomplete when the
	// path must also come back before any restart can create the root.
	rootUnresolved bool
	// rootIdentityMismatch narrows rootUnresolved: the recorded path RESOLVES,
	// but the checkout there does not carry the project's registry marker.
	// "Bring the path back" is an impossible remedy there — the path is
	// present; the fix is a rebind plus a daemon restart (#3299 review
	// round 4).
	rootIdentityMismatch bool
	// rootMarkerUnreadable narrows rootUnresolved the other way: the recorded
	// path resolves but the marker could not be READ — identity unknowable.
	// Neither "bring the path back" nor a rebind applies; the readability
	// problem is the remedy (#3299 review round 5).
	rootMarkerUnreadable bool
	// rootPathVanished narrows rootMarkerUnreadable: the recorded path itself
	// disappeared mid-verification — the remedy is the path, not marker
	// readability (#3299 review round 12).
	rootPathVanished bool
	// createRefusedAt stamps the create-boundary identity refusal this verdict
	// was derived from (#3714), and is zero for every snapshot-derived one.
	// The three narrowing flags above carry the CAUSE either way — the same
	// three outcomes, the same rendered clauses — and this is what lets the
	// message add the one thing the snapshot-derived wording cannot say: that
	// the daemon is trying and being refused right now, rather than reporting
	// something established once at daemon start.
	createRefusedAt time.Time
}

// rootAgentMaterializeVerdictFor is the single authority for "will the ensure
// loop (re-)create this repo's root, and if not, why not". It applies the
// same checks as the ensure loop, in the same order the daemon's policy ranks
// them: a runtime deletion outlives every config claim; an unreadable
// registry or personal config makes the decision unknown (fail closed,
// #3241/#3247) before any layer is consulted — also skipping the git-forking
// legacy lookup below, whose result could not change the answer; then
// candidacy; then the layered resolution the ensure sweeps share.
func (m *Manager) rootAgentMaterializeVerdictFor(repoID string) rootAgentMaterializeVerdict {
	// The pending gate is sampled BEFORE the snapshot load (#3299 review
	// round 11): the healer publishes the moved layers and only then retires
	// the probe, so a gate observed open here guarantees the Load below sees
	// the post-transition snapshot — sampled the other way around, a verdict
	// could pass the gate yet resolve against layers still keyed by the
	// derived ID.
	if m.rootAttributionPendingFor(repoID) {
		return rootAgentMaterializeVerdict{reason: rootAgentAttributionPending, rootUnresolved: true}
	}
	// One Load for the whole verdict, so the flags, the candidacy sets, and the
	// resolution are read from a single consistent snapshot value. Loaded
	// before the deletion check because deletion state may be keyed by the
	// derived recorded-path ID a re-attributed repo used to carry — the
	// snapshot's reattributedFrom alias bridges the two (#3299 review round 4).
	layers := m.rootAgentLayers.Load()
	m.mu.Lock()
	// Exactly the sweep's rule, from the one function that owns it — a verdict
	// that disagreed with the sweep about whether a project was deleted would
	// explain a root that is running, or fail to explain one that is not.
	deleted := m.rootDeletionTombstoneApplies(layers, repoID)
	m.mu.Unlock()
	if deleted {
		return rootAgentMaterializeVerdict{reason: rootAgentProjectDeleted}
	}
	if layers.registryUnreadable {
		return rootAgentMaterializeVerdict{reason: rootAgentRegistryUnreadable}
	}
	if projectID, unreadable := layers.personalUnreadable[repoID]; unreadable {
		// Mirror decisionUnknown (#3299 review round 14): a PROVEN mismatch
		// means the unreadable config's claimant is disproven at this path,
		// so the occupant's verdict must not send users to repair a dead
		// project's file.
		if record, ok := layers.unresolvedRoots[repoID]; !ok || !record.identityMismatch {
			return rootAgentMaterializeVerdict{reason: rootAgentPersonalUnreadable, projectID: projectID}
		}
	}
	// A direct unresolved record whose marker is unreadable (or whose path
	// vanished mid-verification) makes the decision unknowable for BOTH
	// same-ID and bridged shapes — decisionUnknown fails the sweeps closed —
	// and the verdict names that cause directly rather than falling through
	// to a resolution that would misreport it as an ordinary disable (#3299
	// review round 15).
	if record, ok := layers.unresolvedRoots[repoID]; ok && record.markerUnreadable {
		return rootAgentMaterializeVerdict{reason: rootAgentProjectUnresolved, rootUnresolved: true, rootMarkerUnreadable: true, rootPathVanished: record.pathVanished, projectID: record.projectID}
	}
	// RED (#3782 item 4): the unknown answer is dropped, so an unanswered
	// probe still reports the repo as unconfigured.
	legacy, _ := m.legacyRootAgentForRepo(repoID)
	_, isProject := layers.projectRoots[repoID]
	unresolved, isUnresolved := layers.unresolvedRoots[repoID]
	if legacy == nil && isUnresolved && unresolved.root != "" {
		// A root_agents key is a PATH, and LegacyRootAgentForRepo falls back to
		// hashing one it cannot resolve — which is no longer this project's
		// identity now that an unresolved record is addressed by the identity
		// it RECORDED (#3530). Master matched here by accident, because it
		// addressed such a project BY that hash. Without this a project whose
		// opt-in sits in root_agents the whole time resolves to "disabled"
		// instead of "enabled, but its recorded path did not resolve" — the
		// misreport #3264 exists to prevent.
		// RED (#3782 item 4): same, for the recorded-root fallback.
		legacy, _ = m.legacyRootAgentForRecordedRoot(unresolved.root)
	}
	if legacy == nil && !isProject && !isUnresolved {
		if len(layers.recordFailureIDs) > 0 {
			return rootAgentMaterializeVerdict{reason: rootAgentRecordsUnreadable, unreadableRecords: layers.recordFailureIDs}
		}
		return rootAgentMaterializeVerdict{reason: rootAgentNotConfigured}
	}
	resolution := layers.resolve(repoID, legacy)
	if !resolution.Enabled {
		return rootAgentMaterializeVerdict{reason: rootAgentDisabled, enabledSource: resolution.EnabledSource, rootUnresolved: isUnresolved, rootIdentityMismatch: unresolved.identityMismatch, rootMarkerUnreadable: unresolved.markerUnreadable, rootPathVanished: unresolved.pathVanished, projectID: unresolved.projectID}
	}
	if legacy == nil && !isProject {
		// Enabled on paper, but the recorded root did not resolve at daemon
		// start and no legacy entry's per-tick retry covers the repo: nothing
		// can create this root until a daemon start where the path resolves.
		return rootAgentMaterializeVerdict{reason: rootAgentProjectUnresolved, enabledSource: resolution.EnabledSource, rootUnresolved: true, rootIdentityMismatch: unresolved.identityMismatch, rootMarkerUnreadable: unresolved.markerUnreadable, rootPathVanished: unresolved.pathVanished, projectID: unresolved.projectID}
	}
	// Policy allows it and the snapshot's binding is healthy — and until #3714
	// that was the whole answer, so a repo whose every create was refused at
	// the identity boundary read as will-materialize and sent
	// deliverToReemergingRoot to wait out targetDeliverWait for a root that was
	// never coming. The record is consulted LAST on purpose: it can only turn a
	// would-be will-materialize into a refusal, so no verdict that already had
	// a cause changes, and every consumer that treats an unresolved root as
	// "still the project's" (DeleteProject's live-session preflight) keeps the
	// answer it had.
	if refusal, ok := m.rootCreateRefusalFor(repoID, layers.projectRoots[repoID]); ok {
		return rootAgentCreateRefusalVerdict(refusal, resolution.EnabledSource)
	}
	return rootAgentMaterializeVerdict{reason: rootAgentWillMaterialize}
}

// rootAgentCreateRefusalVerdict renders a standing create-boundary refusal as a
// verdict, mapping its outcome class onto the narrowing flag that already
// carries that cause. rootUnresolved stays FALSE throughout: the recorded root
// resolved — that is why a create was attempted at all — and the flag means the
// opposite (it did not resolve at daemon start). Only the rootAgentDisabled
// clause reads it, and a disable outranks this verdict anyway.
func rootAgentCreateRefusalVerdict(refusal rootCreateRefusal, enabledSource config.RootAgentSource) rootAgentMaterializeVerdict {
	verdict := rootAgentMaterializeVerdict{
		reason:          rootAgentProjectUnresolved,
		projectID:       refusal.binding.projectID,
		enabledSource:   enabledSource,
		createRefusedAt: refusal.at,
	}
	switch refusal.class {
	case rootCreateRefusalMismatch:
		verdict.rootIdentityMismatch = true
	case rootCreateRefusalRootGone:
		// The gone clause is worded for a path that vanished while its identity
		// was being verified, which is exactly what happened here — the create
		// boundary IS that verification.
		verdict.rootPathVanished = true
	default:
		// rootCreateRefusalMarkerUnreadable — and the fail-safe reading of a
		// class that reached here unset, because this is the one clause that
		// prescribes NO rebind. A wrong guess in the other direction would tell
		// a user to rebind over a checkout that may be the original.
		verdict.rootMarkerUnreadable = true
	}
	return verdict
}

// legacyRootAgentForRecordedRoot is config's shared recorded-root lookup, which
// states the rule and its two limits; the daemon reads it from its start-of-day
// config exactly as every other legacy lookup here does.
func (m *Manager) legacyRootAgentForRecordedRoot(root string) (*config.RootAgentConfig, error) {
	// RED (#3782 item 4): unbounded, on the same goroutine and under the same
	// lock as the lookup above it.
	entry, _, err := legacyRootAgentForRecordedRootContext(context.Background(), m.cfg, root)
	return entry, err
}

// legacyRootAgentForRecordedRootContext is the recorded-root half of the same
// seam. A package var for the same reason; production assigns it once.
var legacyRootAgentForRecordedRootContext = config.LegacyRootAgentForRecordedRootContext

// rootAgentUnavailableDetail renders a refusing verdict as the clause a
// consumer appends to its message: what stops the root, and what fixes it.
// Empty for a verdict that will materialize.
func rootAgentUnavailableDetail(v rootAgentMaterializeVerdict) string {
	switch v.reason {
	case rootAgentProjectDeleted:
		// Suppression is installed when a delete BEGINS (suppressRootAgent), so
		// this covers a completed delete — which also durably removed the
		// root_agents opt-in, meaning re-registering alone leaves the root on
		// the built-in default — and a delete that failed partway or targeted
		// an already-unknown project, where the config may still be intact.
		// Say what is certain (the suppression and its trigger) and hedge the
		// rest (#3264 review).
		return "its project was deleted this daemon run (a completed delete also removes the root_agents opt-in); confirm the project's registration and root-agent enable are as intended, then restart the daemon"
	case rootAgentNotConfigured:
		return "no root agent is configured for this repo — add a root_agents entry or a registered project's [root_agent], then restart the daemon"
	case rootAgentRecordsUnreadable:
		return fmt.Sprintf("no readable root-agent config covers this repo, and %d project registry record(s) (%s) cannot be read — one of them may be this repo's; repair or remove those record directories, then restart the daemon", len(v.unreadableRecords), strings.Join(v.unreadableRecords, ", "))
	case rootAgentAttributionPending:
		return "identity verification for its recorded project root is in progress — the daemon is confirming the checkout's registry marker on its ensure cadence; retry shortly"
	case rootAgentProjectUnresolved:
		// The cause clause is shared verbatim with the snapshot-derived shapes;
		// only the refusal clause distinguishes a live create-boundary refusal
		// (#3714), which is empty for every other caller.
		return rootProjectUnresolvedDetail(v) + rootCreateRefusalClause(v.createRefusedAt)
	case rootAgentDisabled:
		// A disable on a project whose recorded root is also unresolvable needs
		// both remedies: enabling alone leaves a root the restarted daemon
		// still cannot create (#3304 review).
		pathClause := ""
		switch {
		case v.rootPathVanished:
			pathClause = " — and its recorded project root vanished while its identity was being verified, so bring that path back before the restart too"
		case v.rootMarkerUnreadable:
			pathClause = " — and the checkout marker at its recorded project root cannot be read or holds an invalid id, so repair that marker before the restart too"
		case v.rootIdentityMismatch:
			pathClause = fmt.Sprintf(" — and the checkout at its recorded project root does not carry the project's registry marker (a different clone may occupy the path), so run `af projects rebind %s <path>` before the restart too if that checkout replaces the original", v.projectID)
		case v.rootUnresolved:
			pathClause = " — and its recorded project root does not currently resolve to a git repository, so bring that path back before the restart too"
		}
		if v.enabledSource == config.RootAgentSourceBuiltIn || v.enabledSource == "" {
			// No layer enabled it: a registered project with no root-agent
			// config anywhere defaults to disabled. There is no enabled=false
			// to point at, so do not invent one.
			return "no root_agent layer enables this repo (registered projects default to disabled); enable it in the project's personal [root_agent] or the global [root_agent], or add a root_agents entry, then restart the daemon" + pathClause
		}
		return fmt.Sprintf("its root agent resolves to disabled — an explicit enabled=false in the %s layer wins; enable it and restart the daemon%s", v.enabledSource, pathClause)
	case rootAgentRegistryUnreadable:
		registry := config.ProjectRegistryDirName
		if dir, err := config.ProjectRegistryDir(); err == nil {
			registry = dir
		}
		return fmt.Sprintf("the project registry %s could not be listed at daemon start, so af fails every root agent closed rather than start one a personal config may disable; repair the registry — the daemon re-checks it on its ensure cadence", registry)
	case rootAgentLegacyProbeUnanswered:
		return "could not establish whether a legacy root agent is configured for this repository: git never answered the probe (the subprocess was killed, could not be started, or was abandoned mid-read), so af refuses rather than report a repo whose root_agents entry it could not read as unconfigured; retry — the next request re-probes"
	case rootAgentPersonalUnreadable:
		path := "its personal project config"
		if v.projectID != "" {
			if p, err := config.ProjectConfigTomlPath(v.projectID); err == nil {
				path = p
			}
		}
		return fmt.Sprintf("its personal config %s cannot be loaded (read, parsed, or validated), so af fails closed rather than ignore a disable it cannot read; repair or remove it — the daemon re-checks it on its ensure cadence", path)
	}
	return ""
}

// rootProjectUnresolvedDetail is the cause half of a rootAgentProjectUnresolved
// message: the three narrowing flags prescribe three different remedies, and a
// verdict reaching it from the boot snapshot and one reaching it from a
// create-boundary refusal (#3714) must read identically, so this is the one
// place either is worded.
func rootProjectUnresolvedDetail(v rootAgentMaterializeVerdict) string {
	if v.rootPathVanished {
		// Source-agnostic on purpose: an unknowable identity blocks the
		// root whatever layer enables it.
		return "its recorded project root vanished while its identity was being verified; bring the path back — the daemon re-checks and re-verifies it on its ensure cadence"
	}
	if v.rootMarkerUnreadable {
		// Identity is unknowable, not disproven: no rebind advice — a
		// transiently unreadable ORIGINAL checkout rebound over would be
		// data loss.
		return "the checkout marker at its recorded project root cannot be read or holds an invalid id (permissions, I/O, or corruption), so the checkout's identity cannot be verified and no root agent will start for this repo; repair the marker — the daemon re-checks it on its ensure cadence"
	}
	if v.rootIdentityMismatch {
		// The path is PRESENT — "bring it back" is an impossible remedy.
		// What blocks the root is identity: the checkout there does not
		// carry the project's registry marker.
		return fmt.Sprintf("its root agent resolves to enabled (from the %s layer), but the checkout at the recorded project root does not carry the project's registry marker — a different clone may occupy the path; run `af projects rebind %s <path>` if that checkout replaces the original, then restart the daemon", v.enabledSource, v.projectID)
	}
	return fmt.Sprintf("its root agent resolves to enabled (from the %s layer), but the recorded project root does not currently resolve to a git repository; bring the path back — the daemon re-checks it on its ensure cadence and resumes the project without a restart", v.enabledSource)
}
