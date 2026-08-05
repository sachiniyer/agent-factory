package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
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
// — a process the script deliberately leaves running on the SUCCESS path: see
// runHookScript. On the REAP path that changes, and deliberately so:
// quiesceLaunchGroup tears the whole launch tree down under hookQuiesceTimeout
// before delete_cmd runs, because a session being torn down has no product left
// to protect (#2440).
var (
	hookLaunchTimeout = 5 * time.Minute
	hookDeleteTimeout = 60 * time.Second

	// hookQuiesceTimeout bounds the wait for a SIGKILLed launch group to leave
	// the process table before delete_cmd runs. Short by design: the signal
	// cannot be refused, so this covers descheduling only.
	hookQuiesceTimeout = 2 * time.Second
	// hookQuiescePoll is how often that drain is re-checked.
	hookQuiescePoll = 5 * time.Millisecond
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
	// Stop anything launch_cmd left mid-provision BEFORE delete_cmd runs, so the
	// reap is authoritative rather than a snapshot something can outlive (#2440).
	// This belongs here and not in reap: it is sound only while the launch we are
	// cleaning up after has JUST returned. reap also backs Kill and archive,
	// where the launch ended long ago and its group id may since have been
	// recycled onto an unrelated process.
	p.quiesceLaunchGroup()

	// launch_cmd may have provisioned a sandbox before failing to hand back a
	// usable endpoint: it can exit 0 having printed no/bad JSON, exit non-zero
	// after creating a VM, or be killed at the timeout mid-provision. Reap via
	// delete_cmd so nothing leaks on a partial failure (#1955).
	if reapErr := p.reap(); reapErr != nil {
		// The reap failed too, so something on the user's account may still be
		// running and billing with no record of it on our side. That has to reach
		// the person creating the session, not just the log. errors.Join keeps
		// reapErr's ErrWorkspaceStateUnknown sentinel classifiable rather than
		// flattening it into %s text — the hook/docker/ssh unknown-state parity this
		// path is about (ssh does the same in reapProvisionFailure); orphanWarning
		// carries the human-actionable detail.
		return ProvisionResult{}, fmt.Errorf("%w\n\n%s", errors.Join(err, reapErr), p.orphanWarning(reapErr))
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
//
// The two streams go to two SEPARATE files. That is not a detail: docs/remote-hooks.md
// has always said "only launch_cmd's JSON on stdout matters" and "keep non-JSON
// output on stderr", but a single shared file threw that contract away before the
// parser ever saw it. A launch_cmd logging JSON to stderr could then win the
// endpoint (#2637). Two regular files keep every property the comment above is
// about — still no pipe, still nothing a surviving tunnel can hold open — while
// letting launch read the stream the docs actually promise.
func runHookScriptWithEnvironment(timeout time.Duration, name, program string, passthrough []string, args ...string) (hookScriptOutput, *exec.Cmd, error) {
	agentName := sessionenv.AgentForCommand(program)
	if agentName == "" && strings.TrimSpace(program) == "" {
		agentName = tmux.ProgramClaude
	}
	authSelectors := sessionenv.ResolveAuthSelectors(os.Environ(), agentName, program)
	return runHookScriptWithResolvedEnvironment(timeout, name, agentName, authSelectors, passthrough, args...)
}

// hookScriptOutput is one hook script's captured output, kept per-stream because
// the endpoint contract is a STDOUT contract (docs/remote-hooks.md). Combined is
// only for diagnostics, where both streams are worth showing.
type hookScriptOutput struct {
	Stdout []byte
	Stderr []byte
}

// Combined renders both streams for an error message. Interleaving is not
// recoverable from two files and is not worth a pipe to regain: stderr carries
// the script's narrative and stdout carries at most the endpoint line, so
// concatenating them loses nothing a reader was relying on.
func (o hookScriptOutput) Combined() []byte {
	switch {
	case len(o.Stderr) == 0:
		return o.Stdout
	case len(o.Stdout) == 0:
		return o.Stderr
	}
	combined := make([]byte, 0, len(o.Stderr)+len(o.Stdout)+1)
	combined = append(combined, bytes.TrimRight(o.Stderr, "\n")...)
	combined = append(combined, '\n')
	return append(combined, o.Stdout...)
}

func runHookScriptWithResolvedEnvironment(timeout time.Duration, name, agent string, authSelectors, passthrough []string, args ...string) (hookScriptOutput, *exec.Cmd, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Regular files, not pipes: see the doc comment. Unlinked immediately on
	// return — a lingering writer keeps its fd valid and simply writes into an
	// unlinked inode that disappears when it exits.
	stdoutFile, stderrFile, cleanup, err := createHookOutputFiles()
	if err != nil {
		return hookScriptOutput{}, nil, err
	}
	defer cleanup()

	cmd := exec.CommandContext(ctx, name, args...)
	filtered, err := sessionenv.FilterWithAuthSelectors(os.Environ(), agent, authSelectors, passthrough)
	if err != nil {
		return hookScriptOutput{}, nil, fmt.Errorf("resolving the hook environment policy failed: %w", err)
	}
	cmd.Env = filtered
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile
	// Lead a process group so that a FAILED launch's descendants can be torn down
	// as a TREE before delete_cmd reaps — see quiesceLaunchGroup. Setting the
	// group signals nothing by itself: a successful launch_cmd's tunnel is never
	// killed, so the guarantee above is untouched.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	runErr := cmd.Run()
	if runErr != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// The context deadline killed the script mid-run, so whatever it was doing
		// to the remote workspace is UNKNOWN. exec surfaces this as a bare
		// "signal: killed" that does NOT wrap context.DeadlineExceeded, so wrap it
		// here — that is the only place the ctx is in scope — letting callers (reap
		// in particular) tell a timeout from a script that answered, and retain the
		// record instead of trusting a success that never happened (#2529).
		runErr = fmt.Errorf("%w: %w", context.DeadlineExceeded, runErr)
	}

	stdout, readErr := os.ReadFile(stdoutFile.Name())
	if readErr != nil && runErr == nil {
		return hookScriptOutput{}, cmd, fmt.Errorf("reading the hook output failed: %w", readErr)
	}
	stderr, readErr := os.ReadFile(stderrFile.Name())
	if readErr != nil && runErr == nil {
		return hookScriptOutput{}, cmd, fmt.Errorf("reading the hook output failed: %w", readErr)
	}
	return hookScriptOutput{Stdout: stdout, Stderr: stderr}, cmd, runErr
}

func createHookOutputFiles() (*os.File, *os.File, func(), error) {
	stdoutFile, err := os.CreateTemp("", "af-hook-stdout-*")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating the hook output file failed: %w", err)
	}
	stderrFile, err := os.CreateTemp("", "af-hook-stderr-*")
	if err != nil {
		_ = stdoutFile.Close()
		_ = os.Remove(stdoutFile.Name())
		return nil, nil, nil, fmt.Errorf("creating the hook output file failed: %w", err)
	}
	return stdoutFile, stderrFile, func() {
		_ = stdoutFile.Close()
		_ = os.Remove(stdoutFile.Name())
		_ = stderrFile.Close()
		_ = os.Remove(stderrFile.Name())
	}, nil
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

	// launchPgid is the process group launch_cmd led, which is its own pid
	// (runHookScript sets Setpgid). It is what lets the reap tear down a failed
	// launch's still-running descendants BEFORE delete_cmd runs — see
	// quiesceLaunchGroup. Zero when launch_cmd never ran, or when this
	// provisioner was rebuilt for a Kill/archive rather than the launch itself.
	launchPgid int

	// Reap memoizes only an attempt that COMPLETED — a success or a delete_cmd that
	// ANSWERED with an error. A TIMEOUT deliberately does NOT latch: it leaves the
	// remote workspace state unknown, so the daemon must retain the record and
	// actually re-run delete_cmd on its next poll. sync.Once cannot express that
	// conditional latch — it latched the timeout too, so the second reap skipped the
	// closure and returned nil (reapErr was a per-call local), the record was
	// deleted, and the sandbox leaked exactly one poll later (#2529), the same
	// #2063-review defect docker and ssh already fixed this way.
	reapMu  sync.Mutex
	reaped  bool
	reapErr error
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
// message, redacting JSON token fields, and says so explicitly when there was
// no output.
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
	trimmed := strings.TrimSpace(redactHookOutputTokens(string(out)))
	if trimmed == "" {
		return "; it printed nothing — a hook script must report its own errors, " +
			"or the cause reaches nobody (see docs/remote-hooks.md)"
	}
	return fmt.Sprintf("; its output was:\n%s", trimmed)
}

func decodeHookEndpointJSON(candidate string) (hookEndpointJSON, bool) {
	decoder := json.NewDecoder(strings.NewReader(candidate))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return hookEndpointJSON{}, false
	}

	fields := make(map[string]struct{}, 3)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return hookEndpointJSON{}, false
		}
		key, ok := token.(string)
		if !ok {
			return hookEndpointJSON{}, false
		}
		switch key {
		case "url", "token", "tls_fingerprint":
		default:
			return hookEndpointJSON{}, false
		}
		if _, duplicate := fields[key]; duplicate {
			return hookEndpointJSON{}, false
		}
		fields[key] = struct{}{}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return hookEndpointJSON{}, false
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return hookEndpointJSON{}, false
	}
	if _, ok := fields["url"]; !ok {
		return hookEndpointJSON{}, false
	}
	if _, ok := fields["token"]; !ok {
		return hookEndpointJSON{}, false
	}

	var endpoint hookEndpointJSON
	if err := json.Unmarshal([]byte(candidate), &endpoint); err != nil {
		return hookEndpointJSON{}, false
	}
	if strings.TrimSpace(endpoint.URL) == "" || strings.TrimSpace(endpoint.Token) == "" {
		return hookEndpointJSON{}, false
	}
	return endpoint, true
}

// launch runs the user's launch_cmd with the provision spec as flags, then
// PARSES the {url,token} JSON it echoes on STDOUT — the stream
// docs/remote-hooks.md reserves exclusively for it. stderr is the script's own
// narrative and is never a source of endpoints, so a launch_cmd that logs JSON
// there cannot outrank the real record (#2637).
//
// stdout being the endpoint's alone is what makes this a parse. While it was
// shared with a background tunnel, deciding whether a given line was the record
// or a log was UNDECIDABLE — #2845 exhibits two input pairs identical on every
// property a classifier can inspect that require opposite handling — and the
// dangerous half of that guess was dialing a URL with a bearer token both lifted
// from a log line. Reserving the stream deletes the question instead of ranking
// answers to it.
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
	// Assigned unconditionally, INCLUDING the zero: launchPgid must describe the
	// launch that just returned and nothing earlier. Leaving a previous attempt's
	// value in place when this one never spawned would let quiesceLaunchGroup
	// signal a group that is long gone — and whose id the kernel may since have
	// handed to an unrelated process.
	p.launchPgid = 0
	if p.launchStarted {
		// Setpgid made the child its own group leader, so the group id IS its pid.
		p.launchPgid = cmd.Process.Pid
	}

	if err != nil {
		// "launch_cmd failed" stays a contiguous phrase: #1955's reap tests
		// assert the original provisioning error is not swallowed by matching on
		// it. The path is added AFTER, so #1946's diagnostic detail and #1955's
		// contract both hold.
		return nil, fmt.Errorf("launch_cmd failed (%s): %w%s", p.hooks.LaunchCmd, err,
			hookOutputSuffix(out.Combined()))
	}

	endpoint, sawJSON, violation := parseHookEndpoint(string(out.Stdout))
	if endpoint != nil {
		return endpoint, nil
	}
	if violation != nil {
		// The contract was violated, so say so and name the fix. This is the error
		// an operator meets at 2am after adding a tunnel to a script that worked
		// yesterday, and everything they need to act is in it: which stream, what
		// was on it that should not have been, and the redirect that repairs it.
		return nil, fmt.Errorf("launch_cmd (%s) printed something other than its endpoint on stdout: %s\n"+
			"stdout carries the {\"url\",\"token\"} endpoint JSON and nothing else. Redirect every other "+
			"writer off it — start a tunnel as `mytunnel >/dev/null 2>&1 &`, or send it to a file — and "+
			"write progress to stderr instead. See docs/remote-hooks.md%s",
			p.hooks.LaunchCmd, hookStdoutExcerpt(violation.offending), hookOutputSuffix(out.Combined()))
	}
	if !sawJSON {
		return nil, fmt.Errorf("launch_cmd (%s) exited 0 but printed no {\"url\",\"token\"} JSON on stdout "+
			"(see docs/remote-hooks.md for the recipe)%s", p.hooks.LaunchCmd, hookOutputSuffix(out.Combined()))
	}
	return nil, fmt.Errorf("launch_cmd (%s) exited 0 and printed JSON on stdout, but it is not the endpoint record; "+
		"it must echo the af agent-server's {\"url\",\"token\"} with both non-empty "+
		"(see docs/remote-hooks.md for the recipe)%s",
		p.hooks.LaunchCmd, hookOutputSuffix(out.Combined()))
}

// hookStdoutViolation carries the stdout bytes that were not the endpoint
// record. launch_cmd's stdout is reserved for the {"url","token"} JSON, so
// anything else on it breaks the contract — offending is the text quoted back to
// the operator, redacted and bounded by hookStdoutExcerpt before it is shown.
type hookStdoutViolation struct {
	offending string
}

// hookStdoutExcerptRunes bounds the offending text in the error's first line. A
// flooding tunnel must not paste megabytes into a headline whose job is to name
// one thing to go fix; the script's whole output is attached below it anyway.
const hookStdoutExcerptRunes = 200

// hookStdoutExcerptScanBytes bounds what the excerpt reads to find that line.
// The offending text can be the whole of a flooded stdout, and rendering one
// line of it must not cost a redaction pass over all of it.
const hookStdoutExcerptScanBytes = 8 << 10

// parseHookEndpoint reads launch_cmd's stdout, which by contract holds the
// {"url","token"} endpoint record and nothing else. It returns the endpoint,
// whether stdout held JSON at all (which separates "printed nothing" from
// "printed the wrong shape" in the error), and any content that was not the
// record.
//
// This is a parse, not a classification, and that is the whole point of the
// schema change. While docs/remote-hooks.md let a background tunnel inherit
// stdout, "is this line the endpoint or a log?" was UNDECIDABLE: #2845 exhibits
// two input pairs identical on every inspectable property that require opposite
// handling, and seven successive rules each closed one counterexample and
// admitted the next. Reserving the stream removes the question rather than
// answering it — one JSON value, of the documented shape, surrounded by nothing
// but whitespace.
//
// Whitespace is not content: a trailing newline from `echo`, an indent, a blank
// line, and a CRLF ending are all what a shell normally produces and none of
// them is another writer on the stream.
func parseHookEndpoint(stdout string) (*AgentServerEndpoint, bool, *hookStdoutViolation) {
	open := 0
	for open < len(stdout) && isJSONSpace(stdout[open]) {
		open++
	}
	if open >= len(stdout) {
		// Nothing at all on stdout. That is a different diagnosis from "it also
		// printed something else", and gets its own error.
		return nil, false, nil
	}

	decoder := json.NewDecoder(strings.NewReader(stdout[open:]))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		// stdout does not begin with a JSON value, so all of it is the other output
		// the contract forbids — a tunnel that logged first, or a status line.
		return nil, false, &hookStdoutViolation{offending: stdout[open:]}
	}

	valueEnd := open + int(decoder.InputOffset())
	tail := valueEnd
	for tail < len(stdout) && isJSONSpace(stdout[tail]) {
		tail++
	}
	endpoint, isEndpoint := decodeHookEndpointJSON(string(raw))
	if tail >= len(stdout) {
		if isEndpoint {
			// endpoint.TLSFingerprint is intentionally not read — TLS was removed; an
			// old script that still echoes it parses fine and the value is dropped.
			return &AgentServerEndpoint{URL: endpoint.URL, Token: endpoint.Token}, true, nil
		}
		// One value, wrong shape. That is a different diagnosis from a shared
		// stream, and the operator needs to hear about the schema, not a redirect.
		return nil, true, nil
	}

	// More than one thing on stdout, so quote the FIRST of them that is not the
	// endpoint record — the leading value when it is not one, otherwise whatever
	// follows the record. This chooses what to SHOW, never what to dial: both
	// halves are refused either way, so no guess is smuggled back in. A second
	// endpoint-shaped object is refused too, because picking between them is
	// exactly the guess this contract exists to delete.
	if !isEndpoint {
		return nil, true, &hookStdoutViolation{offending: stdout[open:valueEnd]}
	}
	return nil, true, &hookStdoutViolation{offending: stdout[tail:]}
}

// hookStdoutExcerpt renders the non-endpoint stdout for the error's first line:
// its first non-empty line, token-redacted and bounded. One concrete string is
// what an operator greps their own script for.
//
// Redaction runs BEFORE the line split, so a pretty-printed record collapses to
// one informative line rather than an excerpt reading `{`, and a token wrapped
// across a break is still caught.
func hookStdoutExcerpt(offending string) string {
	if len(offending) > hookStdoutExcerptScanBytes {
		offending = offending[:hookStdoutExcerptScanBytes]
	}
	for rest := redactHookOutputTokens(offending); rest != ""; {
		line, remainder, _ := strings.Cut(rest, "\n")
		rest = remainder
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if runes := []rune(trimmed); len(runes) > hookStdoutExcerptRunes {
			return strings.TrimRight(string(runes[:hookStdoutExcerptRunes]), " \t") + "…"
		}
		return trimmed
	}
	return ""
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// reap runs delete_cmd to tear down whatever launch_cmd provisioned, idempotently
// and only if launch_cmd actually STARTED — if it never ran, nothing was
// provisioned and there is nothing to delete. It backs the session's Kill (after
// the in-sandbox workspace is torn down over REST), a provisioning failure, and a
// bad-endpoint NewInstance failure — so a provisioned sandbox is never leaked.
//
// It memoizes CONDITIONALLY (reapMu + reaped, not sync.Once): a success or a
// delete_cmd that ANSWERED with an error latches, collapsing repeated Kill retries
// to one delete_cmd. A TIMEOUT does NOT latch — the reap did not complete, so the
// daemon must re-run delete_cmd on its next poll rather than see a cached nil and
// delete the record (#2529, the #2063-review defect docker/ssh fixed the same way).
//
// This is best-effort by contract: it may be called for a sandbox that was never
// fully built, so delete_cmd must tolerate a slug it cannot find (documented in
// docs/remote-hooks.md).
func (p *hookProvisioner) reap() error {
	// Held across delete_cmd so concurrent reaps serialize and the latch is written
	// race-free — the mutual exclusion sync.Once gave, minus its permanent latch.
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	if p.reaped {
		return p.reapErr
	}
	if !p.launchStarted {
		// launch_cmd never ran, so nothing was provisioned: a completed no-op reap.
		p.reaped = true
		return nil
	}
	p.resolveAuthSelectors()
	// runHookScript builds its timeout from context.Background(), NEVER a caller's
	// context — and that is load-bearing here. reap's whole job is to run on the
	// failure path, where the launch context is already cancelled or expired, and a
	// WithTimeout derived from a dead parent is born expired: delete_cmd would never
	// spawn and the sandbox would leak in silence. That is the exact failure #1955 is
	// about, reintroduced by the cleanup.
	out, _, err := runHookScriptWithResolvedEnvironment(hookDeleteTimeout, p.hooks.DeleteCmd,
		p.environmentAgent(), p.authSelectors, p.spec.SessionEnvPassthrough, "--name", p.slug)
	if err == nil {
		p.reaped = true
		p.reapErr = nil
		log.InfoLog.Printf("hook runtime: reaped remote session %q via delete_cmd", p.slug)
		return nil
	}
	reapErr := fmt.Errorf("backend=hook: delete_cmd failed for %q: %s: %w", p.slug,
		strings.TrimSpace(redactHookOutputTokens(string(out.Combined()))), err)
	if errors.Is(err, context.DeadlineExceeded) {
		// A timeout is proof of NOTHING: delete_cmd was SIGKILLed mid-reap, so the
		// remote workspace may still be running. Mark it unknown-state so
		// deleteSessionRecord RETAINS the record, and do NOT latch (reaped stays
		// false) so the next poll actually RE-RUNS delete_cmd — a latch here would
		// buy one poll of retention and then return nil, deleting the record and
		// leaking the workspace one poll later (#2529). Same as docker/ssh.
		reapErr = fmt.Errorf("%w: %w", ErrWorkspaceStateUnknown, reapErr)
		log.ErrorLog.Printf("%s", p.orphanWarning(reapErr))
		return reapErr
	}
	// delete_cmd ANSWERED with an error: the reap completed and TOLD us something, so
	// it latches as a KNOWN-state error and the record may be deleted — retrying
	// would just re-run a delete that already reported.
	p.reaped = true
	p.reapErr = reapErr
	log.ErrorLog.Printf("%s", p.orphanWarning(reapErr))
	return reapErr
}

// quiesceLaunchGroup SIGKILLs launch_cmd's process group and waits for it to
// drain, so delete_cmd runs against a world in which nothing can still be
// provisioning.
//
// This closes #1955's leak through the door the TIMEOUT path left open (#2440).
// runHookScript kills only the SCRIPT — no WaitDelay, output to a file so Wait
// returns the instant the script dies — which is deliberate and right for a
// launch that SUCCEEDED: its tunnel is the product, not garbage. But a launch
// killed mid-provision leaves its `terraform`/`gcloud` child running, and that
// child is not killed with the script. delete_cmd then reaps only what exists at
// that instant, reports SUCCESS, and the orphan finishes creating the VM
// afterwards. Provisioning failed, so af keeps no record of the session — and
// orphanWarning never fires, because the reap did not fail. The resource bills
// forever with nothing pointing at it, which is the exact harm #1955 was about.
//
// Only provisionOrReap calls this, and only on the failure of a launch that has
// just returned. Every descendant of a launch being reaped seconds after it
// failed is garbage by definition, so the "we stop listening; we kill nothing"
// rule that governs the SUCCESS path does not apply — but it must not be
// extended to the other reap paths, for two independent reasons:
//
//   - Kill and archive reap a launch that ended long ago. Its group is empty by
//     then, so the group id may have been RECYCLED onto an unrelated process,
//     and signalling it would kill a stranger's process tree. Freshness is what
//     makes the pgid safe to signal, and only this call site has it.
//   - Those paths rebuild the provisioner from the stored cleanup handle
//     (runtime_cleanup.go), which has no pgid to begin with — so the guard below
//     already makes them no-ops. This comment records why that must stay true.
//
// Freshness is ENFORCED, not merely relied on, because "the caller builds a new
// provisioner each time" is an invariant a future caller can silently break and
// the failure mode is signalling a stranger's process group. Two mechanics hold
// it: launch assigns launchPgid on every attempt including the zero, so it can
// never describe an earlier launch; and this function consumes the value, so the
// right to signal a given group is spent exactly once.
func (p *hookProvisioner) quiesceLaunchGroup() {
	if p.launchPgid == 0 {
		return
	}
	// Consume it. A pgid is safe to signal only while the launch that led it has
	// just returned, so the right to signal this one is spent here — a second
	// call can never reach the syscall with a value that has since gone stale.
	pgid := p.launchPgid
	p.launchPgid = 0

	// A negative pid targets the whole group led by launch_cmd. Any error means
	// there is nothing left to wait for: ESRCH is the group already being empty,
	// which is the common case (the script exited and spawned nothing that
	// outlived it) and not a failure.
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		return
	}
	// SIGKILL cannot be caught, so this only covers the descheduling window
	// between delivery and the last member leaving the process table. Bounded
	// because a reap that hangs here would strand the caller it is cleaning up
	// after — the leak this prevents is worse than a straggler, but not worse
	// than a wedge.
	deadline := time.Now().Add(hookQuiesceTimeout)
	for time.Now().Before(deadline) {
		// Signal 0 tests whether the group still has members without delivering
		// anything; an error (ESRCH) means it has drained.
		if err := syscall.Kill(-pgid, 0); err != nil {
			return
		}
		time.Sleep(hookQuiescePoll)
	}
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
