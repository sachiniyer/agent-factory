package session

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// RuntimeCleanupData is the storage-only teardown identity committed alongside a
// remote session's kill tombstone. It is deliberately a tagged union rather than
// a bag of shared strings: each backend restores only its own exact handle, and a
// malformed record carrying two variants is refused instead of guessed at.
//
// InstanceData uses a private staging field to keep this out of daemon snapshots;
// ForStorage publishes it only for UserKilled records. Bug reports drop it in
// full because host names, command paths, and container ids are operator-private.
type RuntimeCleanupData struct {
	Docker *DockerRuntimeCleanupData `json:"docker,omitempty"`
	SSH    *SSHRuntimeCleanupData    `json:"ssh,omitempty"`
	Hook   *HookRuntimeCleanupData   `json:"hook,omitempty"`
}

type DockerRuntimeCleanupData struct {
	ContainerID string `json:"container_id"`
	// EngineID is Docker's stable, non-secret daemon ID. Empty means a legacy
	// tombstone that cannot be targeted safely and must fail closed.
	EngineID string `json:"engine_id,omitempty"`
}

type SSHRuntimeCleanupData struct {
	Config     config.SSHConfig `json:"config"`
	SessionDir string           `json:"session_dir"`
	RemotePID  string           `json:"remote_pid,omitempty"`
}

type HookRuntimeCleanupData struct {
	DeleteCmd             string   `json:"delete_cmd"`
	Slug                  string   `json:"slug"`
	Agent                 string   `json:"agent,omitempty"`
	AgentResolved         bool     `json:"agent_resolved,omitempty"`
	AuthSelectors         []string `json:"auth_selectors,omitempty"`
	AuthSelectorsResolved bool     `json:"auth_selectors_resolved,omitempty"`
	SessionEnvPassthrough []string `json:"session_env_passthrough,omitempty"`
}

type runtimeCleanupProvider interface {
	runtimeCleanupData() *RuntimeCleanupData
}

var (
	_ runtimeCleanupProvider = (*dockerBackend)(nil)
	_ runtimeCleanupProvider = (*sshBackend)(nil)
	_ runtimeCleanupProvider = (*HookBackend)(nil)
)

func (d *RuntimeCleanupData) clone() *RuntimeCleanupData {
	if d == nil {
		return nil
	}
	out := &RuntimeCleanupData{}
	if d.Docker != nil {
		v := *d.Docker
		out.Docker = &v
	}
	if d.SSH != nil {
		v := *d.SSH
		out.SSH = &v
	}
	if d.Hook != nil {
		v := *d.Hook
		v.AuthSelectors = append([]string(nil), d.Hook.AuthSelectors...)
		v.SessionEnvPassthrough = append([]string(nil), d.Hook.SessionEnvPassthrough...)
		out.Hook = &v
	}
	return out
}

func positivePID(raw string) bool {
	pid, err := strconv.Atoi(strings.TrimSpace(raw))
	return err == nil && pid > 0
}

func (b *dockerBackend) runtimeCleanupData() *RuntimeCleanupData {
	if b == nil || b.cleanup == nil || b.cleanup.ContainerID == "" {
		return nil
	}
	v := *b.cleanup
	return &RuntimeCleanupData{Docker: &v}
}

func (b *sshBackend) runtimeCleanupData() *RuntimeCleanupData {
	if b == nil || b.cleanup == nil || b.cleanup.SessionDir == "" {
		return nil
	}
	v := *b.cleanup
	return &RuntimeCleanupData{SSH: &v}
}

func (b *HookBackend) runtimeCleanupData() *RuntimeCleanupData {
	if b == nil || b.cleanup == nil || b.cleanup.DeleteCmd == "" || b.cleanup.Slug == "" {
		return nil
	}
	v := *b.cleanup
	v.AuthSelectors = append([]string(nil), b.cleanup.AuthSelectors...)
	v.SessionEnvPassthrough = append([]string(nil), b.cleanup.SessionEnvPassthrough...)
	return &RuntimeCleanupData{Hook: &v}
}

// restoreRuntimeCleanup rebuilds only a teardown-capable backend. It does not
// dial, run docker, or invoke hook scripts while loading storage; the returned
// closure performs that I/O later when finishUserKill processes the tombstone.
func restoreRuntimeCleanup(title, backendType string, data *RuntimeCleanupData) (Backend, func() error, error) {
	if data == nil {
		return nil, nil, fmt.Errorf("no runtime cleanup handle was stored")
	}
	variants := 0
	if data.Docker != nil {
		variants++
	}
	if data.SSH != nil {
		variants++
	}
	if data.Hook != nil {
		variants++
	}
	if variants != 1 {
		return nil, nil, fmt.Errorf("runtime cleanup handle has %d backend variants, want exactly one", variants)
	}

	switch backendType {
	case "docker":
		if data.Docker == nil || strings.TrimSpace(data.Docker.ContainerID) == "" {
			return nil, nil, fmt.Errorf("docker cleanup handle has no container id")
		}
		if strings.TrimSpace(data.Docker.EngineID) == "" {
			return nil, nil, fmt.Errorf("docker cleanup handle has no engine identity (legacy record); select the original Docker context or DOCKER_HOST before repairing the record")
		}
		p := &dockerProvisioner{
			spec:               ProvisionSpec{Title: title},
			containerID:        data.Docker.ContainerID,
			engineID:           data.Docker.EngineID,
			verifyEngineOnReap: true,
		}
		teardown := p.reap
		return &dockerBackend{
			remoteAgentBackend: remoteAgentBackend{reap: teardown},
			containerID:        p.containerID,
			provisioner:        p,
			cleanup: &DockerRuntimeCleanupData{
				ContainerID: p.containerID,
				EngineID:    p.engineID,
			},
		}, teardown, nil
	case "ssh":
		if data.SSH == nil || strings.TrimSpace(data.SSH.Config.Host) == "" || strings.TrimSpace(data.SSH.SessionDir) == "" {
			return nil, nil, fmt.Errorf("ssh cleanup handle is missing its host or remote session directory")
		}
		if data.SSH.RemotePID != "" && !positivePID(data.SSH.RemotePID) {
			return nil, nil, fmt.Errorf("ssh cleanup handle has invalid remote pid %q", data.SSH.RemotePID)
		}
		cleanup := *data.SSH
		p := &sshProvisioner{
			spec:       ProvisionSpec{Title: title},
			cfg:        data.SSH.Config,
			sessionDir: data.SSH.SessionDir,
			remotePID:  data.SSH.RemotePID,
		}
		teardown := p.reap
		return &sshBackend{
			remoteAgentBackend: remoteAgentBackend{reap: teardown},
			provisioner:        p,
			cleanup:            &cleanup,
		}, teardown, nil
	case "remote":
		if data.Hook == nil || strings.TrimSpace(data.Hook.DeleteCmd) == "" || strings.TrimSpace(data.Hook.Slug) == "" {
			return nil, nil, fmt.Errorf("hook cleanup handle is missing delete_cmd or slug")
		}
		cleanup := *data.Hook
		passthrough, err := sessionenv.NormalizeExtraNames(data.Hook.SessionEnvPassthrough)
		if err != nil {
			return nil, nil, fmt.Errorf("hook cleanup handle has invalid session environment names: %w", err)
		}
		agent := data.Hook.Agent
		if agent != "" && !tmux.IsSupportedProgram(agent) {
			return nil, nil, fmt.Errorf("hook cleanup handle has invalid agent %q", agent)
		}
		if len(data.Hook.AuthSelectors) != 0 && !data.Hook.AuthSelectorsResolved {
			return nil, nil, fmt.Errorf("hook cleanup handle has authentication selectors without a resolved policy marker")
		}
		authSelectors, err := sessionenv.NormalizeAuthSelectors(agent, data.Hook.AuthSelectors)
		if err != nil {
			return nil, nil, fmt.Errorf("hook cleanup handle has invalid authentication selector names: %w", err)
		}
		// Records written before the environment boundary had no agent field and
		// historically ran hooks with Claude's environment. New records distinguish
		// that legacy absence from a resolved command that intentionally matched no
		// known agent, which must restore with the common allowlist only.
		program := agent
		if data.Hook.AgentResolved && agent == "" {
			program = hookNoAgentEnvironmentProgram
		}
		cleanup.SessionEnvPassthrough = append([]string(nil), passthrough...)
		cleanup.AuthSelectors = append([]string(nil), authSelectors...)
		p := &hookProvisioner{
			hooks: config.RemoteHooks{DeleteCmd: data.Hook.DeleteCmd},
			spec: ProvisionSpec{
				Title:                 title,
				SessionEnvPassthrough: passthrough,
			},
			slug:                  data.Hook.Slug,
			program:               program,
			authSelectors:         authSelectors,
			authSelectorsResolved: data.Hook.AuthSelectorsResolved,
			launchStarted:         true,
		}
		teardown := p.reap
		return &HookBackend{
			remoteAgentBackend: remoteAgentBackend{reap: teardown},
			provisioner:        p,
			cleanup:            &cleanup,
		}, teardown, nil
	default:
		return nil, nil, fmt.Errorf("backend %q has no restorable remote cleanup handle", backendType)
	}
}

func unavailableRuntimeCleanup(title, backendType string, cause error) func() error {
	return func() error {
		return fmt.Errorf("%w: session %q is tombstoned for backend %q, but its durable cleanup handle is unavailable (%v); retaining the record rather than claiming its off-box workspace was reaped",
			ErrWorkspaceStateUnknown, title, backendType, cause)
	}
}
