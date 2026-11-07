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
	// RemoteAddr, AgentServerVersion and TokenRef are the reattach half of the
	// runtime identity (#5571 W1.1): the agent-server's own loopback address on
	// the sandbox (the banner's addr — the destination an `ssh -L` forward has
	// to re-dial), the version the remote binary reported, and the opaque
	// reference the daemon's runtime-identity key derives the expected bearer
	// token from. The token itself is NEVER persisted — deriving it needs the
	// 0600 key the daemon keeps in its AF home. All three are absent on rows
	// written before W1.1 (and on docker/hook rows), which reload inert
	// exactly as before.
	RemoteAddr         string `json:"remote_addr,omitempty"`
	AgentServerVersion string `json:"agent_server_version,omitempty"`
	TokenRef           string `json:"token_ref,omitempty"`
	// LaunchScript is the command that started the agent-server (W1.4), so a
	// crashed one can be relaunched after proof it is dead. No secret in it: the
	// token and callback credential live in the 0600 env files it sources.
	LaunchScript string `json:"launch_script,omitempty"`
	// AdoptPending records that a restarted agent-server is running but has not
	// yet adopted its surviving workspace (W1.4). A reattach that finds the
	// server responsive finishes the adoption instead of binding a server that
	// holds no session.
	AdoptPending bool `json:"adopt_pending,omitempty"`
	// CallbackRef and CallbackReverse are the callback half of the runtime
	// identity (#5571 W3.6): the reference the sandbox's callback credential
	// derives from and the `-R` spec its forward uses. Neither is a secret — the
	// credential is re-derived from the daemon's key — and both are absent when no
	// callback was granted or the row predates this field.
	CallbackRef     string `json:"callback_ref,omitempty"`
	CallbackReverse string `json:"callback_reverse,omitempty"`
	// LaunchUnconfirmed is set when an agent-server was launched but its pid was
	// never obtained (#5571 W8.6): reap then refuses to remove the directory without
	// stopping a server that may still run, across a daemon restart too.
	LaunchUnconfirmed bool `json:"launch_unconfirmed,omitempty"`
	// AccountSyncHost is the listed host (global `hosts`) that licensed the
	// session's synced account (W7.3), recorded only when one was synced. A
	// reattach after a daemon restart rebuilds the account workspace from it and
	// the row's account (#5948); empty on rows with no account.
	AccountSyncHost string `json:"account_sync_host,omitempty"`
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
	// HostName is the listed host (global `hosts`) the session was placed on, or
	// empty for a session built from the repo's own ssh settings. Config above is
	// the resolved copy the reap composes from, so a later edit to the host list
	// cannot redirect a teardown; this records the operator's name for it (W6.3).
	HostName string `json:"host_name,omitempty"`
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
	// The same reattach identity as SandboxRuntimeCleanupData carries (#5571
	// W1.1) — see its fields for the contract.
	RemoteAddr         string `json:"remote_addr,omitempty"`
	AgentServerVersion string `json:"agent_server_version,omitempty"`
	TokenRef           string `json:"token_ref,omitempty"`
	LaunchScript       string `json:"launch_script,omitempty"`
	AdoptPending       bool   `json:"adopt_pending,omitempty"`
	// CallbackRef and CallbackReverse are the callback half of the runtime
	// identity (#5571 W3.6): the reference the sandbox's callback credential
	// derives from and the `-R` spec its forward uses. Neither is a secret — the
	// credential is re-derived from the daemon's key — and both are absent when no
	// callback was granted or the row predates this field.
	CallbackRef     string `json:"callback_ref,omitempty"`
	CallbackReverse string `json:"callback_reverse,omitempty"`
	// LaunchUnconfirmed is set when an agent-server was launched but its pid was
	// never obtained (#5571 W8.6): reap then refuses to remove the directory without
	// stopping a server that may still run, across a daemon restart too.
	LaunchUnconfirmed bool `json:"launch_unconfirmed,omitempty"`
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
	// clearRuntimeCleanupData drops the backend's immutable cleanup identity in
	// place. It is called only after a confirmed reap (Instance.resetRemoteRuntime)
	// so a later checkpoint cannot publish a teardown handle for a runtime af has
	// already reaped, and a restart cannot rebuild one to act on a resource it no
	// longer owns. The reaper itself is the provisioner's, not this field, so
	// clearing it does not affect a teardown still bound to runtimeTeardown.
	clearRuntimeCleanupData()
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

func (b *dockerBackend) clearRuntimeCleanupData() {
	if b != nil {
		b.cleanup = nil
	}
}

func (b *sshBackend) runtimeCleanupData() *RuntimeCleanupData {
	if b == nil || b.cleanup == nil || b.cleanup.SessionDir == "" {
		return nil
	}
	v := *b.cleanup
	return &RuntimeCleanupData{SSH: &v}
}

func (b *sshBackend) clearRuntimeCleanupData() {
	if b != nil {
		b.cleanup = nil
	}
}

func (b *sandboxBackend) runtimeCleanupData() *RuntimeCleanupData {
	if b == nil || b.cleanup == nil || b.cleanup.SessionDir == "" {
		return nil
	}
	v := *b.cleanup
	return &RuntimeCleanupData{Sandbox: &v}
}

func (b *sandboxBackend) clearRuntimeCleanupData() {
	if b != nil {
		b.cleanup = nil
	}
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

func (b *HookBackend) clearRuntimeCleanupData() {
	if b != nil {
		b.cleanup = nil
	}
}

// restoreRuntimeCleanup rebuilds only a teardown-capable backend. It does not
// dial, run docker, or invoke hook scripts while loading storage; the returned
// closure performs that I/O later when finishUserKill processes the tombstone.
func restoreRuntimeCleanup(title, backendType string, data *RuntimeCleanupData) (Backend, func() error, error) {
	return restoreRuntimeCleanupWithAccount(title, backendType, data, restoredAccount{})
}

// restoreRuntimeCleanupWithAccount is restoreRuntimeCleanup for a row that
// carries a synced account: the sandbox's account workspace is rebuilt from the
// row (#5948).
func restoreRuntimeCleanupWithAccount(title, backendType string, data *RuntimeCleanupData, account restoredAccount) (Backend, func() error, error) {
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
		// The W1.1 identity rides the handle back into the live provisioner: the
		// token ref re-binds the reap's process check, and remote_addr/version are
		// what a reattach keys on. All three are empty on pre-W1.1 rows.
		p.remoteAddr = data.SSH.RemoteAddr
		p.agentServerVersion = data.SSH.AgentServerVersion
		p.tokenRef = data.SSH.TokenRef
		p.launchScript, p.adoptPending = data.SSH.LaunchScript, data.SSH.AdoptPending
		p.restoreCallbackIdentity(data.SSH.CallbackRef, data.SSH.CallbackReverse)
		p.launchAttempted = data.SSH.LaunchUnconfirmed
		p.restoreAccountWorkspace(account, data.SSH.HostName)
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
			spec:               ProvisionSpec{Title: title},
			sshCmd:             data.Sandbox.SSHCommand,
			sessionDir:         data.Sandbox.SessionDir,
			remotePID:          data.Sandbox.RemotePID,
			remoteAddr:         data.Sandbox.RemoteAddr,
			agentServerVersion: data.Sandbox.AgentServerVersion,
			tokenRef:           data.Sandbox.TokenRef,
			launchScript:       data.Sandbox.LaunchScript,
			adoptPending:       data.Sandbox.AdoptPending,
		}
		p.restoreCallbackIdentity(data.Sandbox.CallbackRef, data.Sandbox.CallbackReverse)
		p.launchAttempted = data.Sandbox.LaunchUnconfirmed
		p.restoreAccountWorkspace(account, data.Sandbox.AccountSyncHost)
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

// unavailableRuntimeCleanup is the teardown a reload installs when the row's
// persisted cleanup handle cannot be rebuilt. The workspace state is genuinely
// unknown — the record must be retained rather than deleted — but the cause is
// also unhealable by retrying: the same persisted handle fails every rebuild,
// so the refusal composes ErrCleanupHandleUnusable into the unknown-state
// class and the daemon's paced retry retires the record instead of looping the
// same answer (#2737). And because #6791 requires every refusal on this path to
// name a way out that actually works, the message ends with the manual steps —
// repair the persisted handle, or remove what it names and drop the row.
func unavailableRuntimeCleanup(data InstanceData, cause error) func() error {
	id, title, backendType := data.ID, data.Title, data.BackendType
	ws, handle := data.ArchivedWorkspace, data.RuntimeCleanup
	return func() error {
		return &unusableCleanupRefusal{msg: fmt.Sprintf(
			"session %q is tombstoned for backend %q, but its durable cleanup handle could not be rebuilt (%v); retaining the record rather than claiming its off-box workspace was reaped. %s",
			title, backendType, cause, cleanupRecordManualWayOut(id, title, ws, handle, true))}
	}
}

// cleanupRecordManualWayOut names the steps an operator can still take when af
// cannot rebuild a session's off-box cleanup handle (#6791). No verb covers a
// handle that cannot be rebuilt, so the way out is the record itself, edited by
// hand: repair the persisted handle so the next daemon start rebuilds it, or
// remove the off-box resource it names and drop the row. The ordering is the
// only one that works: every af surface respawns the daemon it cannot find
// (the TUI's snapshot poll runs EnsureDaemon, and so does every `af` CLI call),
// so the TUIs and stray `af` runs are silenced FIRST, the daemon is stopped
// AFTER them, and `af daemon status` — which starts nothing — re-verifies the
// stop immediately before the save. Skip either and a respawned daemon flushes
// its in-memory copy over the file, losing the repair or resurrecting a
// deleted row with no signal it happened (#6791 review item 1).
//
// committedKill selects the post-restart outcome. The kill path shows this
// after the tombstone is already durable, so a repaired row's kill finishes on
// the next poll — restore is impossible for it, and EVERY ending removes the
// resource, which is why its keep-work warning leads. The restore path can
// keep the work through adoption, so its warning sits inside the destructive
// branches instead — and it names `remote_pid` as what a repair needs, because
// a handle without one still cannot prove the recorded server stopped and
// restore refuses again (#6791 review items 2-4).
func cleanupRecordManualWayOut(id, title string, ws *ArchivedWorkspaceData, handle *RuntimeCleanupData, committedKill bool) string {
	home := "the AF home (~/.agent-factory, or $AGENT_FACTORY_HOME when set)"
	if dir, err := config.GetConfigDir(); err == nil {
		home = dir
	}
	resource := cleanupResourceDescription(ws, handle)
	keepWork := "it may hold work that was never pushed — copy what you want to keep out of it first"
	if ws != nil && ws.Dir != "" {
		host := "its host"
		if ws.Host != "" {
			host = fmt.Sprintf("host %q", ws.Host)
		}
		keepWork = fmt.Sprintf("it holds the session's unpushed work — copy the directory or push its branch on %s first if you want to keep it", host)
	}
	preamble, edit, outcome := "", "", ""
	if committedKill {
		preamble = fmt.Sprintf(" Every ending below removes %s: %s.", resource, keepWork)
		edit = fmt.Sprintf("either repair this session's `runtime_cleanup` entry, or remove %s yourself and delete that row", resource)
		outcome = fmt.Sprintf("the kill you ran is already recorded, so with a repaired handle it finishes on the daemon's next poll — or run `af sessions kill %q` to retry it now; with the row deleted there is nothing left to reap", title)
	} else {
		edit = fmt.Sprintf("either repair this session's `runtime_cleanup` entry — restore can use it only if it carries enough for af to prove the recorded server stopped, meaning the `remote_pid` of the agent-server af left running — or, to give the session up (%s), remove %s yourself and delete that row", keepWork, resource)
		outcome = fmt.Sprintf("retry `af sessions restore %q`. If af still cannot prove the server stopped it refuses the same way; removal is then — %s — `af sessions kill %q`, which reaps %s", title, keepWork, title, resource)
	}
	return fmt.Sprintf("To move on, edit the row by hand — no af verb covers a handle that cannot be rebuilt.%s\n"+
		"  1. Quit every af TUI and pause anything else that runs `af` — each one restarts the daemon.\n"+
		"  2. Send SIGTERM to the pid `af daemon status` names and wait for it to exit; the exit flushes this row over the file, so nothing af-related may run while you edit.\n"+
		"  3. In the `instances/*/instances.json` file under %s that contains the row whose id is %q — `grep -l %q %s/instances/*/instances.json` names it — %s.\n"+
		"  4. Right before saving, run `af daemon status` again — if a daemon is back, repeat from step 1 or its exit overwrites your edit.\n"+
		"  5. Start af; %s",
		preamble, home, id, id, home, edit, outcome)
}

// cleanupResourceDescription names the off-box resource a row's cleanup
// identity points at, so a refusal can tell the operator exactly what to remove
// by hand when af cannot drive the reap itself. The preserved-workspace record
// wins when present — it is the resource the row exists to protect — and the
// rejected handle's own fields are the fallback (a tombstoned row may name a
// resource and still be unreadable for want of an engine id or host).
func cleanupResourceDescription(ws *ArchivedWorkspaceData, handle *RuntimeCleanupData) string {
	switch {
	case ws != nil && ws.Dir != "" && ws.Host != "":
		return fmt.Sprintf("its preserved workspace at %s on host %q", ws.Dir, ws.Host)
	case ws != nil && ws.Dir != "":
		return fmt.Sprintf("its preserved workspace at %s on its host", ws.Dir)
	case handle != nil && handle.SSH != nil && handle.SSH.Config.Host != "":
		return fmt.Sprintf("session directory %s on host %q", handle.SSH.SessionDir, handle.SSH.Config.Host)
	case handle != nil && handle.SSH != nil && handle.SSH.SessionDir != "":
		return fmt.Sprintf("session directory %s on its host", handle.SSH.SessionDir)
	case handle != nil && handle.Sandbox != nil && handle.Sandbox.SessionDir != "":
		return fmt.Sprintf("session directory %s on the host its ssh command reaches", handle.Sandbox.SessionDir)
	case handle != nil && handle.Docker != nil && handle.Docker.ContainerID != "":
		return fmt.Sprintf("container %s", handle.Docker.ContainerID)
	case handle != nil && handle.Hook != nil && handle.Hook.Slug != "":
		return fmt.Sprintf("the resource its hook delete_cmd names (slug %q)", handle.Hook.Slug)
	default:
		return "whatever off-box resource the row's runtime_cleanup or archived_workspace fields name"
	}
}
