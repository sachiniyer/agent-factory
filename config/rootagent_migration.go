package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
)

// legacyRootAgentsWarned keeps the permanent compatibility adapter from
// flooding a long-lived daemon. The source path is the identity: one file gets
// one actionable migration notice per process, however often config is loaded.
var legacyRootAgentsWarned sync.Map

// legacyRootAgentsAdvice is the migration recipe both root_agents notices open
// with. Only their tail differs: what happened to the file.
//
// Its recipe IS legacyRootAgentsManualStep, spliced in rather than restated.
// The two were separate strings for one commit and immediately drifted — the
// review caught the migrate report omitting the "remove the entry" step, and
// fixing only that would have left the WARNING telling readers to do work that
// does not silence it. Sharing the literal is what makes that class impossible
// rather than merely fixed once (#3624 review).
const legacyRootAgentsAdvice = "root_agents is the legacy path map; use [root_agent], the current project profile, for new configuration; for exact per-path equivalence, " + legacyRootAgentsManualStep

// legacyRootAgentsInShape reports whether shape carries a configured legacy
// path map. An empty table is not one, so the presence test and the migration's
// are the same test.
func legacyRootAgentsInShape(shape map[string]any) bool {
	_, ok := legacyRootAgentsPaths(shape)
	return ok
}

// legacyRootAgentsPaths is this deprecation's presence test for the shared
// table: the configured legacy paths, sorted, or false when the map is absent
// or empty. The warning and `af config migrate` both read it, so neither can
// find a legacy entry the other does not.
func legacyRootAgentsPaths(shape map[string]any) ([]string, bool) {
	rootAgents, ok := shape[LegacyRootAgentsKey].(map[string]any)
	if !ok || len(rootAgents) == 0 {
		return nil, false
	}
	paths := make([]string, 0, len(rootAgents))
	for path := range rootAgents {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, true
}

// legacyRootAgentsDeprecation is this key's entry in the shared deprecation
// table, which owns the remedy clause the warning ends with (#3624).
func legacyRootAgentsDeprecation() configDeprecation {
	for _, deprecation := range configDeprecations() {
		if deprecation.key == LegacyRootAgentsKey {
			return deprecation
		}
	}
	// Unreachable while configDeprecations lists the key, and the table's
	// coverage test pins that. Degrade to the bare advice rather than panic in
	// a config loader.
	return configDeprecation{key: LegacyRootAgentsKey}
}

func warnLegacyRootAgents(shape map[string]any, prettyPath string) {
	if !legacyRootAgentsInShape(shape) {
		return
	}
	if _, seen := legacyRootAgentsWarned.LoadOrStore(prettyPath, struct{}{}); seen {
		return
	}
	// The tail used to read "no file was rewritten", which named the obstruction
	// and no remedy. It now names the verb AND why that verb stops short of this
	// one key, so a reader who runs it is not surprised (#3624).
	log.WarningLog.Printf("config %s: %s; %s", prettyPath, legacyRootAgentsAdvice, legacyRootAgentsDeprecation().tomlRemedy(false))
}

func warnConvertedLegacyRootAgents(rootAgents map[string]RootAgentConfig, legacyPath, tomlPath string) {
	if len(rootAgents) == 0 {
		return
	}
	// Key the notice by the canonical TOML path. The immediate reparse and every
	// later load in this process then suppress the generic no-write wording.
	if _, seen := legacyRootAgentsWarned.LoadOrStore(tomlPath, struct{}{}); seen {
		return
	}
	log.WarningLog.Printf("config %s: %s; conversion wrote %s and preserved every legacy entry", legacyPath, legacyRootAgentsAdvice, tomlPath)
}

func resetLegacyRootAgentsWarnings() {
	legacyRootAgentsWarned.Clear()
}

// ── migrating entries whose path is already a registered project (#6957) ──
//
// A root_agents key names a path. When that path resolves to a registered
// project the entry has a current-spelling successor — the project's personal
// [root_agent] — and `af config migrate` performs the move itself: it writes
// the leaves the project's effective profile needs (an empty entry means
// enabled, plus an optional program), only when the project's own profile does
// not already produce the identical result, and drops the legacy entry. The
// effective profile the daemon resolves is the assertion, not the hope:
// ResolveRootAgent is run over the layers before and after, and an entry whose
// post-write resolution would differ is left in place, not moved.
//
// Everything the daemon itself uses to attribute an entry — the repository
// probe, the checkout-marker project lookup, the same recorded-root and
// path-hash fallbacks — is what classifies a key here, so an entry the daemon
// would serve is one this migration recognizes. And everything that cannot be
// proven — a probe that never answered, a registry that cannot be read, a
// personal config that will not load — is reported with its cause and left
// exactly where it is: an ambiguous entry is never guessed at.

// migrateRootAgentsProbeBudget bounds the repository probes, checkout-marker
// reads, and registry reads one migrate run spends on classification. A lapsed
// budget does not become a guess; the undecided entries are reported.
const migrateRootAgentsProbeBudget = 30 * time.Second

// migrateRootAgentsAmbiguousStep is the reported step for entries that could
// not be proven safe to move. Unregistered paths keep the manual recipe
// (legacyRootAgentsManualStep); these name a cause to fix and a re-run.
const migrateRootAgentsAmbiguousStep = "could not be migrated safely — resolve the cause named beside each entry, then run `af config migrate` again"

// errMovedCheckoutAmbiguous is returned by verifyTwoIdentitySafety when a
// moved-but-unrebound project's two daemon identities would not both keep
// their pre-migration effective profile. It is NOT a write failure: the check
// runs before any personal write, so the caller leaves the legacy keys in
// place and reports the cause under migrateRootAgentsAmbiguousStep.
var errMovedCheckoutAmbiguous = errors.New("project is recorded at a moved-away path and the daemon serves it under two repo identities until it is rebound")

// legacyMigrateGroup is every root_agents key proven to name ONE registered
// project, plus the identity the daemon uses to find the entry that applies —
// the resolved repository identity, or the project's recorded root for a path
// that cannot resolve at all.
type legacyMigrateGroup struct {
	project Project
	repoID  string
	keys    []string
	// recordedRootKey reports that a key spelling the project's recorded
	// (now-determinately-free) root was merged into this resolving group via
	// findResolvingGroupByProjectIdentity — the moved-but-unrebound state: a
	// checkout moved (its marker traveled, so the moved key resolves under a NEW
	// repo identity) while the record still points at the moved-away path. The
	// daemon then serves the project under TWO identities — the group's repoID
	// (the moved checkout) and the project's recorded identity (the durable
	// RepoID the snapshot keys the personal layer and the unresolved record
	// under). The group carries only the moved identity, so a one-identity
	// check would silently flip the recorded identity's profile and leave the
	// moved identity with no applying entry; a group carrying this flag must be
	// verified under both before any write (verifyTwoIdentitySafety).
	recordedRootKey bool
}

// lookupLegacyEntry answers the question a migrated group must agree with the
// daemon on: which root_agents entry applies to this project — over the file
// as it stands, or over the post-write model that drops the migrated keys.
func (g *legacyMigrateGroup) lookupLegacyEntry(ctx context.Context, cfg *Config) (*RootAgentConfig, string, error) {
	if g.repoID != "" {
		return LegacyRootAgentForRepoContext(ctx, cfg, g.repoID)
	}
	return LegacyRootAgentForRecordedRootContext(ctx, cfg, g.project.Root)
}

// spansTwoDaemonIdentities reports whether this group's keys are served by the
// daemon under two distinct repo identities — the moved-but-unrebound state a
// recordedRootKey merge creates. The group's repoID is the moved checkout's
// identity (the key resolved); the project's recorded identity
// (ReconciledRepoIDForProject) is the durable RepoID the daemon keys the
// personal layer and the unresolved record under while the recorded root does
// not resolve. They differ exactly when the checkout moved to a new identity
// and the record has not yet been rebound; a same-identity move (the recorded
// root's hash equals repoID, or an un-rebased record whose recorded root still
// resolves to repoID) keeps one identity and is not a two-identity case.
func (g *legacyMigrateGroup) spansTwoDaemonIdentities() bool {
	return g.recordedRootKey && g.repoID != "" && ReconciledRepoIDForProject(g.project) != g.repoID
}

// legacyEntryForRecordedIdentity is the legacy root_agents entry the daemon
// applies under the project's RECORDED identity — the same lookup
// rootAgentMaterializeVerdictFor runs for an unresolved project: first
// LegacyRootAgentForRepoContext (whose path-hash fallback reaches a key that
// spells the recorded root once that root no longer resolves), then the
// recorded-root leg as a last resort. nil means no entry applies there.
func (g *legacyMigrateGroup) legacyEntryForRecordedIdentity(ctx context.Context, cfg *Config) (*RootAgentConfig, error) {
	recordedID := ReconciledRepoIDForProject(g.project)
	entry, _, err := LegacyRootAgentForRepoContext(ctx, cfg, recordedID)
	if err != nil {
		return nil, err
	}
	if entry != nil {
		return entry, nil
	}
	entry, _, err = LegacyRootAgentForRecordedRootContext(ctx, cfg, g.project.Root)
	return entry, err
}

// postWriteLayer models the personal [root_agent] layer after the migration's
// patch lands — the fields `before` needed that `dropped` would lose are set,
// everything else from layer is preserved — without committing the write. It
// is the same merge rootAgentMigratePatch + scalarWrite applyProject performs,
// computed in memory so verifyTwoIdentitySafety can evaluate the post-write
// profile under the recorded identity before any file is touched.
func postWriteLayer(layer *RootAgentLayer, before, dropped RootAgent) *RootAgentLayer {
	patch := rootAgentMigratePatch(before, dropped)
	if len(patch) == 0 {
		return layer
	}
	var updated RootAgentLayer
	if layer != nil {
		updated = *layer
	}
	if _, ok := patch["enabled"]; ok {
		updated.EnabledSet = true
		updated.Value.Enabled = before.Enabled
	}
	if _, ok := patch["program"]; ok {
		updated.Value.Program = before.Program
	}
	return &updated
}

// verifyTwoIdentitySafety checks the migration's invariant — a project's
// effective root-agent profile is never silently altered — under BOTH repo
// identities a moved-but-unrebound project is served by, since the group's
// one-identity check (before/dropped over the moved checkout) cannot. It runs
// inside the personal file lock, before any write, using the freshly-read
// layer, so the answer is ordered against the write it gates.
//
// Recorded identity: the personal layer this write lands in is keyed here, so
// the recorded identity's post-write profile is the modeled post-write layer
// (with the legacy entries gone). It must equal the recorded identity's
// pre-migration profile — its own legacy entry (the stale recorded-root key,
// reached by the path-hash fallback) plus the personal layer — or the
// migration silently flips which program the recorded identity resolves to.
//
// Moved identity: no personal layer is keyed here while the recorded root is
// unresolved, so removing the legacy entry drops it to whatever the global
// layer alone produces. The moved winner's pre-migration profile must already
// equal that, or the moved checkout — the one the user actually works in —
// silently loses its root agent ("not configured") after the next daemon
// start.
//
// Returns nil when both identities keep their profile, errMovedCheckoutAmbiguous
// when either would change, and any lookup error otherwise.
func (g *legacyMigrateGroup) verifyTwoIdentitySafety(
	ctx context.Context,
	globalLayer *RootAgentLayer,
	cfg, postCfg *Config,
	layer *RootAgentLayer,
	winner, remainingEntry *RootAgentConfig,
	before, dropped RootAgent,
) error {
	if !g.spansTwoDaemonIdentities() {
		return nil
	}
	recordedLegacyPre, err := g.legacyEntryForRecordedIdentity(ctx, cfg)
	if err != nil {
		return fmt.Errorf("the recorded identity's legacy entry could not be established: %w", err)
	}
	recordedPre := ResolveRootAgent(RootAgentInputs{Global: globalLayer, Legacy: recordedLegacyPre, Personal: layer}).RootAgent
	recordedLegacyPost, err := g.legacyEntryForRecordedIdentity(ctx, postCfg)
	if err != nil {
		return fmt.Errorf("the recorded identity's post-removal legacy entry could not be established: %w", err)
	}
	recordedPost := ResolveRootAgent(RootAgentInputs{Global: globalLayer, Legacy: recordedLegacyPost, Personal: postWriteLayer(layer, before, dropped)}).RootAgent
	if recordedPre != recordedPost {
		return errMovedCheckoutAmbiguous
	}
	movedPre := ResolveRootAgent(RootAgentInputs{Global: globalLayer, Legacy: winner, Personal: nil}).RootAgent
	movedPost := ResolveRootAgent(RootAgentInputs{Global: globalLayer, Legacy: remainingEntry, Personal: nil}).RootAgent
	if movedPre != movedPost {
		return errMovedCheckoutAmbiguous
	}
	return nil
}

// nonResolvingLegacyKey is a root_agents key whose path did not resolve to a
// repository, retained with its expanded spelling and the probe's outcome so
// the two fallback legs can still prove it — the same evidence the daemon's
// own lookup gives them.
type nonResolvingLegacyKey struct {
	key      string
	cleaned  string
	probeErr error
}

// findResolvingGroupByProjectIdentity returns the resolving group whose project
// is the same one as spelled, if one exists. After a checkout move, a project
// can have both a resolving key (the moved checkout, found by the checkout
// marker) and a non-resolving key (the stale recorded root, found by the
// recorded-root leg). The two keys hash to different repo IDs, so the
// hash-merge leg cannot join them; matching by project ID does, keeping one
// group per project so the migration carries the daemon's actual winner once.
func findResolvingGroupByProjectIdentity(resolving map[string]*legacyMigrateGroup, spelled *Project) *legacyMigrateGroup {
	if spelled == nil {
		return nil
	}
	for _, g := range resolving {
		if g.project.ID == spelled.ID {
			return g
		}
	}
	return nil
}

// classifyLegacyRootAgents sorts every configured root_agents path into the
// three states the migration treats differently: groups it can move
// (registered-project membership is proven), paths it cannot move because no
// registered project answers for them (the manual recipe still applies), and
// paths whose answer could not be established (named with the cause, left).
func classifyLegacyRootAgents(ctx context.Context, paths []string) (groups []*legacyMigrateGroup, unregistered, ambiguous []string) {
	resolving := map[string]*legacyMigrateGroup{}
	var nonResolving []nonResolvingLegacyKey
	for _, key := range paths {
		expanded := ExpandTilde(key)
		repo, probeErr := RepoFromPathContext(ctx, expanded)
		if probeErr == nil {
			project, found, err := projectForRepoContext(ctx, repo)
			switch {
			case err != nil:
				ambiguous = append(ambiguous, fmt.Sprintf("%s — whether it names a registered project could not be established: %s", key, err))
			case !found:
				unregistered = append(unregistered, key)
			default:
				g := resolving[repo.ID]
				if g == nil {
					g = &legacyMigrateGroup{project: project, repoID: repo.ID}
					resolving[repo.ID] = g
				}
				g.keys = append(g.keys, key)
			}
			continue
		}
		nonResolving = append(nonResolving, nonResolvingLegacyKey{key: key, cleaned: filepath.Clean(expanded), probeErr: probeErr})
	}

	if len(nonResolving) == 0 {
		for _, g := range resolving {
			groups = append(groups, g)
		}
		sort.Slice(groups, func(i, j int) bool { return groups[i].project.ID < groups[j].project.ID })
		return groups, unregistered, ambiguous
	}

	// A key that does not resolve can still name a registered project, and the
	// daemon knows two ways: its cleaned spelling may hash to a repository's
	// recorded identity (LegacyRootAgentForRepoContext's fallback), or it may
	// spell a registered project's recorded root while that root is provably
	// free (LegacyRootAgentForRecordedRoot). An answer neither leg can prove is
	// "unregistered" only when git ANSWERED that the path is not a repository —
	// an unanswered probe is ambiguity, not evidence of absence.
	var projects []Project
	var projectsErr error
	projectsListed := false
	for _, pending := range nonResolving {
		var hashed *legacyMigrateGroup
		for _, g := range resolving {
			if g.repoID == RepoIDFromRoot(pending.cleaned) {
				if hashed != nil {
					ambiguous = append(ambiguous, pending.key+" — its path-derived identity matches more than one repository")
					hashed = nil
					break
				}
				hashed = g
			}
		}
		if hashed != nil {
			hashed.keys = append(hashed.keys, pending.key)
			continue
		}
		if !projectsListed {
			projects, projectsErr = listProjectsWithoutRootProbes()
			projectsListed = true
		}
		var spelled *Project
		if projectsErr == nil {
			resolvedSpelling := pathutil.ResolveForCompare(pending.cleaned)
			for i := range projects {
				if projects[i].Root == "" || pathutil.ResolveForCompare(filepath.Clean(projects[i].Root)) != resolvedSpelling {
					continue
				}
				if spelled != nil {
					ambiguous = append(ambiguous, pending.key+" — its spelling matches the recorded root of more than one registered project")
					spelled = nil
					break
				}
				spelled = &projects[i]
			}
		}
		switch {
		case projectsErr != nil:
			ambiguous = append(ambiguous, fmt.Sprintf("%s — the project registry could not be read, so no recorded-root match could be proven: %s", pending.key, projectsErr))
		case spelled == nil:
			if RepoProbeUnanswered(pending.probeErr) {
				ambiguous = append(ambiguous, RepoProbeUnansweredClaim("the root_agents entry for path", pending.key))
			} else {
				unregistered = append(unregistered, pending.key)
			}
		case !PathIsDeterminatelyFree(pending.cleaned, pending.probeErr):
			ambiguous = append(ambiguous, fmt.Sprintf("%s — it spells the recorded root of registered project %s, but whether that path still holds the checkout could not be proven: %s", pending.key, spelled.ID, pending.probeErr))
		default:
			if g := findResolvingGroupByProjectIdentity(resolving, spelled); g != nil {
				g.keys = append(g.keys, pending.key)
				g.recordedRootKey = true
			} else {
				groups = append(groups, &legacyMigrateGroup{project: *spelled, keys: []string{pending.key}})
			}
		}
	}
	for _, g := range resolving {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].project.ID != groups[j].project.ID {
			return groups[i].project.ID < groups[j].project.ID
		}
		return groups[i].keys[0] < groups[j].keys[0]
	})
	return groups, unregistered, ambiguous
}

// loadPersonalLayerForMigrate reads the project's personal [root_agent] layer
// for the migration, deliberately NOT through LoadProjectConfig: that loader
// resolves the global branch_prefix for its once-per-file warning, a read that
// would take the very config lock the caller already holds.
func loadPersonalLayerForMigrate(id string) (*RootAgentLayer, error) {
	path, err := ProjectConfigTomlPath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read personal project config %s: %w", prettyHomePath(path), err)
	}
	pc, err := parseProjectConfig(data, path)
	if err != nil {
		return nil, err
	}
	return pc.RootAgentLayer(), nil
}

// rootAgentMigratePatch is the sparse [root_agent] write that makes the
// project's personal profile produce `before`: exactly the leaves the
// post-removal resolution `dropped` would lose. An empty map means the
// personal layer already reproduces the profile and the legacy entry can be
// dropped with no write at all.
func rootAgentMigratePatch(before, dropped RootAgent) map[string]any {
	patch := make(map[string]any, 2)
	if dropped.Enabled != before.Enabled {
		patch["enabled"] = before.Enabled
	}
	if dropped.Program != before.Program {
		patch["program"] = before.Program
	}
	return patch
}

// migrateLegacyRootAgents performs the #6957 half of the migration over the
// config text. It returns the updated content, the per-entry report, the
// entries left in place (unregistered paths under the manual step, ambiguous
// ones under their cause), and the removed map keys for the drift guard's
// exemption. Personal [root_agent] writes land under the projects' own file
// locks BEFORE the global file is committed: a failure between them leaves a
// consistent, resumable state — the legacy entry still present beside a
// profile that already covers it — rather than a project whose root agent was
// silently dropped.
func migrateLegacyRootAgents(ctx context.Context, content string, cfg *Config, shape map[string]any) (string, []MigratedRootAgent, []UnmigratedKey, []string, error) {
	paths, ok := legacyRootAgentsPaths(shape)
	if !ok {
		return content, nil, nil, nil, nil
	}
	groups, unregistered, ambiguous := classifyLegacyRootAgents(ctx, paths)
	globalLayer := GlobalRootAgentLayer(cfg)

	type groupPlan struct {
		group        *legacyMigrateGroup
		winner       *RootAgentConfig
		personalPath string
	}
	var plans []*groupPlan
	leftGroup := func(g *legacyMigrateGroup, reason string) {
		for _, key := range g.keys {
			ambiguous = append(ambiguous, key+" — "+reason)
		}
	}
	for _, g := range groups {
		winner, _, err := g.lookupLegacyEntry(ctx, cfg)
		switch {
		case err != nil:
			leftGroup(g, "the legacy entry the daemon applies could not be established: "+err.Error())
			continue
		case winner == nil:
			leftGroup(g, "no legacy entry the daemon would apply could be proven for it")
			continue
		}
		if _, err := loadPersonalLayerForMigrate(g.project.ID); err != nil {
			leftGroup(g, "its personal config could not be read: "+err.Error())
			continue
		}
		personalPath, err := ProjectConfigTomlPath(g.project.ID)
		if err != nil {
			leftGroup(g, "its personal config path could not be resolved: "+err.Error())
			continue
		}
		plans = append(plans, &groupPlan{
			group:        g,
			winner:       winner,
			personalPath: personalPath,
		})
	}
	if len(plans) == 0 {
		return content, nil, legacyRootAgentsLeft(unregistered, ambiguous), nil, nil
	}

	// Learn which spellings the file will actually give up before deciding
	// anything downstream of them — the post-write model below must count an
	// entry the edit cannot remove as still present.
	wanted := map[string]bool{}
	for _, p := range plans {
		for _, key := range p.group.keys {
			wanted[key] = true
		}
	}
	_, removable, err := removeTOMLRootAgentsEntries(content, wanted)
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("internal error: could not edit root_agents in the decoded config: %w", err)
	}
	planned := map[string]bool{}
	for _, p := range plans {
		kept := p.group.keys[:0]
		for _, key := range p.group.keys {
			if removable[key] {
				kept = append(kept, key)
				planned[key] = true
				continue
			}
			ambiguous = append(ambiguous, key+" — the rewrite could not find its entry in the file; remove its root_agents line by hand")
		}
		p.group.keys = kept
	}

	// The post-write model: the config as the daemon will see it once the
	// planned entries are gone. Every remaining-entry lookup below asks this
	// model the same question the daemon's own lookup asks of the live file.
	postCfg := *cfg
	postCfg.RootAgents = make(map[string]RootAgentConfig, len(cfg.RootAgents))
	for key, entry := range cfg.RootAgents {
		if !planned[key] {
			postCfg.RootAgents[key] = entry
		}
	}

	var migrated []MigratedRootAgent
	for _, p := range plans {
		if len(p.group.keys) == 0 {
			continue
		}
		remainingEntry, _, err := p.group.lookupLegacyEntry(ctx, &postCfg)
		if err != nil {
			leftGroup(p.group, "whether another spelling would still apply after the move could not be established: "+err.Error())
			continue
		}
		// Everything that depends on the personal file's CONTENTS is decided
		// under its lock: the layer is re-read inside it, the patch computed
		// from that read, the write applied and the identity re-verified. A
		// patch computed outside the lock can overwrite a preference that
		// landed between the read and the write (Codex on #6957).
		var before RootAgent
		var personalWritten bool
		writeErr := WithFileLock(p.personalPath, func() error {
			layer, err := loadPersonalLayerForMigrate(p.group.project.ID)
			if err != nil {
				return err
			}
			before = ResolveRootAgent(RootAgentInputs{Global: globalLayer, Legacy: p.winner, Personal: layer}).RootAgent
			dropped := ResolveRootAgent(RootAgentInputs{Global: globalLayer, Legacy: remainingEntry, Personal: layer}).RootAgent
			// A moved-but-unrebound project is served by the daemon under two
			// repo identities; the before/dropped check above models only the
			// group's repoID (the moved checkout). The personal write lands under
			// the RECORDED identity, so it would silently adopt the moved
			// winner's profile there, and the moved identity keys no personal
			// layer so it would drop to "not configured" once its legacy entry
			// is removed. Verify both identities keep their pre-migration
			// profile before touching the file, or leave the keys in place.
			if err := p.group.verifyTwoIdentitySafety(ctx, globalLayer, cfg, &postCfg, layer, p.winner, remainingEntry, before, dropped); err != nil {
				return err
			}
			patch := rootAgentMigratePatch(before, dropped)
			if len(patch) == 0 {
				return nil
			}
			raw, err := json.Marshal(patch)
			if err != nil {
				return fmt.Errorf("internal error: could not encode the personal root_agent patch: %w", err)
			}
			write := scalarWrite{key: "root_agent", structured: true, rawStructured: string(raw)}
			if _, err := write.applyProject(p.personalPath, prettyHomePath(p.personalPath)); err != nil {
				return err
			}
			personalWritten = true
			// The write is only half the claim — the assertion is that the
			// daemon's next resolution lands on the identical profile. A
			// mismatch leaves the entries in place; the landed leaves only
			// ever carry the effective values, so the kept state stays
			// consistent either way.
			updated, err := loadPersonalLayerForMigrate(p.group.project.ID)
			if err != nil {
				return fmt.Errorf("the personal [root_agent] write could not be read back: %w", err)
			}
			if after := ResolveRootAgent(RootAgentInputs{Global: globalLayer, Legacy: remainingEntry, Personal: updated}).RootAgent; after != before {
				return errors.New("the personal write did not reproduce the effective profile")
			}
			return nil
		})
		if writeErr != nil {
			if errors.Is(writeErr, errMovedCheckoutAmbiguous) {
				leftGroup(p.group, "project "+p.group.project.ID+" is recorded at the moved-away path "+p.group.project.Root+"; the daemon serves it under two repo identities until it is rebound — run `af projects rebind "+p.group.project.ID+" <moved-checkout-path>`, then re-run `af config migrate`")
			} else {
				leftGroup(p.group, "its personal [root_agent] could not be updated safely: "+writeErr.Error())
			}
			continue
		}
		for _, key := range p.group.keys {
			migrated = append(migrated, MigratedRootAgent{
				Path:            key,
				ProjectID:       p.group.project.ID,
				PersonalConfig:  p.personalPath,
				PersonalWritten: personalWritten,
				Profile:         before,
			})
		}
	}
	if len(migrated) == 0 {
		return content, nil, legacyRootAgentsLeft(unregistered, ambiguous), nil, nil
	}

	// Commit the removals for the entries that were actually migrated. A
	// spelling the dry run found but the real edit cannot remove is reported,
	// not claimed: its entry stays in the file, and the reason says so.
	remove := make(map[string]bool, len(migrated))
	for _, m := range migrated {
		remove[m.Path] = true
	}
	updated, removed, err := removeTOMLRootAgentsEntries(content, remove)
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("internal error: could not remove the migrated root_agents entries: %w", err)
	}
	content = updated
	var kept []MigratedRootAgent
	var exempt []string
	for _, m := range migrated {
		if !removed[m.Path] {
			ambiguous = append(ambiguous, m.Path+" — the rewrite could not remove its entry; its legacy leaves may still apply — remove its root_agents line by hand")
			continue
		}
		kept = append(kept, m)
		exempt = append(exempt, LegacyRootAgentsKey+"."+m.Path)
	}
	return content, kept, legacyRootAgentsLeft(unregistered, ambiguous), exempt, nil
}

// legacyRootAgentsLeft assembles the Left report for a root_agents pass:
// unregistered paths under the shared manual recipe, ambiguous ones under the
// step that names why they were not guessed at.
func legacyRootAgentsLeft(unregistered, ambiguous []string) []UnmigratedKey {
	var left []UnmigratedKey
	if len(unregistered) > 0 {
		sort.Strings(unregistered)
		left = append(left, UnmigratedKey{Key: LegacyRootAgentsKey, Step: legacyRootAgentsManualStep, Detail: unregistered})
	}
	if len(ambiguous) > 0 {
		sort.Strings(ambiguous)
		left = append(left, UnmigratedKey{Key: LegacyRootAgentsKey, Why: "these root_agents entries could not be proven safe to move", Step: migrateRootAgentsAmbiguousStep, Detail: ambiguous})
	}
	return left
}
