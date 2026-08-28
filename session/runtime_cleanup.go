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
// remote session's kill tombstone or an unknown cleanup outcome. It is
// deliberately a tagged union rather than a bag of shared strings: each backend
// restores only its own exact handle, and a malformed record carrying two
// variants is refused instead of guessed at.
//
// InstanceData uses a private staging field to keep this out of daemon snapshots;
// ForStorage publishes it only at those two retention boundaries. Bug reports
// drop it in full because host names, command paths, and container ids are
// operator-private.
type RuntimeCleanupData struct {
	Docker  *DockerRuntimeCleanupData  `json:"docker,omitempty"`
	SSH     *SSHRuntimeCleanupData     `json:"ssh,omitempty"`
	Sandbox *SandboxRuntimeCleanupData `json:"sandbox,omitempty"`
	Hook    *HookRuntimeCleanupData    `json:"hook,omitempty"`
}

// SandboxRuntimeCleanupData is the restart-safe teardown handle for the sandbox
// runtime (#2476 PR2). Without it, the unknown-state retention in
// sandboxProvisioner.reap would be pointless: the row is deliberately RETAINED
// when a reap cannot prove it ran, and after a daemon restart there would be no
// handle to retry with, so the remote agent-server and session dir would leak
// permanently.
//
// SSHCommand is operator-private (it can name hosts, ports and jump hosts), so
// it is subject to the same ForStorage scrubbing as its siblings.
type SandboxRuntimeCleanupData struct {
	SSHCommand string `json:"ssh_command"`
	SessionDir string `json:"session_dir"`
	RemotePID  string `json:"remote_pid,omitempty"`
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
	// DialAddress is the literal address this session was provisioned on: the one
	// thing the record knows and no re-resolution can recover. Without it, reaping a
	// multi-address name could reach a DIFFERENT machine, find nothing, report
	// success and retire the only tombstone — a permanent, silent leak (#3086).
	//
	// HOW IT IS APPLIED CHANGED, WHAT IT MEANS DID NOT. #3090 wrote this field and
	// dialled it as ssh's destination, which forced a `-o HostKeyAlias` and rejected
	// host certificates on every non-default port; #3100 reverted the writing but
	// kept honouring it. It is now applied as a `-o ProxyCommand` that pins only the
	// TCP dial while ssh's destination stays the configured NAME, so the certificate
	// conflict is gone and there is nothing left to accept as a cost.
	//
	// So a record from EITHER era reads the same way, and both reap correctly under
	// the current mechanism. An empty value — a record written before #3090, or
	// between #3100 and this change, or one whose provision could not settle on an
	// address — dials the name, exactly as it always did.
	// DialAddress is the machine this session was provisioned on, and the ONE field
	// that carries the pin. Two spellings, both of which every release reads or
	// round-trips safely:
	//
	//	"198.51.100.8"    an ADDRESS pin (#3086/#3118), reached on the configured port
	//	"198.51.100.8:22" a MACHINE pin (#3122), carrying the port that machine serves
	//
	// The second spelling exists because behind an L4 balancer the backend listens on
	// a different port than its VIP is reached on, so the port has to travel with the
	// address. It is deliberately NOT a separate field: a daemon rolled back to a
	// release without it would DROP that field on its next checkpoint — the pin and
	// the machine gone for good — while still honouring a bare DialAddress and
	// reaping some other backend. One field it already knows round-trips intact.
	//
	// And it fails closed there: that release appends the configured port itself, so
	// "198.51.100.8:22" becomes an unresolvable "[198.51.100.8:22]:2200", the relay
	// cannot dial, ssh exits 255, and the record is RETAINED and retried rather than
	// reaping the wrong machine. Measured. Retained-and-retried beats
	// silently-wrong-and-retired.
	//
	// A bare address is written whenever the machine's port IS the configured one, so
	// an older release keeps a pin it reads correctly. See sshRecordPinnedMachine and
	// sshPinnedCleanupTarget.
	DialAddress         string `json:"dial_address,omitempty"`
	HostKeyVerification string `json:"host_key_verification,omitempty"`
}

type HookRuntimeCleanupData struct {
	DeleteCmd             string   `json:"delete_cmd"`
	Slug                  string   `json:"slug"`
	Agent                 string   `json:"agent,omitempty"`
	AgentResolved         bool     `json:"agent_resolved,omitempty"`
	AuthSelectors         []string `json:"auth_selectors,omitempty"`
	AuthSelectorsResolved bool     `json:"auth_selectors_resolved,omitempty"`
	SessionEnvPassthrough []string `json:"session_env_passthrough,omitempty"`
	// HasKnownHostsDir marks a session provisioned through provision_cmd, which
	// pins one host key per session under hook-hosts/<slug> and OWNS that
	// directory. It is what lets a teardown rebuilt after a daemon restart drop
	// the pin on the same success-only condition the live one uses (#3454).
	//
	// It is a claim of OWNERSHIP, so only the provision_cmd constructor sets it —
	// never the shared cleanupData(), which the launch_cmd path uses too. That
	// contract owns a URL and a token and pins nothing, and a launch_cmd tombstone
	// claiming this directory would delete a live session's pin under a slug it
	// never owned.
	//
	// omitempty keeps it compatible in both directions, and both directions
	// degrade to the SAME pre-existing leak rather than to anything unsafe. A
	// record written before this field restores with it false and orphans its
	// directory exactly as every record does today; a daemon rolled BACK to a
	// release without the field silently DROPS it on its next checkpoint (#3122)
	// and lands in the same place. Nothing removes a pin it cannot prove it owns.
	HasKnownHostsDir bool `json:"has_known_hosts_dir,omitempty"`
}

type runtimeCleanupProvider interface {
	runtimeCleanupData() *RuntimeCleanupData
}

var (
	_ runtimeCleanupProvider = (*dockerBackend)(nil)
	_ runtimeCleanupProvider = (*sshBackend)(nil)
	_ runtimeCleanupProvider = (*sandboxBackend)(nil)
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
	if d.Sandbox != nil {
		v := *d.Sandbox
		out.Sandbox = &v
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

func (b *sandboxBackend) runtimeCleanupData() *RuntimeCleanupData {
	if b == nil || b.cleanup == nil || b.cleanup.SessionDir == "" {
		return nil
	}
	v := *b.cleanup
	return &RuntimeCleanupData{Sandbox: &v}
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
	if data.Sandbox != nil {
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
			containerID: p.containerID,
			provisioner: p,
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
		// A handle persisted before #3044 may embed one port and carry another.
		// Normalize it with the precedence it was written against, so the reap can
		// still reach the machine: refusing a TEARDOWN over an ambiguous address
		// protects nothing and leaks the workspace it was meant to remove.
		legacyCfg := data.SSH.Config
		legacyCfg.Host, legacyCfg.Port = normalizeLegacySSHAddress(legacyCfg.Host, legacyCfg.Port)
		// Compose the transport from the PERSISTED config, through the same
		// constructor the create path uses. A handle written before #3052 carries
		// ssh.* settings, not a command — and refusing to reap it because the
		// transport changed underneath would leak the workspace it exists to
		// remove (the #3044 lesson).
		// A handle carrying a dial address knows the machine its session actually ran
		// on. Reach THAT machine rather than re-resolving the name — see DialAddress.
		// An empty one composes the ordinary name-based command, so a record from
		// before any of this reaps exactly as it always did.
		pinAddr, pinPort := sshPinnedCleanupTarget(data.SSH)
		sshCmd, cmdErr := sshCommandPinnedTo(legacyCfg, data.SSH.HostKeyVerification, pinAddr, pinPort)
		if cmdErr != nil {
			return nil, nil, fmt.Errorf("ssh cleanup handle has an unusable address: %w", cmdErr)
		}
		p := newSSHSandboxProvisioner(ProvisionSpec{Title: title}, sshCmd, "", "")
		p.sessionDir = data.SSH.SessionDir
		p.remotePID = data.SSH.RemotePID
		// The accept-new store is prepared inside each ATTEMPT, never here: this
		// function composes a closure while persisted instances are being loaded,
		// and a transiently unwritable AF home must not be captured as a permanently
		// dead cleanup. sshCommandPinnedTo above touches no filesystem state for the
		// same reason.
		teardown := sshTeardownWithStore(p.reap, legacyCfg, data.SSH.HostKeyVerification)
		if strings.TrimSpace(data.SSH.HostKeyVerification) == "" {
			// The ONLY way an empty posture reaches here is a record written before
			// #2704 added the field — config defaults it to strict at parse time, so a
			// live session always records one. Such a record restores as strict, and if
			// its host is absent from the strict store no retry can ever complete it;
			// classify that so the daemon retires it instead of backing off forever
			// (#2737). See ssh_legacy_tombstone.go for what is deliberately different
			// from the x/crypto predicate this replaces.
			teardown = legacySSHTombstoneReap(teardown, legacyCfg)
		}
		return &sshBackend{
			provisioner: p,
			cleanup:     &cleanup,
		}, teardown, nil
	case "sandbox":
		if data.Sandbox == nil || strings.TrimSpace(data.Sandbox.SSHCommand) == "" || strings.TrimSpace(data.Sandbox.SessionDir) == "" {
			return nil, nil, fmt.Errorf("sandbox cleanup handle is missing its ssh command or remote session directory")
		}
		if data.Sandbox.RemotePID != "" && !positivePID(data.Sandbox.RemotePID) {
			return nil, nil, fmt.Errorf("sandbox cleanup handle has invalid remote pid %q", data.Sandbox.RemotePID)
		}
		cleanup := *data.Sandbox
		p := &sandboxProvisioner{
			spec:       ProvisionSpec{Title: title},
			sshCmd:     data.Sandbox.SSHCommand,
			sessionDir: data.Sandbox.SessionDir,
			remotePID:  data.Sandbox.RemotePID,
		}
		teardown := p.reap
		return &sandboxBackend{
			provisioner: p,
			cleanup:     &cleanup,
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
		if data.Hook.HasKnownHostsDir {
			// A provision_cmd session, which pinned one host key under
			// hook-hosts/<slug>. Finish that half of the teardown too, or a kill that
			// outlives its daemon reaps the machine and orphans the pin forever
			// (#3454). See restoredHookProvisionTeardown for why the directory is
			// resolved per attempt rather than here.
			teardown = restoredHookProvisionTeardown(p.reap, p.slug, title)
		}
		return &HookBackend{
			provisioner: p,
			cleanup:     &cleanup,
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
