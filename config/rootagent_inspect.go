package config

import (
	"fmt"
	"sort"
	"strconv"
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
// layers by, so those layers resolve as absent; pass --project <path> to see them
// participate.
func ResolveRootAgentForInspection(projectSelector string) (ResolvedValue, error) {
	inputs, locs, err := assembleRootAgentInspectionInputs(projectSelector)
	if err != nil {
		return ResolvedValue{}, err
	}
	return rootAgentResolvedValue(ResolveRootAgent(inputs), locs), nil
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
func assembleRootAgentInspectionInputs(projectSelector string) (RootAgentInputs, rootAgentLocations, error) {
	global, err := LoadConfig()
	if err != nil {
		return RootAgentInputs{}, rootAgentLocations{}, err
	}
	locs := rootAgentLocations{globalPath: global.source.path}
	if locs.globalPath == "" {
		if path, err := globalConfigTomlPath(); err == nil {
			locs.globalPath = path
		}
	}
	inputs := RootAgentInputs{Global: GlobalRootAgentLayer(global)}

	if projectSelector != "" {
		abs, err := ResolveUserPath(projectSelector)
		if err != nil {
			return RootAgentInputs{}, rootAgentLocations{}, fmt.Errorf("failed to resolve --project path %q: %w", projectSelector, err)
		}
		repo, err := RepoFromPath(abs)
		if err != nil {
			return RootAgentInputs{}, rootAgentLocations{}, fmt.Errorf("failed to resolve --project path %q: %w", projectSelector, err)
		}
		if legacy, key := legacyRootAgentForRepoID(global, repo.ID); legacy != nil {
			inputs.Legacy = legacy
			locs.legacyKey = key
		}
		project, found, err := projectForRoot(repo.Root)
		if err != nil {
			return RootAgentInputs{}, rootAgentLocations{}, err
		}
		if found {
			pc, err := LoadProjectConfig(project.ID)
			if err != nil {
				return RootAgentInputs{}, rootAgentLocations{}, err
			}
			if layer := pc.RootAgentLayer(); layer != nil {
				inputs.Personal = layer
				if path, err := ProjectConfigTomlPath(project.ID); err == nil {
					locs.personalPath = path
				}
			}
		}
	}

	return inputs, locs, nil
}

// legacyRootAgentForRepoID returns the root_agents entry whose path resolves to
// repoID (and the matched key), or nil. Keys are checked in sorted order so the
// match is deterministic when two spellings resolve to the same repo — the same
// stability the daemon's snapshot relies on.
func legacyRootAgentForRepoID(global *Config, repoID string) (*RootAgentConfig, string) {
	keys := make([]string, 0, len(global.RootAgents))
	for key := range global.RootAgents {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		repo, err := RepoFromPath(ExpandTilde(key))
		if err != nil {
			continue
		}
		if repo.ID == repoID {
			entry := global.RootAgents[key]
			return &entry, key
		}
	}
	return nil, ""
}

// rootAgentResolvedValue maps a RootAgentResolution into the generic
// ResolvedValue trace so --explain renders it identically to every other key: a
// candidate row per layer (present or not), a per-field origin (Enabled and
// Program can come from different layers), and the by-field merge policy. Pure —
// no disk — so the per-field layer naming is unit-tested directly against the
// decisive fixtures (empty-legacy, personal-disable).
func rootAgentResolvedValue(res RootAgentResolution, locs rootAgentLocations) ResolvedValue {
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
		rv.Candidates = append(rv.Candidates, CandidateTrace{
			Layer:   string(c.Source),
			Path:    path,
			KeyPath: keyPath,
			Allowed: true,
			Present: c.Present,
			Value:   rootAgentCandidateLeaves(c),
			Result:  c.Result,
			Reason:  c.Reason,
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
