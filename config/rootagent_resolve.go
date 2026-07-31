package config

// This file is the canonical root-agent resolver and the permanent legacy
// `root_agents` compatibility adapter (#2216 Phase 6). It defines the singleton
// root-agent profile and ResolveRootAgent — the single authority on how every
// root-agent source combines, so no consumer re-implements precedence.
//
// PR1 shipped the built-in + legacy layers as a config-only adapter with the
// daemon untouched. PR2 adds the global and personal-project singleton layers and
// wires the daemon (EnsureRootAgents and repoRootAgentWillMaterialize) onto this
// function; the legacy path-keyed map is kept forever as a read-only source, so
// existing configs never stop working.

// RootAgent is the canonical root-agent profile: whether a project keeps an
// always-ensured "root" session, and the command it runs. It is the singular
// successor to the path-keyed `root_agents` map, which is adapted into this shape
// by ResolveRootAgent.
type RootAgent struct {
	// Enabled is whether an always-ensured root session runs for the project.
	// The built-in default is false, which keeps root agents strictly opt-in.
	//
	// Deliberately NO omitempty: an explicit `enabled = false` written to override
	// an enabling global layer must survive a full serialization — RegisterRootAgent
	// and saveConfigLocked toml.Marshal the whole Config — or the override is
	// silently erased on the next write, the zero-value-elision class this repo
	// already paid for in #1700.
	Enabled bool `json:"enabled" toml:"enabled"`
	// Program is the command the root session runs. Empty means UNSET, not "run
	// an empty command": it falls through to a lower-precedence layer's program
	// and ultimately the default root profile (the repo's resolved claude with
	// --dangerously-skip-permissions), exactly as an empty legacy
	// RootAgentConfig.Program does today.
	Program string `json:"program,omitempty" toml:"program,omitempty"`
}

// RootAgentLayer is one configured singleton [root_agent] source's contribution
// (global or personal-project). Value holds the decoded profile; EnabledSet
// reports whether the source explicitly set `enabled`, because an explicit
// `enabled = false` is a real value distinct from absence — presence comes from
// the source file's shape, not from the zero value. Program needs no such flag:
// empty means unset, the same convention the legacy source uses.
type RootAgentLayer struct {
	Value      RootAgent
	EnabledSet bool
}

// RootAgentInputs are the per-project candidate sources ResolveRootAgent layers,
// lowest precedence first. Each pointer is nil when that source configured no
// root agent for the project. Adding a field keeps existing callers valid.
type RootAgentInputs struct {
	// Global is the global [root_agent] singleton (nil = not configured).
	Global *RootAgentLayer
	// Legacy is the path-keyed `root_agents` entry for this project, or nil. A
	// present entry always ENABLES the root (that is what a legacy entry has
	// always meant); its program applies only when non-empty, because an empty
	// legacy program has always meant "use the default profile" — i.e. unset. The
	// common legacy entry IS empty (af's own register and project-switch paths
	// write an empty RootAgentConfig for every project), so treating empty as
	// set-to-empty would clobber a program back to nothing.
	Legacy *RootAgentConfig
	// Personal is the personal per-project [root_agent] singleton (nil = not
	// configured). Highest precedence — see the rationale on ResolveRootAgent.
	Personal *RootAgentLayer
}

// RootAgentSource names a candidate layer in a resolution trace.
type RootAgentSource string

const (
	// RootAgentSourceBuiltIn is the opt-in-false base.
	RootAgentSourceBuiltIn RootAgentSource = "built-in"
	// RootAgentSourceGlobal is the global [root_agent] singleton.
	RootAgentSourceGlobal RootAgentSource = "global"
	// RootAgentSourceLegacy is a path-keyed `root_agents` entry, kept as a
	// permanent read-only compatibility source.
	RootAgentSourceLegacy RootAgentSource = "legacy root_agents"
	// RootAgentSourcePersonal is the personal per-project [root_agent] singleton.
	RootAgentSourcePersonal RootAgentSource = "personal project"
)

// RootAgentCandidate is one considered layer in a ResolveRootAgent trace. The
// trace is produced in the same pass as the value so `af config get root_agent
// --explain` renders it without a second precedence implementation (#2216 §5).
type RootAgentCandidate struct {
	Source  RootAgentSource `json:"source"`
	Present bool            `json:"present"`
	Enabled bool            `json:"enabled"`
	// EnabledSet reports whether THIS layer set `enabled` (an explicit false
	// counts). It distinguishes a layer that contributed enabled from one that
	// left it to a lower layer, so the --explain trace can show each layer's own
	// fields — e.g. a present-but-empty legacy entry as `enabled=true` with no
	// program, rather than implying it also set enabled=false or a program.
	EnabledSet bool   `json:"enabled_set"`
	Program    string `json:"program,omitempty"`
	// Result is one of "base", "winner", "shadowed", or "absent".
	Result string `json:"result"`
	Reason string `json:"reason"`
}

// RootAgentResolution is the effective root-agent profile plus the full trace of
// every candidate the resolver considered.
type RootAgentResolution struct {
	RootAgent
	// EnabledSource / ProgramSource name the layer that supplied each effective
	// field, for the --explain surface. EnabledSource is always set (the built-in
	// base sets `enabled`); ProgramSource is empty when no layer supplied a
	// program and the default profile applies.
	EnabledSource RootAgentSource      `json:"enabled_source"`
	ProgramSource RootAgentSource      `json:"program_source,omitempty"`
	Candidates    []RootAgentCandidate `json:"candidates"`
}

// GlobalRootAgentLayer extracts the global [root_agent] singleton layer from a
// loaded Config, or nil when the config did not declare [root_agent]. Presence of
// `enabled` comes from the config's decoded shape, so an explicit `enabled=false`
// is distinguished from absence (a value the zero RootAgent cannot carry).
func GlobalRootAgentLayer(cfg *Config) *RootAgentLayer {
	if cfg == nil {
		return nil
	}
	shape, _ := cfg.source.topLevel("root_agent")
	return rootAgentLayerFromShape(cfg.RootAgent, shape)
}

// rootAgentLayerFromShape builds a RootAgentLayer from a decoded RootAgent value
// and the source's shape entry for "root_agent" (the shapeless decode used for
// presence). It returns nil when [root_agent] was not declared, and sets
// EnabledSet from whether the file's [root_agent] table carried an `enabled` key.
func rootAgentLayerFromShape(value RootAgent, rootAgentShape any) *RootAgentLayer {
	sub, ok := rootAgentShape.(map[string]any)
	if !ok {
		return nil
	}
	_, enabledSet := sub["enabled"]
	return &RootAgentLayer{Value: value, EnabledSet: enabledSet}
}

// rootAgentFromLegacy adapts a legacy RootAgentConfig into the canonical profile.
// It is the ONE place a legacy entry becomes canonical; the guard test
// TestRootAgentConfigIsFullyAdapted fails when RootAgentConfig gains a field, so
// a new per-repo field (the AutoYes/#2335 class) can never be silently dropped
// from resolution — and thus from an ensured root.
func rootAgentFromLegacy(rc RootAgentConfig) RootAgent {
	return RootAgent{Enabled: true, Program: rc.Program}
}

// rootAgentNormLayer is one precedence layer normalized to the same shape (the
// built-in base, a singleton layer, or the adapted legacy entry) so the fold and
// the trace treat every source uniformly.
type rootAgentNormLayer struct {
	source     RootAgentSource
	enabledSet bool
	enabled    bool
	program    string
}

// ResolveRootAgent layers the provided sources into one effective root-agent
// profile and returns it with the full provenance trace.
//
// PRECEDENCE (low → high) — DO NOT REORDER legacy above personal:
//
//	built-in  <  global  <  legacy root_agents[path]  <  personal-project
//
// Legacy MUST sit BELOW personal. RegisterRootAgent and the TUI project-switch
// write an EMPTY root_agents entry for EVERY registered project, and a present
// legacy entry means Enabled=true. If legacy outranked personal, that ubiquitous
// Enabled=true would override a personal `enabled=false`, so a user could NEVER
// disable a root for a registered project through the singleton — exactly the
// silent no-op #2216 exists to kill. Keeping legacy below personal lets a personal
// `enabled=false` disable it. (root_agent admits no in-repo layer.)
//
// Merge is by FIELD on presence: a higher layer overrides `enabled` only if it
// set it (an explicit false counts) and `program` only if non-empty (empty =
// unset). So an empty legacy entry enables the root without clobbering a global
// program, and a personal profile can flip enabled or reprogram independently.
func ResolveRootAgent(in RootAgentInputs) RootAgentResolution {
	layers := []rootAgentNormLayer{{source: RootAgentSourceBuiltIn, enabledSet: true, enabled: false}}
	if in.Global != nil {
		layers = append(layers, rootAgentNormLayer{RootAgentSourceGlobal, in.Global.EnabledSet, in.Global.Value.Enabled, in.Global.Value.Program})
	}
	if in.Legacy != nil {
		la := rootAgentFromLegacy(*in.Legacy)
		layers = append(layers, rootAgentNormLayer{RootAgentSourceLegacy, true, la.Enabled, la.Program})
	}
	if in.Personal != nil {
		layers = append(layers, rootAgentNormLayer{RootAgentSourcePersonal, in.Personal.EnabledSet, in.Personal.Value.Enabled, in.Personal.Value.Program})
	}

	var res RootAgentResolution
	for _, l := range layers {
		if l.enabledSet {
			res.Enabled = l.enabled
			res.EnabledSource = l.source
		}
		if l.program != "" {
			res.Program = l.program
			res.ProgramSource = l.source
		}
	}

	// Trace every possible source (present or not) so --explain shows the whole
	// picture, marking which supplied each effective field.
	present := map[RootAgentSource]rootAgentNormLayer{}
	for _, l := range layers {
		present[l.source] = l
	}
	for _, source := range []RootAgentSource{RootAgentSourceBuiltIn, RootAgentSourceGlobal, RootAgentSourceLegacy, RootAgentSourcePersonal} {
		l, ok := present[source]
		res.Candidates = append(res.Candidates, rootAgentCandidate(source, l, ok, res))
	}
	return res
}

// rootAgentCandidate builds one trace row, classifying the source against the
// resolved per-field origins.
func rootAgentCandidate(source RootAgentSource, l rootAgentNormLayer, present bool, res RootAgentResolution) RootAgentCandidate {
	c := RootAgentCandidate{Source: source, Present: present, Enabled: l.enabled, EnabledSet: l.enabledSet, Program: l.program}
	switch {
	case source == RootAgentSourceBuiltIn:
		c.Result = "base"
		c.Reason = "root agents are opt-in by default"
	case !present:
		c.Result = "absent"
		c.Reason = "not configured"
	default:
		wonEnabled := res.EnabledSource == source
		wonProgram := res.ProgramSource == source
		switch {
		case wonEnabled && wonProgram:
			c.Result, c.Reason = "winner", "supplies the effective enabled and program"
		case wonEnabled:
			c.Result, c.Reason = "winner", "supplies the effective enabled"
		case wonProgram:
			c.Result, c.Reason = "winner", "supplies the effective program"
		default:
			c.Result = "shadowed"
			if source == RootAgentSourceLegacy && l.program == "" {
				c.Reason = "enabled by a higher layer; its empty program is unset"
			} else {
				c.Reason = "overridden by a higher-precedence layer"
			}
		}
	}
	return c
}
