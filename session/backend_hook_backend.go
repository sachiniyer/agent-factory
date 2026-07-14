package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The remote-hook runtime (#1592 Phase 4 PR7) — the bring-your-own-provisioner
// escape hatch, migrated to the SAME provision-and-expose contract as the
// docker/ssh runtimes (a BREAKING clean-break; the old terminal/attach/preview/
// enumeration machinery is deleted). Where the docker runtime runs a container
// and the ssh runtime dials a host, the hook runtime shells out to a
// user-provided launch_cmd that provisions the workspace on WHATEVER infra the
// user owns (k8s, Modal, Daytona, a bespoke orchestrator) and starts an
// `af agent-server` (PR1) there, echoing that server's authed http:// URL. The
// daemon then drives it through the remoteAgentServer HTTP/WS client (PR2)
// exactly as it drives a docker/ssh (or local) session — no hook attach proxy,
// no preview capture, no per-config terminal gating.
//
// Contract (docs/remote-hooks.md):
//
//	launch_cmd --name <slug> --title <title> --repo <cloneURL> \
//	           [--branch <branch>] [--program <p>] [--auto-yes]
//	    clones <cloneURL> (repo@branch on RESTORE) on the user's infra, starts
//	    `af agent-server --listen :PORT --repo <clonedir> --title <title> …`
//	    there, and echoes ONE JSON object on stdout:
//	        {"url":"http://host:port","token":"…"}
//	    The agent-server is HTTP-only; the URL must be http:// (or ws://). The
//	    token travels over the plaintext connection, so the launch_cmd must reach
//	    the agent-server over a private network / tunnel it controls.
//	delete_cmd --name <slug>
//	    reaps whatever launch_cmd provisioned (the runtime teardown).
//
// This is the most direct provision-and-expose runtime: no container/tunnel of
// our own, just the user's script handing us a URL. GitHub is still the durable
// workspace store (archive pushes the branch + reaps via delete_cmd, restore
// re-runs launch_cmd to re-provision + re-clone), so hook reaches FULL capability
// parity like docker/ssh — no ErrRecoverUnsupported, no locality special-case.

const (
	// hookLaunchTimeout / hookDeleteTimeout bound the user-provided provisioning
	// and teardown scripts. launch_cmd may pull an image, spin up a VM, or clone a
	// large repo, so it gets a generous budget; delete_cmd is bounded tighter so a
	// kill never hangs on an unreachable provisioner.
	hookLaunchTimeout = 5 * time.Minute
	hookDeleteTimeout = 60 * time.Second
)

// hookRuntime provisions a session on user-provided infrastructure via the
// remote_hooks scripts (#1592 Phase 4 PR7). Declared in runtime.go's registry;
// its Provision lives here (it replaces the pre-Phase-4 ForceRemote HookBackend
// construction, which allocated a remote session id + terminal metadata).
type hookRuntime struct{}

func (hookRuntime) Provision(spec ProvisionSpec) (ProvisionResult, error) {
	hooks, err := loadRemoteHooksForPath(spec.RepoRoot)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=hook: %w", err)
	}
	p := &hookProvisioner{hooks: hooks, spec: spec, slug: Slugify(spec.Title)}
	res, err := p.provision()
	if err != nil {
		// launch_cmd may have provisioned a sandbox before failing to hand back a
		// usable endpoint (e.g. it started the af agent-server but printed no/bad
		// JSON); reap it via delete_cmd so nothing leaks on a partial failure.
		p.reap()
		return ProvisionResult{}, err
	}
	return res, nil
}

// hookProvisioner holds the state of one hook provisioning so its launch step
// and its reap closure share the slug and the "did launch_cmd actually run"
// flag that gates teardown.
type hookProvisioner struct {
	hooks config.RemoteHooks
	spec  ProvisionSpec
	slug  string

	launched bool
	reapOnce sync.Once
}

// hookEndpointJSON is the object launch_cmd echoes: the authed `af agent-server`
// endpoint the daemon dials — the same {url,token} docker/ssh read from their
// in-sandbox agent-server banner, here handed back by the user's script instead.
//
// TLSFingerprint is accepted but IGNORED: af removed TLS, so a fingerprint is
// meaningless now. It stays in the struct only so a launch_cmd written against
// the old TLS contract (which echoed "tls_fingerprint") still parses without
// error — the field is dropped, not honored. New scripts should omit it and echo
// an http:// URL.
type hookEndpointJSON struct {
	URL            string `json:"url"`
	Token          string `json:"token"`
	TLSFingerprint string `json:"tls_fingerprint"`
}

// provision runs launch_cmd, parses the endpoint it echoes, and returns the
// wiring a hook session needs: an inert HookBackend (its one local job is the
// delete_cmd reap), the authed endpoint the daemon dials, and the teardown.
func (p *hookProvisioner) provision() (ProvisionResult, error) {
	ep, err := p.launch()
	if err != nil {
		return ProvisionResult{}, err
	}
	teardown := p.reap
	log.InfoLog.Printf("hook runtime: session %q provisioned via launch_cmd, agent-server at %s", p.spec.Title, ep.URL)
	return ProvisionResult{
		Backend:  &HookBackend{reap: teardown},
		Endpoint: ep,
		Teardown: teardown,
	}, nil
}

// launch runs the user's launch_cmd with the provision spec as flags, then
// recovers the {url,token} JSON it echoes (stderr may interleave progress, so
// extractJSON pulls the object out of the combined output, mirroring how
// docker/ssh poll their agent-server banner file).
func (p *hookProvisioner) launch() (*AgentServerEndpoint, error) {
	args := []string{
		"--name", p.slug,
		"--title", p.spec.Title,
		"--repo", p.spec.CloneURL,
	}
	if branch := strings.TrimSpace(p.spec.RestoreBranch); branch != "" {
		args = append(args, "--branch", branch)
	}
	if prog := strings.TrimSpace(p.spec.Program); prog != "" {
		args = append(args, "--program", prog)
	}
	if p.spec.AutoYes {
		args = append(args, "--auto-yes")
	}

	ctx, cancel := context.WithTimeout(context.Background(), hookLaunchTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, p.hooks.LaunchCmd, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("launch_cmd failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	// launch_cmd exited 0, so it may have provisioned a sandbox; from here any
	// failure must trigger the delete_cmd reap (via the caller's p.reap()).
	p.launched = true

	jsonStr := extractJSON(string(out))
	if jsonStr == "" {
		return nil, fmt.Errorf("launch_cmd returned no {\"url\",\"token\"} JSON in its output (see docs/remote-hooks.md for the recipe): %s", strings.TrimSpace(string(out)))
	}
	var ej hookEndpointJSON
	if err := json.Unmarshal([]byte(jsonStr), &ej); err != nil {
		return nil, fmt.Errorf("launch_cmd returned invalid endpoint JSON: %s: %w", jsonStr, err)
	}
	if strings.TrimSpace(ej.URL) == "" || strings.TrimSpace(ej.Token) == "" {
		return nil, fmt.Errorf("launch_cmd endpoint JSON is missing url or token (got %s); it must echo the af agent-server's {\"url\",\"token\"}", jsonStr)
	}
	// ej.TLSFingerprint is intentionally not read — TLS was removed; an old
	// script that still echoes it parses fine and the value is dropped.
	return &AgentServerEndpoint{
		URL:   ej.URL,
		Token: ej.Token,
	}, nil
}

// reap runs delete_cmd to tear down whatever launch_cmd provisioned, idempotently
// and only if launch_cmd actually ran (nothing to delete otherwise). It backs the
// session's Kill (after the in-sandbox workspace is torn down over REST), a
// provisioning failure, and a bad-endpoint NewInstance failure — so a provisioned
// sandbox is never leaked. The sync.Once collapses the repeated Kill retries and
// the Kill-vs-provision-failure races to one delete_cmd.
func (p *hookProvisioner) reap() error {
	var reapErr error
	p.reapOnce.Do(func() {
		if !p.launched {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), hookDeleteTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, p.hooks.DeleteCmd, "--name", p.slug).CombinedOutput()
		if err != nil {
			reapErr = fmt.Errorf("backend=hook: delete_cmd failed for %q: %s: %w", p.slug, strings.TrimSpace(string(out)), err)
			log.WarningLog.Printf("%v", reapErr)
			return
		}
		log.InfoLog.Printf("hook runtime: reaped remote session %q via delete_cmd", p.slug)
	})
	return reapErr
}

// HookBackend is the in-process Backend for a remote-hook session (#1592 Phase 4
// PR7). Like sshBackend/dockerBackend, its agent-facing operations delegate to
// the instance's remote AgentServer (the HTTP/WS client to the user-provisioned
// `af agent-server`) — so lifecycle, preview, prompt, and liveness all go over
// the wire. Its ONE local responsibility is running delete_cmd to reap the
// provisioned sandbox, shared via the same idempotent closure with the
// AgentServer Kill path.
//
// It stays EXPORTED (unlike sshBackend/dockerBackend) because it is the public
// bring-your-own-provisioner escape hatch and the canonical remote-backend
// stand-in in cross-package tests. A zero-value &HookBackend{} is a valid INERT
// hook backend (nil reap — nothing live to tear down), which is exactly what
// FromInstanceData rebuilds for a "remote" record loaded from disk and what
// restore replaces wholesale via a fresh hookRuntime.Provision.
type HookBackend struct {
	// reap runs delete_cmd to tear down the provisioned sandbox; shared with the
	// runtime's Teardown / the AgentServer Kill path, and idempotent (a sync.Once
	// behind the closure). nil for an inert backend loaded from disk.
	reap func() error
}

var _ Backend = (*HookBackend)(nil)

func (b *HookBackend) Type() string { return "remote" }

// Capabilities advertises FULL parity for the remote-hook backend (#1592 Phase 4
// PR7, epic §5) — identical to docker/ssh: the workspace is off-box, but
// attach/preview/liveness/prompt/tabs work through the user-provisioned
// agent-server, and Archive/Recover work by pushing the branch to GitHub and
// re-running launch_cmd to re-provision a fresh sandbox that clones it back
// (§5.1). Every capability is true — no ErrRecoverUnsupported, no per-config
// TerminalTab gating (the in-sandbox agent-server manages tabs natively, so the
// old terminal_cmd bit is gone).
func (b *HookBackend) Capabilities() Capabilities {
	return Capabilities{
		Workspace:        WorkspaceRemote,
		Attach:           true,
		Archive:          true,
		Recover:          true,
		TabManagement:    true,
		TerminalTab:      true,
		InteractiveInput: true,
	}
}

// Start provisions then launches the remote workspace. The sandbox +
// agent-server were established by the runtime (during NewInstance); Start drives
// the agent-server's own provision/launch over REST, so the in-sandbox LOCAL
// backend creates the git worktree + branch and spawns the agent.
func (b *HookBackend) Start(i *Instance, firstTimeSetup bool) error {
	if err := b.Provision(i, firstTimeSetup); err != nil {
		return err
	}
	return b.Launch(i, firstTimeSetup)
}

// Provision drives the remote agent-server's Provision over the wire (the clone
// is already done by launch_cmd; this creates the in-sandbox git worktree).
func (b *HookBackend) Provision(i *Instance, firstTimeSetup bool) error {
	return i.AgentServer().Provision(firstTimeSetup)
}

// Launch drives the remote agent-server's Launch (spawn the agent), sets the
// started flag, and seeds the daemon-side tab model with the agent tab (the
// sandbox owns the real tabs; the daemon-side list mirrors the agent tab so the
// UI renders it, matching the docker/ssh baseline).
func (b *HookBackend) Launch(i *Instance, firstTimeSetup bool) error {
	if err := i.AgentServer().Launch(firstTimeSetup); err != nil {
		return err
	}
	i.mu.Lock()
	i.started = true
	if len(i.Tabs) == 0 {
		i.Tabs = []*Tab{newRemoteAgentTab()}
	}
	i.mu.Unlock()
	return nil
}

// Kill runs the delete_cmd reap. Instance.Kill routes through the AgentServer
// (which tears the in-sandbox workspace down over REST and then runs the same
// reap), so this is normally reached only if something calls backend.Kill
// directly; the shared idempotent reap makes either path safe.
func (b *HookBackend) Kill(i *Instance) error {
	i.mu.Lock()
	i.started = false
	i.mu.Unlock()
	if b.reap != nil {
		return b.reap()
	}
	return nil
}

// CloseAttachOnly discards this instance's local view WITHOUT running delete_cmd
// — used to drop a duplicate Instance built from disk that lost a race to the
// canonical one (#867). Reaping here would tear down the sandbox the canonical
// Instance shares, so it must not run.
func (b *HookBackend) CloseAttachOnly(i *Instance) error {
	i.mu.Lock()
	i.started = false
	i.mu.Unlock()
	return nil
}

func (b *HookBackend) Preview(i *Instance) (string, error) {
	return i.AgentServer().Preview(0, false)
}

func (b *HookBackend) PreviewFullHistory(i *Instance) (string, error) {
	return i.AgentServer().Preview(0, true)
}

// Attach/AttachTerminal: a hook session attaches CLIENT-side over the WS PTY
// stream (the daemon proxies the sandbox's stream), exactly like a docker/ssh or
// local session — the client's attach dispatch branches on
// Capabilities().Workspace and never reaches the backend. These satisfy the
// interface with an explicit routing-invariant error rather than a silent no-op.
func (b *HookBackend) Attach(*Instance) (chan struct{}, error) {
	return nil, fmt.Errorf("hook sessions attach client-side over the WS PTY stream, not through the backend")
}

func (b *HookBackend) AttachTerminal(*Instance, int) (chan struct{}, error) {
	return nil, fmt.Errorf("hook terminal tabs attach client-side over the WS PTY stream, not through the backend")
}

func (b *HookBackend) HasUpdated(i *Instance) (updated bool, hasPrompt bool, content string) {
	obs, err := i.AgentServer().Snapshot()
	if err != nil {
		return false, false, ""
	}
	return obs.Updated, obs.HasPrompt, obs.Content
}

func (b *HookBackend) SendPromptCommand(i *Instance, prompt string) error {
	return i.AgentServer().SendPrompt(prompt)
}

func (b *HookBackend) IsAlive(i *Instance) bool {
	return i.AgentServer().Alive()
}

// CheckAndHandleTrustPrompt is a daemon-side no-op: the in-sandbox agent-server
// dismisses trust/permission prompts itself on every Snapshot (its localAgentServer
// runs CheckAndHandleTrustPrompt before reading the pane), so there is nothing for
// the daemon to do over the wire.
func (b *HookBackend) CheckAndHandleTrustPrompt(*Instance) bool { return false }

func (b *HookBackend) TapEnter(i *Instance) { i.AgentServer().TapEnter() }

// Recover/Respawn re-establish a hook session by RE-PROVISIONING via launch_cmd
// (which clones the session's branch back from origin), then relaunching the
// agent (#1592 Phase 4 PR7) — the same recoverSandbox docker/ssh use (written
// once against the Runtime interface). The old HookBackend returned
// ErrRecoverUnsupported here; provision-and-expose makes recovery a fresh
// re-provision of the durable branch on GitHub, so hook reaches parity.
func (b *HookBackend) Recover(i *Instance) error { return recoverSandbox(i) }
func (b *HookBackend) Respawn(i *Instance) error { return recoverSandbox(i) }
