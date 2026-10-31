package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync/atomic"
)

// AgentPolicy is the operator policy the daemon resolves once and hands to an
// off-box agent-server (#5571 W2.5). The agent-server runs on another
// filesystem: reading ITS host's AF home would apply a stranger's config.toml
// (or create one), and reading the clone's committed .agent-factory file would
// miss uncommitted edits, legacy per-repo keys and hook saves made in the TUI.
// The daemon holds all of those, so it resolves them and ships the answer.
//
// It is an allowlist projection, not the Config: fields that exist to run the
// daemon (sandbox.ssh, hosts, move_cmd, default_accounts, the listener) are
// never serialized, because the file lands on a machine the operator does not
// necessarily control end to end.
type AgentPolicy struct {
	Version int          `json:"version"`
	Global  PolicyLayer  `json:"global"`
	Repo    PolicyLayer  `json:"repo"`
	Hooks   PolicyHooks  `json:"hooks"`
	Source  PolicySource `json:"source,omitempty"`
}

// PolicySource records where the daemon resolved the policy from, for the
// agent-server's log; it carries no behaviour.
type PolicySource struct {
	ProjectRoot string `json:"project_root,omitempty"`
}

// PolicyLayer is the projection of one resolved Config the agent-server reads.
type PolicyLayer struct {
	DefaultProgram        string            `json:"default_program,omitempty"`
	ProgramOverrides      map[string]string `json:"program_overrides,omitempty"`
	SessionEnvPassthrough []string          `json:"session_env_passthrough,omitempty"`
	BranchPrefix          string            `json:"branch_prefix"`
	WorktreeRoot          string            `json:"worktree_root,omitempty"`
	GlobalAgentSkills     bool              `json:"global_agent_skills"`
}

// PolicyHooks are the repo's effective hooks.
type PolicyHooks struct {
	PostWorktreeCommands []string `json:"post_worktree_commands,omitempty"`
}

// AgentPolicyVersion is the only wire version this build reads.
const AgentPolicyVersion = 1

// NewAgentPolicy projects the daemon's resolved global and repo configs.
func NewAgentPolicy(global, repo *ResolvedConfig) AgentPolicy {
	p := AgentPolicy{Version: AgentPolicyVersion}
	if global != nil {
		p.Global = policyLayer(global)
	}
	if repo != nil {
		p.Repo = policyLayer(repo)
		p.Hooks.PostWorktreeCommands = slices.Clone(repo.PostWorktreeCommands)
		p.Source.ProjectRoot = repo.ProjectRoot
	}
	return p
}

// policyLayer copies the allowlisted fields. A program override the operator
// did not author (the daemon's auto-detected built-in, an absolute path on the
// DAEMON's filesystem) is dropped, so the agent-server host keeps detecting its
// own (#5571 W2.1).
func policyLayer(r *ResolvedConfig) PolicyLayer {
	layer := PolicyLayer{
		DefaultProgram:        r.DefaultProgram,
		SessionEnvPassthrough: slices.Clone(r.SessionEnvPassthrough),
		BranchPrefix:          r.BranchPrefix,
		WorktreeRoot:          r.WorktreeRoot,
		GlobalAgentSkills:     r.GlobalAgentSkills,
	}
	for agent, command := range r.ProgramOverrides {
		if value, ok := r.ResolvedValuePath("program_overrides." + agent); ok && value.Winner != nil && value.Winner.Layer == SourceBuiltIn.String() {
			continue
		}
		if layer.ProgramOverrides == nil {
			layer.ProgramOverrides = map[string]string{}
		}
		layer.ProgramOverrides[agent] = command
	}
	return layer
}

// Marshal renders the policy as the file the agent-server reads.
func (p AgentPolicy) Marshal() ([]byte, error) {
	return json.Marshal(p)
}

// ParseAgentPolicy reads a policy file. Unknown fields are an error, so a
// daemon newer than this binary cannot hand it policy it would silently drop.
func ParseAgentPolicy(data []byte) (AgentPolicy, error) {
	var p AgentPolicy
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return AgentPolicy{}, fmt.Errorf("agent policy is not valid: %w", err)
	}
	if p.Version != AgentPolicyVersion {
		return AgentPolicy{}, fmt.Errorf("agent policy version %d is not supported by this af (want %d): update af on this host", p.Version, AgentPolicyVersion)
	}
	return p, nil
}

// LoadAgentPolicyFile reads and parses path.
func LoadAgentPolicyFile(path string) (AgentPolicy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentPolicy{}, fmt.Errorf("reading the agent policy: %w", err)
	}
	return ParseAgentPolicy(data)
}

var installedAgentPolicy atomic.Pointer[AgentPolicy]

// InstallAgentPolicy makes p the process-wide source of configuration:
// LoadConfig, ResolveGlobalConfig and every repo resolve answer from it and
// never read or create anything under the AF home. Only an off-box
// agent-server calls it.
func InstallAgentPolicy(p AgentPolicy) { installedAgentPolicy.Store(&p) }

// ClearAgentPolicy removes the installed policy (tests).
func ClearAgentPolicy() { installedAgentPolicy.Store(nil) }

func activeAgentPolicy() *AgentPolicy { return installedAgentPolicy.Load() }

// config builds the Config a layer stands for. It starts from DefaultConfig so
// every field the policy does not carry has a sane value (a zero poll interval
// would panic a ticker), then overlays the layer; the host's own detected
// built-in overrides stay unless the operator authored one for the same agent.
func (l PolicyLayer) config() *Config {
	cfg := DefaultConfig()
	if l.DefaultProgram != "" {
		cfg.DefaultProgram = l.DefaultProgram
	}
	if cfg.ProgramOverrides == nil && len(l.ProgramOverrides) > 0 {
		cfg.ProgramOverrides = map[string]string{}
	}
	for agent, command := range l.ProgramOverrides {
		cfg.ProgramOverrides[agent] = command
	}
	cfg.SessionEnvPassthrough = slices.Clone(l.SessionEnvPassthrough)
	cfg.BranchPrefix = l.BranchPrefix
	if l.WorktreeRoot != "" {
		cfg.WorktreeRoot = l.WorktreeRoot
	}
	cfg.GlobalAgentSkills = l.GlobalAgentSkills
	return cfg
}

func (p *AgentPolicy) globalConfig() *Config { return p.Global.config() }

func (p *AgentPolicy) resolvedRepo(repoRoot string) *ResolvedConfig {
	root := repoRoot
	if root == "" {
		root = p.Source.ProjectRoot
	}
	return &ResolvedConfig{
		Config:               *p.Repo.config(),
		PostWorktreeCommands: slices.Clone(p.Hooks.PostWorktreeCommands),
		ProjectRoot:          root,
	}
}
