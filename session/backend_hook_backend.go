package session

import (
	"context"
	"encoding/json"
	"errors"
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
//	    reaps whatever launch_cmd provisioned (the runtime teardown). Best-effort
//	    by contract: it also runs after a launch_cmd that STARTED and then failed
//	    or timed out, which may have left a half-built sandbox — or none at all —
//	    so it must tolerate a slug it cannot find (#1955).
//
// This is the most direct provision-and-expose runtime: no container/tunnel of
// our own, just the user's script handing us a URL. GitHub is still the durable
// workspace store (archive pushes the branch + reaps via delete_cmd, restore
// re-runs launch_cmd to re-provision + re-clone), so hook reaches FULL capability
// parity like docker/ssh — no ErrRecoverUnsupported, no locality special-case.

// The bounds on the user-provided provisioning and teardown scripts. Vars (not
// consts) so a test can shrink them to prove each bound fires.
//
//   - hookLaunchTimeout: launch_cmd may pull an image, spin up a VM, or clone a
//     large repo, so it gets a generous budget.
//   - hookDeleteTimeout: bounded tighter so a kill never hangs on an unreachable
//     provisioner.
//   - hookOutputDrainGrace: how long to wait for a script's output pipe to drain
//     AFTER the script itself has exited or been killed. Without it the two
//     timeouts above bound NOTHING: CombinedOutput waits for the output pipe to
//     close, any process the script leaves behind inherits that pipe and holds it
//     open, and exec.CommandContext kills only the script, not its children. So a
//     launch_cmd that provisions and then hangs would block Provision forever
//     rather than for hookLaunchTimeout — and the #1955 reap could never run at
//     all, because launch() would never return to trigger it. Bounding the drain
//     is what turns "launch_cmd timed out" into an event the reap can react to.
//     The hook scripts are the last exec surface that lacked this bound; it
//     mirrors gitWaitDelay and tmuxWaitDelay (#856/#896), and a user-authored
//     provisioner is the likeliest of all of them to leave a child behind.
var (
	hookLaunchTimeout    = 5 * time.Minute
	hookDeleteTimeout    = 60 * time.Second
	hookOutputDrainGrace = 5 * time.Second
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
	return p.provisionOrReap()
}

// provisionOrReap provisions and, on ANY failure, reaps whatever launch_cmd may
// have created before it failed. It is the whole of hookRuntime.Provision below
// the config load, split out so a test can drive the real reap-on-failure gate
// with a hand-built provisioner — the gate is the thing #1955 was about, and a
// test that re-implemented it would prove nothing about this path.
func (p *hookProvisioner) provisionOrReap() (ProvisionResult, error) {
	res, err := p.provision()
	if err == nil {
		return res, nil
	}
	// launch_cmd may have provisioned a sandbox before failing to hand back a
	// usable endpoint: it can exit 0 having printed no/bad JSON, exit non-zero
	// after creating a VM, or be killed at the timeout mid-provision. Reap via
	// delete_cmd so nothing leaks on a partial failure (#1955).
	if reapErr := p.reap(); reapErr != nil {
		// The reap failed too, so something on the user's account may still be
		// running and billing with no record of it on our side. That has to reach
		// the person creating the session, not just the log.
		return ProvisionResult{}, fmt.Errorf("%w\n\n%s", err, p.orphanWarning(reapErr))
	}
	return ProvisionResult{}, err
}

// orphanWarning is what a user sees when delete_cmd could not reap a sandbox that
// launch_cmd may have provisioned. A leak the user knows about is survivable; a
// silent one is not (#1955) — so it names the session and its slug, says plainly
// that real infrastructure may still be running, and gives the exact command to
// reap it by hand. It goes to ErrorLog AND into the returned provision error,
// since someone creating a session is not reading the daemon log.
//
// Worded to hold on EVERY reap path, not just the provisioning failure that
// motivated it: reap also backs Kill and archive, where launch_cmd succeeded and
// the sandbox certainly existed. "launch_cmd ran, so a sandbox may still be out
// there" is the claim common to all three.
func (p *hookProvisioner) orphanWarning(reapErr error) string {
	return fmt.Sprintf(
		"A sandbox may still be running on your infrastructure — delete_cmd could not reap it, and af will not retry.\n"+
			"launch_cmd ran for session %q (hook name %q), so it may hold real resources: a VM, a pod, a cloud sandbox.\n"+
			"Reap it by hand, then check your provider for anything still running:\n"+
			"    %s\n"+
			"delete_cmd error: %v",
		p.spec.Title, p.slug, p.manualReapCommand(), reapErr)
}

// manualReapCommand is the shell command orphanWarning tells the user to paste.
// Every interpolated value is shell-quoted, because this command has to be
// correct exactly when things are messy — which is exactly when names are weird.
//
// delete_cmd is an arbitrary user-configured path: a space, an apostrophe, a `$`
// or a backtick in it yields a command that fails, or worse, runs something other
// than what it reads like. The slug is already constrained to [a-z0-9-] by
// Slugify, so quoting it is belt-and-braces — but it costs nothing and the safety
// then lives here rather than depending on a caller's charset invariant holding
// forever.
func (p *hookProvisioner) manualReapCommand() string {
	return fmt.Sprintf("%s --name %s", shellQuote(p.hooks.DeleteCmd), shellQuote(p.slug))
}

// normalizeHookWaitDelay converts an exec.ErrWaitDelay into success: the hook
// script itself exited 0 (a non-zero exit surfaces as an ExitError, and a
// timeout kill as a signal — never as ErrWaitDelay), and only a process it left
// behind held the output pipe open past hookOutputDrainGrace. A launch_cmd that
// backgrounds a tunnel to make the agent-server reachable is a documented
// pattern, so this is not a failure — and treating it as one would reap a
// sandbox that came up fine. Success is the script's EXIT STATUS, never
// `err == nil`. Mirrors tmux's normalizeWaitDelay (#676/#914 precedent).
func normalizeHookWaitDelay(err error) error {
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}

// hookProvisioner holds the state of one hook provisioning so its launch step
// and its reap closure share the slug and the "did launch_cmd actually run"
// flag that gates teardown.
type hookProvisioner struct {
	hooks config.RemoteHooks
	spec  ProvisionSpec
	slug  string

	// launchStarted records that the kernel spawned launch_cmd — NOT that it
	// succeeded. It gates the delete_cmd reap, so it must stay the weaker of the
	// two claims: a launch that started and then failed may have provisioned
	// infrastructure, and only delete_cmd can reap it (#1955).
	launchStarted bool
	reapOnce      sync.Once
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
		Backend:  &HookBackend{remoteAgentBackend: remoteAgentBackend{reap: teardown}},
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
	cmd := exec.CommandContext(ctx, p.hooks.LaunchCmd, args...)
	cmd.WaitDelay = hookOutputDrainGrace
	out, err := cmd.CombinedOutput()

	// Gate the reap on whether launch_cmd STARTED, not on whether it succeeded
	// (#1955). A script that creates a VM and then times out or exits non-zero
	// has provisioned real infrastructure on the user's account that only
	// delete_cmd can reap: "it failed" is not evidence that nothing exists, and
	// af keeps no record of a session whose Provision failed, so an unreaped
	// sandbox bills forever with nothing pointing at it.
	//
	// cmd.Process is non-nil exactly when the kernel spawned the process, which
	// is that question answered directly. Do NOT infer it from the error type:
	// only a BARE command name goes through exec.LookPath and yields *exec.Error,
	// and the documented launch_cmd is a path ("./.agent-factory/hooks/launch.sh"),
	// so a missing or non-executable script surfaces as *fs.PathError instead — an
	// *exec.Error check would read "never ran" as "ran" and fire delete_cmd for a
	// typo'd path.
	p.launchStarted = cmd.Process != nil

	if errors.Is(err, exec.ErrWaitDelay) {
		// Exited 0 but left a process holding the output pipe, so the drain grace
		// cut the read short. The endpoint JSON is normally already written, so
		// parse what we captured rather than failing (and reaping) a sandbox that
		// is very likely up.
		log.WarningLog.Printf("hook runtime: launch_cmd for %q exited 0 but left its output pipe open; parsing the output captured so far", p.spec.Title)
	}
	if err := normalizeHookWaitDelay(err); err != nil {
		return nil, fmt.Errorf("launch_cmd failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

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
// and only if launch_cmd actually STARTED — if it never ran, nothing was
// provisioned and there is nothing to delete. It backs the session's Kill (after
// the in-sandbox workspace is torn down over REST), a provisioning failure, and a
// bad-endpoint NewInstance failure — so a provisioned sandbox is never leaked. The
// sync.Once collapses the repeated Kill retries and the Kill-vs-provision-failure
// races to one delete_cmd.
//
// This is best-effort by contract: it may be called for a sandbox that was never
// fully built, so delete_cmd must tolerate a slug it cannot find (documented in
// docs/remote-hooks.md).
func (p *hookProvisioner) reap() error {
	var reapErr error
	p.reapOnce.Do(func() {
		if !p.launchStarted {
			return
		}
		// context.Background(), NEVER a caller's context. reap's whole job is to
		// run on the failure path, where the launch context is already cancelled or
		// expired — and a WithTimeout derived from a dead parent is born expired,
		// so delete_cmd would never spawn and the sandbox would leak in silence.
		// That is the exact failure #1955 is about, reintroduced by the cleanup.
		ctx, cancel := context.WithTimeout(context.Background(), hookDeleteTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, p.hooks.DeleteCmd, "--name", p.slug)
		cmd.WaitDelay = hookOutputDrainGrace
		out, err := cmd.CombinedOutput()
		if err := normalizeHookWaitDelay(err); err != nil {
			reapErr = fmt.Errorf("backend=hook: delete_cmd failed for %q: %s: %w", p.slug, strings.TrimSpace(string(out)), err)
			log.ErrorLog.Printf("%s", p.orphanWarning(reapErr))
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
	remoteAgentBackend
}

var _ Backend = (*HookBackend)(nil)

func (b *HookBackend) Type() string { return "remote" }
