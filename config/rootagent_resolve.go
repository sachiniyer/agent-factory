package config

// This file is the permanent legacy `root_agents` compatibility adapter (#2216
// Phase 6, PR1). It introduces the canonical root-agent profile and the single
// resolver, ResolveRootAgent, that combines every root-agent source into one
// effective decision so no consumer re-implements precedence.
//
// PR1 is the config-package adapter ONLY: it carries the built-in base and the
// legacy path-keyed source, with tests proving existing `root_agents` entries
// resolve to the canonical form unchanged. The daemon still reads
// m.cfg.RootAgents directly and is untouched here — the strongest non-breaking
// guarantee is that its ensure loop does not change at all. PR2's daemon
// integration is what switches EnsureRootAgents AND repoRootAgentWillMaterialize
// (which both read the map directly today) onto this resolver, at the same time
// it adds the global and personal-project singleton layers below the personal one.

// RootAgent is the canonical root-agent profile: whether a project keeps an
// always-ensured "root" session, and the command it runs. It is the singular
// successor to the path-keyed `root_agents` map. That map is preserved forever
// as a read-only compatibility source and adapted into this shape by
// ResolveRootAgent — existing configs never stop working.
type RootAgent struct {
	// Enabled is whether an always-ensured root session runs for the project.
	// The built-in default is false, which keeps root agents strictly opt-in.
	//
	// Deliberately NO omitempty: once this is the singleton [root_agent] key
	// (PR2), an explicit `enabled = false` written to override an enabling global
	// layer must survive a full serialization — RegisterRootAgent and
	// saveConfigLocked toml.Marshal the whole Config — or the override is silently
	// erased on the next write, the zero-value-elision class this repo already
	// paid for in #1700.
	Enabled bool `json:"enabled" toml:"enabled"`
	// Program is the command the root session runs. Empty means UNSET, not "run
	// an empty command": it falls through to a lower-precedence layer's program
	// and ultimately the default root profile (the repo's resolved claude with
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
	// present entry always ENABLES the root (that is what a legacy entry has
	// always meant); its program applies only when non-empty, because an empty
	// legacy program has always meant "use the default profile" — i.e. unset. That
	// distinction is load-bearing once a lower layer can supply a program: the
	// common legacy entry is EMPTY (af's own register and project-switch paths
	// write an empty RootAgentConfig for every project), so treating empty as
	// set-to-empty would clobber a global program back to nothing.
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
// present legacy entry resolves to Enabled=true with its program (an empty
// program stays unset and falls through to the default profile) — matching the
// profile the daemon derives when it reads the map directly, which is the
// non-breaking guarantee this PR proves by test. PR1 does NOT wire the daemon to
// this function. PR2's daemon integration does, and adds the global layer
// between the built-in base and the legacy source, and the personal-project
// layer after it.
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
		reason := "a path-keyed root_agents entry is an always-ensured root"
		if in.Legacy.Program != "" {
			res.Program = in.Legacy.Program
		} else {
			// An empty legacy program is UNSET, not set-to-empty: leave any
			// lower-precedence program in place instead of clobbering it (P3-a).
			reason += "; empty program is unset, so the default profile applies"
		}
		res.Candidates = append(res.Candidates, RootAgentCandidate{
			Source:  RootAgentSourceLegacy,
			Present: true,
			Enabled: true,
			Program: in.Legacy.Program,
			Result:  "winner",
			Reason:  reason,
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
