package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// This file backs `af config get root_agent --explain` (#2216 Phase 6). The
// generic manifest resolver only knows root_agent's two singleton layers
// (global < personal), but the daemon resolves FOUR — built-in < global <
// legacy root_agents < personal-project — through config.ResolveRootAgent. A
// two-layer explanation of a four-layer decision is confidently wrong: a user
// debugging why a root agent is or is not running would be shown a precedence
// model that did not decide it. ResolveRootAgentForInspection produces the full
// four-layer trace in the same ResolvedValue shape every other key renders, so
// --explain describes what the daemon actually does.

// rootAgentLocations records, per root-agent source, the on-disk file and the
// key-within-file that supplied (or would supply) that layer, for the --explain
// LOCATION column. The built-in layer has no file.
type rootAgentLocations struct {
	// globalPath is the global config file; both root_agent and the legacy
	// root_agents map live there.
	globalPath string
	// personalPath is the registered project's personal config file, "" when the
	// project has no personal [root_agent].
	personalPath string
	// legacyKey is the matched root_agents map key, "" when no entry resolves to
	// the inspected repo.
	legacyKey string
}

func (locs rootAgentLocations) forSource(source RootAgentSource) (path, keyPath string) {
	switch source {
	case RootAgentSourceGlobal:
		return locs.globalPath, "root_agent"
	case RootAgentSourceLegacy:
		if locs.legacyKey == "" {
			return locs.globalPath, "root_agents"
		}
		return locs.globalPath, "root_agents[" + strconv.Quote(locs.legacyKey) + "]"
	case RootAgentSourcePersonal:
		return locs.personalPath, "root_agent"
	default: // built-in has no on-disk location
		return "", "root_agent"
	}
}

// ResolveRootAgentForInspection resolves the on-disk root-agent profile for the
// `af config get root_agent --explain` surface and returns it as a ResolvedValue
// whose trace names all FOUR layers the daemon resolves — built-in < global <
// legacy root_agents < personal-project — with a per-field origin (Enabled and
// Program can come from different layers). It mirrors the daemon's
// rootAgentInputsFor, so --explain describes the decision the daemon makes at its
// NEXT start; it reads on-disk config, never the running daemon.
//
// Global scope (an empty selector) has no project to key the legacy and personal
// layers by, so those layers resolve as absent. The command normally supplies
// the cwd's repository and reaches global scope only outside git.
func ResolveRootAgentForInspection(projectSelector string, strictProjectLookup bool) (ResolvedValue, error) {
	assembly, err := assembleRootAgentInspectionInputs(projectSelector, strictProjectLookup)
	if err != nil {
		return ResolvedValue{}, err
	}
	if assembly.failClosed != "" {
		return rootAgentFailClosedValue(assembly), nil
	}
	resolved := rootAgentResolvedValue(ResolveRootAgent(assembly.inputs), assembly.locs, projectSelector != "")
	if assembly.ignoredGlobal != nil {
		markRootAgentGlobalIneligible(&resolved, *assembly.ignoredGlobal, assembly.ignoredReason, assembly.locs)
	}
	return resolved, nil
}

// rootAgentInspectionAssembly is what assembleRootAgentInspectionInputs hands
// the renderer: the layer inputs and their locations, plus at most one of two
// degraded verdicts — an ignored global layer (the project is simply not
// registered), or a fail-closed cause that overrides the layers entirely.
type rootAgentInspectionAssembly struct {
	inputs RootAgentInputs
	locs   rootAgentLocations
	// ignoredGlobal/ignoredReason render the global layer as present but
	// inapplicable, without failing the trace.
	ignoredGlobal *RootAgentCandidate
	ignoredReason string
	// failClosed, when non-empty, is the cause the daemon fails this repo's
	// root agent closed with at its next start: the registry cannot be listed
	// (#3247) or the personal config cannot be loaded (#3241). The explain
	// must then mirror the daemon — disabled, with every present layer shown
	// ignored for this reason — because this surface is documented as "the
	// decision the daemon makes at its NEXT start", and rendering the layer
	// merge would confidently explain a decision the failed read already
	// overrode (#3264).
	failClosed string
	// personalUnreadable narrows failClosed: the cause is THIS project's
	// personal config, so the trace must render the personal layer as present
	// but unreadable — its file exists at locs.personalPath — rather than as
	// "this project has no entry", which would deny the very source the cause
	// names.
	personalUnreadable bool
}

// assembleRootAgentInspectionInputs builds the RootAgentInputs the --explain
// surface resolves, from on-disk config: the global [root_agent] layer, and —
// when a project is selected — the legacy root_agents entry and personal
// [root_agent] that resolve to its repo, with each layer's source path for the
// LOCATION column.
//
// It deliberately DUPLICATES the daemon's rootAgentInputsFor rather than sharing
// it, because the two must read from different sources: the daemon from its
// start-of-day snapshot, this from on-disk config (--explain describes the next
// start, not the running daemon). The shared half is ResolveRootAgent, so
// precedence and merge never diverge. The duplication has one hazard — a new
// RootAgentInputs layer added for the daemon but not assembled here would drop
// silently out of --explain, the exact wrong-trace bug this file fixes. That is
// why TestRootAgentInspectionInputsPopulateEveryLayer reflects over the assembled
// inputs and fails when any layer is left unset: keep this in step with
// RootAgentInputs and the guard, not with a comment.
func assembleRootAgentInspectionInputs(projectSelector string, strictProjectLookup bool) (rootAgentInspectionAssembly, error) {
	global, err := LoadConfig()
	if err != nil {
		return rootAgentInspectionAssembly{}, err
	}
	out := rootAgentInspectionAssembly{
		locs:   rootAgentLocations{globalPath: global.source.path},
		inputs: RootAgentInputs{Global: GlobalRootAgentLayer(global)},
	}
	if out.locs.globalPath == "" {
		if path, err := globalConfigTomlPath(); err == nil {
			out.locs.globalPath = path
		}
	}

	if projectSelector != "" {
		abs, err := ResolveUserPath(projectSelector)
		if err != nil {
			return rootAgentInspectionAssembly{}, fmt.Errorf("failed to resolve project path %q: %w", projectSelector, err)
		}
		repo, err := RepoFromPath(abs)
		if err != nil {
			return rootAgentInspectionAssembly{}, fmt.Errorf("failed to resolve project path %q: %w", projectSelector, err)
		}
		legacy, key := LegacyRootAgentForRepo(global, repo.ID)
		if legacy != nil {
			out.inputs.Legacy = legacy
			out.locs.legacyKey = key
		}
		project, found, err := projectForRoot(repo.Root)
		if err != nil {
			if strictProjectLookup {
				return rootAgentInspectionAssembly{}, err
			}
			// Fail CLOSED, like the daemon (#3247): an unlistable registry means
			// no repo can be proven un-disabled, legacy entries included. The
			// pre-#3247 rendering here — the legacy layer winning, or only the
			// global layer dropped — described the daemon's old fail-open
			// behavior and would misdirect the operator away from the registry.
			registry := ProjectRegistryDirName
			if dir, dirErr := ProjectRegistryDir(); dirErr == nil {
				registry = dir
			}
			out.failClosed = fmt.Sprintf("the project registry %s could not be listed, so the daemon fails every root agent closed at its next start (a personal config it cannot enumerate may hold enabled=false); repair the registry", registry)
			return out, nil
		}
		if found {
			pc, err := LoadProjectConfig(project.ID)
			if err != nil {
				// Fail CLOSED, like the daemon (#3241) — and explain rather than
				// error out: the decision at the next daemon start is known
				// (disabled), and the operator running --explain is exactly the
				// person who needs the cause and the file named (#3264).
				path := "its personal config"
				if p, pathErr := ProjectConfigTomlPath(project.ID); pathErr == nil {
					path = p
					out.locs.personalPath = p
				}
				out.failClosed = fmt.Sprintf("personal config %s exists but cannot be loaded, so the daemon fails this project's root agent closed at its next start; fix or remove that file", path)
				out.personalUnreadable = true
				return out, nil
			}
			if layer := pc.RootAgentLayer(); layer != nil {
				out.inputs.Personal = layer
				if path, err := ProjectConfigTomlPath(project.ID); err == nil {
					out.locs.personalPath = path
				}
			}
		} else if legacy == nil {
			out.ignoredGlobal = rootAgentCandidateForLayer(out.inputs, RootAgentSourceGlobal)
			out.ignoredReason = "project is not registered and has no legacy root_agents entry"
			out.inputs.Global = nil
			return out, nil
		}
	}

	return out, nil
}

// rootAgentFailClosedValue renders the daemon's fail-closed verdict (#3241,
// #3247): the effective profile is disabled regardless of what the layers say,
// every present layer is shown ignored with the cause, and no field claims a
// config origin — no config source decided this, the failed read did (the same
// zero-provenance rule as the daemon's resolvedRootAgentFor). Layers stay
// Allowed: manifest policy permits them everywhere they appear; it is the
// failed read that ignores them, and Result/Reason carry that. The
// zero-provenance shape doubles as the marker RootAgentValueFailsClosed keys
// on.
//
// The personal row needs its own truth per cause. When the personal config
// itself is the unreadable source, the layer is PRESENT — its file exists at
// the recorded location — just undecodable, so the row shows present with no
// leaves and the cause; rendering it "this project has no entry" would deny
// the very source the cause names. When the registry is the unreadable
// source, whether this project even has a personal entry is unknowable, and
// the row says that instead of asserting absence.
func rootAgentFailClosedValue(assembly rootAgentInspectionAssembly) ResolvedValue {
	rv := rootAgentResolvedValue(ResolveRootAgent(assembly.inputs), assembly.locs, true)
	rv.Value = RootAgent{}
	for i := range rv.Candidates {
		candidate := &rv.Candidates[i]
		if candidate.Layer == string(RootAgentSourcePersonal) {
			if assembly.personalUnreadable {
				candidate.Present = true
				candidate.Value = nil
				candidate.Result = "ignored"
				candidate.Reason = assembly.failClosed
				continue
			}
			if !candidate.Present {
				candidate.Result = "unknown"
				candidate.Reason = "cannot be determined: " + assembly.failClosed
				continue
			}
		}
		if !candidate.Present {
			continue
		}
		candidate.Result = "ignored"
		candidate.Reason = assembly.failClosed
	}
	rv.Origins = nil
	return rv
}

// RootAgentValueFailsClosed reports whether rv is the fail-closed rendering
// (#3264). The zero-provenance shape is the marker: every layered root_agent
// resolution carries at least Origins["enabled"] (ResolveRootAgent always
// sets EnabledSource), and the fail-closed verdict deliberately carries none,
// because no config source decided it.
func RootAgentValueFailsClosed(rv ResolvedValue) bool {
	return rv.Key == "root_agent" && len(rv.Origins) == 0
}

// ProjectFailClosedRootAgentLeaf projects one leaf of a fail-closed
// root_agent table for a dotted read (`af config get root_agent.enabled`).
// The generic ResolvedValuePath cannot serve this shape: it keys the
// projection on Origins — which a fail-closed verdict deliberately lacks —
// and it relabels every candidate against the winner's layer, overwriting the
// cause this rendering exists to surface. Here the effective leaf comes from
// the disabled profile and every candidate keeps its row verbatim, values
// leaf-projected.
func ProjectFailClosedRootAgentLeaf(parent ResolvedValue, keyPath string) (ResolvedValue, bool) {
	leaf, ok := strings.CutPrefix(keyPath, "root_agent.")
	if !ok || leaf == "" {
		return ResolvedValue{}, false
	}
	effective, ok := configLeafValue(parent.Value, leaf)
	if !ok {
		return ResolvedValue{}, false
	}
	projected := parent
	projected.Key = keyPath
	projected.Value = effective
	projected.Default = ""
	projected.Candidates = make([]CandidateTrace, len(parent.Candidates))
	for i, candidate := range parent.Candidates {
		candidate.KeyPath = keyPath
		if leafValue, present := configLeafValue(candidate.Value, leaf); present && candidate.Present {
			candidate.Value = leafValue
		} else {
			candidate.Value = nil
		}
		projected.Candidates[i] = candidate
	}
	return projected, true
}

// LegacyRootAgentForRepo returns the root_agents entry whose path resolves to
// repoID (and the matched key), or nil. Keys are checked in sorted order so the
// inspection surface, daemon lookup, and sorted ensure pass share one stable
// winner when two spellings resolve to the same repo.
func LegacyRootAgentForRepo(global *Config, repoID string) (*RootAgentConfig, string) {
	keys := make([]string, 0, len(global.RootAgents))
	for key := range global.RootAgents {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		expanded := ExpandTilde(key)
		if repo, err := RepoFromPath(expanded); err == nil {
			if repo.ID == repoID {
				entry := global.RootAgents[key]
				return &entry, key
			}
			continue
		}
		// The key's path does not resolve right now — an absent mount, a repo
		// not yet cloned (#1122). Fall back to recorded-root identity
		// (RepoIDForRecordedRoot), the same rule the daemon snapshot uses to
		// attribute unresolvable project roots: a caller asking about that
		// repo must still see this entry, because an empty entry means
		// enabled and the legacy ensure sweep's per-tick retry creates the
		// root the moment the path returns. Dropping it instead told
		// consumers "no layer enables this repo" about a repo whose opt-in
		// sat in root_agents the whole time (#3264 review).
		if RepoIDForRecordedRoot(expanded) == repoID {
			entry := global.RootAgents[key]
			return &entry, key
		}
	}
	return nil, ""
}

func rootAgentCandidateForLayer(inputs RootAgentInputs, source RootAgentSource) *RootAgentCandidate {
	for _, candidate := range ResolveRootAgent(inputs).Candidates {
		if candidate.Source == source && candidate.Present {
			candidateCopy := candidate
			return &candidateCopy
		}
	}
	return nil
}

func markRootAgentGlobalIneligible(
	resolved *ResolvedValue,
	candidate RootAgentCandidate,
	reason string,
	locs rootAgentLocations,
) {
	for i := range resolved.Candidates {
		if resolved.Candidates[i].Layer != string(RootAgentSourceGlobal) {
			continue
		}
		path, keyPath := locs.forSource(RootAgentSourceGlobal)
		resolved.Candidates[i] = CandidateTrace{
			Layer:   string(RootAgentSourceGlobal),
			Path:    path,
			KeyPath: keyPath,
			Allowed: false,
			Present: true,
			Value:   rootAgentCandidateLeaves(candidate),
			Result:  "ignored",
			Reason:  reason,
		}
		return
	}
}

// rootAgentResolvedValue maps a RootAgentResolution into the generic
// ResolvedValue trace so --explain renders it identically to every other key: a
// candidate row per layer (present or not), a per-field origin (Enabled and
// Program can come from different layers), and the by-field merge policy. Pure —
// no disk — so the per-field layer naming is unit-tested directly against the
// decisive fixtures (empty-legacy, personal-disable).
func rootAgentResolvedValue(res RootAgentResolution, locs rootAgentLocations, projectInScope bool) ResolvedValue {
	rv := ResolvedValue{
		Key:     "root_agent",
		Value:   res.RootAgent,
		Default: "not enabled",
		Merge:   MergeTableByField.String(),
		Precedence: []string{
			string(RootAgentSourceBuiltIn),
			string(RootAgentSourceGlobal),
			string(RootAgentSourceLegacy),
			string(RootAgentSourcePersonal),
		},
		Candidates: make([]CandidateTrace, 0, len(res.Candidates)),
	}
	for _, c := range res.Candidates {
		path, keyPath := locs.forSource(c.Source)
		reason := c.Reason
		if !c.Present {
			switch c.Source {
			case RootAgentSourceGlobal:
				reason = "not configured globally"
			case RootAgentSourceLegacy, RootAgentSourcePersonal:
				if projectInScope {
					reason = "this project has no entry"
				} else {
					reason = "no project in scope — pass --repo <path> to resolve this per-project layer"
				}
			}
		}
		rv.Candidates = append(rv.Candidates, CandidateTrace{
			Layer:   string(c.Source),
			Path:    path,
			KeyPath: keyPath,
			Allowed: true,
			Present: c.Present,
			Value:   rootAgentCandidateLeaves(c),
			Result:  c.Result,
			Reason:  reason,
		})
	}
	origins := map[string]SourceRef{
		"enabled": rootAgentOrigin(res.EnabledSource, locs),
	}
	// Program falls through to the default root profile when no layer sets it, so
	// it has a config origin only when some layer actually supplied a program.
	if res.ProgramSource != "" {
		origins["program"] = rootAgentOrigin(res.ProgramSource, locs)
	}
	rv.Origins = origins
	return rv
}

// rootAgentCandidateLeaves is one layer's own contribution as a leaf map — only
// the fields it set — so an absent layer shows "—", a program-only global shows
// just its program, and a present-but-empty legacy entry shows enabled=true with
// no program (the interaction nobody predicts, made legible). Returns nil for an
// absent layer so the renderer prints "—".
func rootAgentCandidateLeaves(c RootAgentCandidate) any {
	if !c.Present {
		return nil
	}
	leaves := map[string]any{}
	if c.EnabledSet {
		leaves["enabled"] = c.Enabled
	}
	if c.Program != "" {
		leaves["program"] = c.Program
	}
	return leaves
}

func rootAgentOrigin(source RootAgentSource, locs rootAgentLocations) SourceRef {
	path, keyPath := locs.forSource(source)
	return SourceRef{Layer: string(source), Path: path, KeyPath: keyPath}
}
