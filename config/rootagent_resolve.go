package config

// This file is the permanent legacy `root_agents` compatibility adapter (#2216
// Phase 6, PR1). It introduces the canonical root-agent profile and the single
// resolver that combines every root-agent source into one effective decision,
// so no consumer re-implements precedence. PR1 carries only the built-in and the
// legacy path-keyed source; a later PR adds the global and personal-project
// singleton layers to the same resolver without any consumer changing.

// RootAgent is the canonical root-agent profile: whether a project keeps an
// always-ensured "root" session, and the command it runs. It is the singular
// successor to the path-keyed `root_agents` map. That map is preserved forever
// as a read-only compatibility source and adapted into this shape by
// ResolveRootAgent — existing configs never stop working.
type RootAgent struct {
	// Enabled is whether an always-ensured root session runs for the project.
	// The built-in default is false, which keeps root agents strictly opt-in.
	Enabled bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
	// Program is the command the root session runs. Empty selects the default
	// root profile downstream (the repo's resolved claude with
	// --dangerously-skip-permissions), exactly as an empty legacy
	// RootAgentConfig.Program does today.
	Program string `json:"program,omitempty" toml:"program,omitempty"`
}

// RootAgentInputs are the per-project candidate values ResolveRootAgent layers,
// lowest precedence first. Each is nil when that source configured no root agent
// for the project. The struct grows as later #2216 phases add the global and
// personal-project singleton layers; PR1 carries only the permanent legacy
// compatibility source, and adding fields keeps existing callers valid.
type RootAgentInputs struct {
	// Legacy is the path-keyed `root_agents` entry for this project, or nil. A
	// present entry is always enabled with its configured program — that is what
	// a legacy entry has always meant, and preserving it verbatim is the
	// non-breaking compatibility guarantee.
	Legacy *RootAgentConfig
}

// RootAgentSource names a candidate layer in a resolution trace.
type RootAgentSource string

const (
	// RootAgentSourceBuiltIn is the opt-in-false base.
	RootAgentSourceBuiltIn RootAgentSource = "built-in"
	// RootAgentSourceLegacy is a path-keyed `root_agents` entry, kept as a
	// permanent read-only compatibility source.
	RootAgentSourceLegacy RootAgentSource = "legacy root_agents"
)

// RootAgentCandidate is one considered layer in a ResolveRootAgent trace. The
// trace is produced in the same pass as the value so a later `af config get
// root_agent --explain` renders it without a second precedence implementation
// (#2216 §5).
type RootAgentCandidate struct {
	Source  RootAgentSource `json:"source"`
	Present bool            `json:"present"`
	Enabled bool            `json:"enabled"`
	Program string          `json:"program,omitempty"`
	// Result is one of "base", "winner", "shadowed", or "absent".
	Result string `json:"result"`
	Reason string `json:"reason"`
}

// RootAgentResolution is the effective root-agent profile plus the full trace of
// every candidate the resolver considered.
type RootAgentResolution struct {
	RootAgent
	Candidates []RootAgentCandidate `json:"candidates"`
}

// ResolveRootAgent layers the provided candidate sources into one effective
// root-agent profile, lowest precedence first, and returns the effective value
// together with its provenance trace. It is the single authority on how
// root-agent config combines.
//
// PR1 (#2216 Phase 6) carries the permanent legacy compatibility source only:
// the built-in opt-in-false base, then the path-keyed `root_agents` entry. A
// present legacy entry resolves to exactly {Enabled: true, Program:
// entry.Program} — byte-for-byte the profile the daemon used when it read the
// map directly, which is the non-breaking guarantee this PR is built to prove.
// Later phases add the global and personal-project singleton layers to this
// function; consumers keep calling ResolveRootAgent unchanged.
func ResolveRootAgent(in RootAgentInputs) RootAgentResolution {
	res := RootAgentResolution{}
	// Built-in base: root agents are opt-in, so nothing runs unless a higher
	// layer turns it on.
	res.Candidates = append(res.Candidates, RootAgentCandidate{
		Source:  RootAgentSourceBuiltIn,
		Present: true,
		Enabled: false,
		Result:  "base",
		Reason:  "root agents are opt-in by default",
	})

	if in.Legacy != nil {
		res.Enabled = true
		res.Program = in.Legacy.Program
		res.Candidates = append(res.Candidates, RootAgentCandidate{
			Source:  RootAgentSourceLegacy,
			Present: true,
			Enabled: true,
			Program: in.Legacy.Program,
			Result:  "winner",
			Reason:  "a path-keyed root_agents entry is an always-ensured root",
		})
	} else {
		res.Candidates = append(res.Candidates, RootAgentCandidate{
			Source:  RootAgentSourceLegacy,
			Present: false,
			Result:  "absent",
			Reason:  "no root_agents entry for this project",
		})
	}
	return res
}
