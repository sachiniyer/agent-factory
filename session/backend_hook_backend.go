package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
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
//	           [--branch <branch>] [--program <p>]
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
//
// These bound the SCRIPT, and only the script. Nothing here bounds — or touches
// — a process the script deliberately leaves running: see runHookScript.
var (
	hookLaunchTimeout = 5 * time.Minute
	hookDeleteTimeout = 60 * time.Second
)

// hookNoAgentEnvironmentProgram is an internal selector, never an executed
// command. It lets a persisted hook cleanup retain "no known agent" across a
// daemon restart without the legacy empty-program fallback admitting Claude
// credentials.
const hookNoAgentEnvironmentProgram = "__af_no_agent_environment__"

// hookRuntime provisions a session on user-provided infrastructure via the
// remote_hooks scripts (#1592 Phase 4 PR7). Declared in runtime.go's registry;
// its Provision lives here (it replaces the pre-Phase-4 ForceRemote HookBackend
// construction, which allocated a remote session id + terminal metadata).
type hookRuntime struct{}

func (hookRuntime) Provision(spec ProvisionSpec) (ProvisionResult, error) {
	resolved, err := resolveRepoConfig(spec.RepoRoot)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=hook: failed to resolve repo config: %w", err)
	}
	if resolved.RemoteHooks == nil {
		return ProvisionResult{}, fmt.Errorf("backend=hook: no remote hooks configured")
	}
	if err := resolved.RemoteHooks.Validate(); err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=hook: %w", err)
	}
	hooks := *resolved.RemoteHooks
	p := &hookProvisioner{
		hooks:   hooks,
		spec:    spec,
		slug:    Slugify(spec.Title),
		program: config.ResolveProgram(&resolved.Config, spec.Program),
	}
	return p.provisionOrReap()
}

// provisionOrReap provisions and, on ANY failure, reaps whatever launch_cmd may
// have created before it failed. It is the whole of hookRuntime.Provision below
// the config load, split out so a test can drive the real reap-on-failure gate
// with a hand-built provisioner — the gate is the thing #1955 was about, and a
// test that re-implemented it would prove nothing about this path.
func (p *hookProvisioner) provisionOrReap() (ProvisionResult, error) {
	p.resolveAuthSelectors()
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
// It goes through the shellsuggest seam (#1978), which quotes every piece: this
// command has to be correct exactly when things are messy, which is exactly when
// names are weird.
//
// delete_cmd is an arbitrary user-configured path — a space, an apostrophe, a `$`
// or a backtick in it yields a command that fails, or worse, runs something other
// than what it reads like. The slug is already constrained to [a-z0-9-] by
// Slugify, so quoting it is belt-and-braces; the seam does it anyway, so the
// safety lives at the print site rather than depending on a caller's charset
// invariant holding forever.
func (p *hookProvisioner) manualReapCommand() string {
	return shellsuggest.Command(p.hooks.DeleteCmd, "--name", p.slug)
}

// runHookScriptWithEnvironment runs one hook script under a timeout and returns
// its combined output. It exists to answer a question the obvious CombinedOutput() gets wrong:
// WHICH CHILDREN ARE OURS TO KILL?
//
// Answer: only the script itself. A launch_cmd is DOCUMENTED to leave a tunnel or
// port-forward running — that background process is not a leak, it is the product,
// the very thing making the endpoint we just captured reachable. Reaping it would
// destroy what the launch just built and leave a session that exists and cannot
// be dialled, with nothing saying why.
//
// The pipe is what conflates the two. CombinedOutput gives the script a PIPE, and
// then:
//   - anything the script leaves behind inherits that pipe and holds it open, so
//     Wait blocks on EOF and the timeout above bounds nothing (#1943's class); and
//   - the cure for that, cmd.WaitDelay, KILLS the pipe-holder. But "still holds
//     the pipe" is not a criterion for garbage, it is a coincidence — it is
//     equally true of a stalled child and of a healthy tunnel. Measured: with
//     WaitDelay, a successful launch_cmd's tunnel is dead within the grace.
//
// So do not use a pipe. The script's stdout and stderr go to a real FILE, whose
// fd exec hands to the child directly — no pipe, no copier goroutine, nothing for
// a survivor to hold open. Wait returns the moment the SCRIPT exits, the context
// still kills the script if it hangs (verified: a hanging launch_cmd returns at
// the deadline with no WaitDelay at all), and a tunnel that outlives it keeps
// writing to a file nobody is reading. We stop listening; we kill nothing.
//
// Reaping a FAILED launch's sandbox is a separate act with a real criterion, and
// it is delete_cmd's job, not a side effect of how we captured output: see reap.
func runHookScriptWithEnvironment(timeout time.Duration, name, program string, passthrough []string, args ...string) ([]byte, *exec.Cmd, error) {
	agentName := sessionenv.AgentForCommand(program)
	if agentName == "" && strings.TrimSpace(program) == "" {
		agentName = tmux.ProgramClaude
	}
	authSelectors := sessionenv.ResolveAuthSelectors(os.Environ(), agentName, program)
	return runHookScriptWithResolvedEnvironment(timeout, name, agentName, authSelectors, passthrough, args...)
}

func runHookScriptWithResolvedEnvironment(timeout time.Duration, name, agent string, authSelectors, passthrough []string, args ...string) ([]byte, *exec.Cmd, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// A regular file, not a pipe: see the doc comment. Unlinked immediately on
	// return — a lingering writer keeps its fd valid and simply writes into an
	// unlinked inode that disappears when it exits.
	f, err := os.CreateTemp("", "af-hook-out-*")
	if err != nil {
		return nil, nil, fmt.Errorf("creating the hook output file failed: %w", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()

	cmd := exec.CommandContext(ctx, name, args...)
	filtered, err := sessionenv.FilterWithAuthSelectors(os.Environ(), agent, authSelectors, passthrough)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving the hook environment policy failed: %w", err)
	}
	cmd.Env = filtered
	cmd.Stdout = f
	cmd.Stderr = f
	runErr := cmd.Run()

	out, readErr := os.ReadFile(f.Name())
	if readErr != nil && runErr == nil {
		return nil, cmd, fmt.Errorf("reading the hook output failed: %w", readErr)
	}
	return out, cmd, runErr
}

// hookProvisioner holds the state of one hook provisioning so its launch step
// and its reap closure share the slug and the "did launch_cmd actually run"
// flag that gates teardown.
type hookProvisioner struct {
	hooks config.RemoteHooks
	spec  ProvisionSpec
	slug  string
	// program is the resolved command used to select the environment allowlist
	// and as the command handed to the remote agent-server.
	program string
	// authSelectors is a value-free snapshot of the resolved conditional
	// provider modes. It keeps launch and delete on the same allowlist and is
	// safe to persist in the durable cleanup handle.
	authSelectors         []string
	authSelectorsResolved bool

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
		Backend: &HookBackend{
			remoteAgentBackend: remoteAgentBackend{reap: teardown},
			provisioner:        p,
			cleanup:            p.cleanupData(),
		},
		Endpoint: ep,
		Teardown: teardown,
	}, nil
}

func (p *hookProvisioner) cleanupData() *HookRuntimeCleanupData {
	return &HookRuntimeCleanupData{
		DeleteCmd:             p.hooks.DeleteCmd,
		Slug:                  p.slug,
		Agent:                 p.environmentAgent(),
		AgentResolved:         true,
		AuthSelectors:         append([]string(nil), p.authSelectors...),
		AuthSelectorsResolved: true,
		SessionEnvPassthrough: append([]string(nil), p.spec.SessionEnvPassthrough...),
	}
}

// hookOutputSuffix renders a hook script's combined output for an error
// message, and says so explicitly when there was none.
//
// launch_cmd runs on the user's own infrastructure, so its output is the ONLY
// window onto what went wrong out there — af has no other source. When a Mac
// user's script died on `setsid: command not found`, that line was the entire
// diagnosis, and everything af could usefully say was a quote of it (#1946).
//
// Empty output gets named rather than left to inference: "launch_cmd failed:
// exit status 1" with nothing after it reads like af truncated something. Saying
// the script printed nothing points the reader at their script's own error
// handling rather than at af. (The timeout case is carried by the wrapped error
// from runHookScript — "signal: killed" — so it is not re-derived here.)
func hookOutputSuffix(out []byte) string {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return "; it printed nothing — a hook script must report its own errors, " +
			"or the cause reaches nobody (see docs/remote-hooks.md)"
	}
	return fmt.Sprintf("; its output was:\n%s", trimmed)
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
	if prog := strings.TrimSpace(p.environmentProgram()); prog != "" {
		args = append(args, "--program", prog)
		args = append(args, "--program-resolved")
	}
	for _, name := range p.spec.SessionEnvPassthrough {
		args = append(args, "--session-env", name)
	}

	out, cmd, err := runHookScriptWithResolvedEnvironment(hookLaunchTimeout, p.hooks.LaunchCmd,
		p.environmentAgent(), p.authSelectors, p.spec.SessionEnvPassthrough, args...)

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
	p.launchStarted = cmd != nil && cmd.Process != nil

	if err != nil {
		// "launch_cmd failed" stays a contiguous phrase: #1955's reap tests
		// assert the original provisioning error is not swallowed by matching on
		// it. The path is added AFTER, so #1946's diagnostic detail and #1955's
		// contract both hold.
		return nil, fmt.Errorf("launch_cmd failed (%s): %w%s", p.hooks.LaunchCmd, err,
			hookOutputSuffix(out))
	}

	jsonStr := extractJSON(string(out))
	if jsonStr == "" {
		return nil, fmt.Errorf("launch_cmd (%s) exited 0 but printed no {\"url\",\"token\"} JSON "+
			"(see docs/remote-hooks.md for the recipe)%s", p.hooks.LaunchCmd, hookOutputSuffix(out))
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
		p.resolveAuthSelectors()
		// runHookScript builds its timeout from context.Background(), NEVER a
		// caller's context — and that is load-bearing here. reap's whole job is to
		// run on the failure path, where the launch context is already cancelled or
		// expired, and a WithTimeout derived from a dead parent is born expired:
		// delete_cmd would never spawn and the sandbox would leak in silence. That
		// is the exact failure #1955 is about, reintroduced by the cleanup.
		out, _, err := runHookScriptWithResolvedEnvironment(hookDeleteTimeout, p.hooks.DeleteCmd,
			p.environmentAgent(), p.authSelectors, p.spec.SessionEnvPassthrough, "--name", p.slug)
		if err != nil {
			reapErr = fmt.Errorf("backend=hook: delete_cmd failed for %q: %s: %w", p.slug, strings.TrimSpace(string(out)), err)
			log.ErrorLog.Printf("%s", p.orphanWarning(reapErr))
			return
		}
		log.InfoLog.Printf("hook runtime: reaped remote session %q via delete_cmd", p.slug)
	})
	return reapErr
}

func (p *hookProvisioner) environmentProgram() string {
	if strings.TrimSpace(p.program) != "" {
		return p.program
	}
	return p.spec.Program
}

func (p *hookProvisioner) environmentAgent() string {
	agent := sessionenv.AgentForCommand(p.environmentProgram())
	if agent == "" && strings.TrimSpace(p.environmentProgram()) == "" {
		return tmux.ProgramClaude
	}
	return agent
}

func (p *hookProvisioner) resolveAuthSelectors() {
	if p.authSelectorsResolved {
		return
	}
	p.authSelectors = sessionenv.ResolveAuthSelectors(os.Environ(), p.environmentAgent(), p.environmentProgram())
	p.authSelectorsResolved = true
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
	// provisioner owns the concrete reaper; cleanup is its immutable storage-only
	// identity. Both are nil for an ordinary inert backend.
	provisioner *hookProvisioner
	cleanup     *HookRuntimeCleanupData
}

var _ Backend = (*HookBackend)(nil)

func (b *HookBackend) Type() string { return "remote" }
