package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The provisioning-hook contract (#2847): a hook that returns an ssh HOST, not
// an endpoint.
//
// WHY THIS EXISTS. `launch_cmd` makes the script own TRANSPORT as the price of
// owning provisioning: it must start an `af agent-server`, capture its bearer
// token, keep a tunnel alive, and echo a URL. Nearly every wound in this area
// came from that second job — #2845's undecidable stdout, and the whole token
// redaction series (#2684, #2687, #2690, #2748, #2771), which exists only
// because a SECRET travels through a script's stdout, argv, logs and errors.
//
// `provision_cmd` splits the two. The script makes a machine and says how to
// reach it; af does the rest with the sandbox transport (#2476 PR2). What that
// deletes, rather than mitigates:
//
//   - No bearer token ever enters a script. A host address and a host PUBLIC key
//     are not secrets, so redaction stops being load-bearing on this path.
//   - No tunnel to background, so the reap-the-launch-tree machinery is not
//     load-bearing either.
//   - No `af agent-server` lifecycle in user code — af already clones, streams
//     its own binary, starts the server and reads the banner.
//
// `launch_cmd` REMAINS the escape hatch. Some targets have no sshd (certain k8s
// pods, serverless runners, WebSocket-only PaaS) and returning a URL is their
// only option. This is a safe default plus an override, not a replacement.

// hookProvisionRecord is the single JSON object provision_cmd prints on stdout.
//
// Like the endpoint record it replaces, stdout carries this and NOTHING else —
// the #2862 contract, adopted here from the first line rather than retrofitted,
// so "is this the record?" is a parse and never a guess.
type hookProvisionRecord struct {
	// Host is the address af connects to. Required.
	Host string `json:"host"`
	// User is the ssh login. Optional — empty defers to the ssh binary's own
	// resolution, which for this transport means the operator's ~/.ssh/config.
	User string `json:"user,omitempty"`
	// Port is the ssh port. Optional — 0 means ssh's default.
	Port int `json:"port,omitempty"`
	// HostKey is the sandbox's PUBLIC host key in authorized_keys form
	// ("ssh-ed25519 AAAA…"). REQUIRED, and the heart of the contract — see
	// hookProvisionKnownHosts.
	HostKey string `json:"host_key"`
}

// parseHookProvisionRecord reads provision_cmd's stdout, which by contract holds
// one JSON object and nothing else.
//
// Deliberately the same discipline as parseHookEndpoint: decode exactly one
// value, refuse anything with content on either side of it. #2845 proved that a
// stdout shared with a tunnel makes "which of these is the record?" undecidable;
// this contract is new, so it starts where that one had to be dragged.
func parseHookProvisionRecord(stdout string) (*hookProvisionRecord, bool, *hookStdoutViolation) {
	open := 0
	for open < len(stdout) && isJSONSpace(stdout[open]) {
		open++
	}
	if open >= len(stdout) {
		return nil, false, nil
	}
	decoder := json.NewDecoder(strings.NewReader(stdout[open:]))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return nil, false, &hookStdoutViolation{offending: stdout[open:]}
	}
	valueEnd := open + int(decoder.InputOffset())
	tail := valueEnd
	for tail < len(stdout) && isJSONSpace(stdout[tail]) {
		tail++
	}
	if tail < len(stdout) {
		return nil, true, &hookStdoutViolation{offending: stdout[tail:]}
	}

	var record hookProvisionRecord
	decodeStrict := json.NewDecoder(strings.NewReader(string(raw)))
	decodeStrict.DisallowUnknownFields()
	if err := decodeStrict.Decode(&record); err != nil {
		return nil, true, nil
	}
	if strings.TrimSpace(record.Host) == "" || strings.TrimSpace(record.HostKey) == "" {
		return nil, true, nil
	}
	return &record, true, nil
}

// hookProvisionKnownHosts writes a known_hosts file holding EXACTLY the one key
// this session's sandbox presented, and returns its path.
//
// This is the answer to the problem that blocked the whole idea: a VM created
// seconds ago has no known_hosts entry, and the resulting prompt is precisely
// what an unattended provision cannot answer. None of af's three postures works
// for it — `strict` refuses an unknown host outright; `accept-new` is
// trust-on-first-use where EVERY session is a first contact, so its one-time
// window is open every time, and its append-only store later REFUSES a
// legitimate VM once a name or IP is recycled; `insecure` is a standing
// invitation to a man-in-the-middle that would see the bearer token.
//
// The way out is that the provisioning script is the only party with an
// AUTHENTIC channel to the key — it is talking to the provider's control plane,
// which af cannot reach. It either reads the key back (AWS console output, GCP
// guest attributes) or, better, injects one it generated via cloud-init before
// first boot. So the script returns it, af pins it for this session only, and
// `StrictHostKeyChecking=yes` against a file containing exactly that key is a
// VERIFICATION rather than a trust-on-first-use.
//
// Fails closed: a record with no key never reaches here, because the parse
// rejects it.
func hookProvisionKnownHosts(dir, host string, port int, hostKey string) (string, error) {
	// Through the home guard: hookProvisionSessionDir builds this from
	// config.GetConfigDir, so it is inside the AF home and is created on the
	// session-launch path, ahead of the create's persist (#3850).
	if err := config.MkdirAllUnderAFHome(dir, 0o700); err != nil {
		return "", fmt.Errorf("cannot create the per-session host-key directory %q: %w", dir, err)
	}
	path := filepath.Join(dir, HookHostsPinFileName)
	// A non-default port is part of the known_hosts identity, and OpenSSH spells
	// it "[host]:port". Getting this wrong makes verification fail with a message
	// about an unknown host rather than about the port.
	entryHost := host
	if port != 0 && port != 22 {
		entryHost = "[" + host + "]:" + strconv.Itoa(port)
	}
	line := entryHost + " " + strings.TrimSpace(hostKey) + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		return "", fmt.Errorf("cannot write the per-session known_hosts %q: %w", path, err)
	}
	return path, nil
}

// hookProvisionSSHCommand composes the ssh invocation for a provisioned host.
//
// It pins verification to the per-session file and turns strict checking ON.
// That combination is what makes this safe on a host with no prior history:
// there is exactly one acceptable key, af put it there, and the script vouched
// for it out of band.
//
// GlobalKnownHostsFile=/dev/null is not belt-and-braces — without it OpenSSH
// still consults /etc/ssh/ssh_known_hosts, so a system-wide entry for a recycled
// address could satisfy the check instead of the key we actually pinned.
func hookProvisionSSHCommand(knownHostsPath string, record *hookProvisionRecord) string {
	parts := []string{
		"ssh",
		"-o", "UserKnownHostsFile=" + shellQuoteSandbox(knownHostsPath),
		"-o", "GlobalKnownHostsFile=/dev/null",
		// ssh_config(5) consults KnownHostsCommand IN ADDITION to both files, so an
		// operator's matching Host block could hand OpenSSH a different key and
		// satisfy verification without our pin ever deciding anything.
		//
		// KEPT HERE ON PURPOSE, having been deleted from the backend=ssh command for
		// making that backend unusable below OpenSSH 8.5 (#3092). The two calls are
		// not the same: ssh_command.go passes `-F none`, so no config file is read and
		// nothing could install such a helper — the option guarded nothing there. THIS
		// command deliberately DOES read the operator's ssh_config, so the helper is
		// genuinely reachable and this option is the only thing between a `Host` block
		// and af's per-session pin. Deleting it by symmetry would trade a version floor
		// for a key-substitution hole.
		//
		// The cost is real and now documented rather than left to be rediscovered:
		// hook provisioning that returns a host_key needs OpenSSH >= 8.5 on the daemon
		// host. See docs/remote-hooks.md.
		"-o", "KnownHostsCommand=none",
		"-o", "StrictHostKeyChecking=yes",
		// The script vouches for the key, so there is nobody to ask. Refuse
		// rather than hang forever on a prompt no unattended provision can answer.
		"-o", "BatchMode=yes",
	}
	if record.Port != 0 {
		parts = append(parts, "-p", strconv.Itoa(record.Port))
	}
	target := record.Host
	if user := strings.TrimSpace(record.User); user != "" {
		target = user + "@" + target
	}
	parts = append(parts, shellQuoteSandbox(target))
	return strings.Join(parts, " ")
}

// newHookSandboxProvisioner builds the transport this hook provisions onto.
//
// It is a seam, and a narrow one: a test overrides it to observe the ssh command
// provisionHost ACTUALLY composed. Without that, every assertion about the
// composed command has to restate provisionHost's own normalization, and then
// provisionHost can stop doing it while the test stays green — which is exactly
// the gap this closes. Production leaves it alone.
var newHookSandboxProvisioner = func(spec ProvisionSpec, sshCmd, afBin, program string) *sandboxProvisioner {
	return &sandboxProvisioner{spec: spec, sshCmd: sshCmd, afBin: afBin, program: program}
}

// hookProvisionPinnedRecord normalizes a record for pinning: the port is split
// out of Host when it was spelled there, and the returned copy carries the split
// values. Extracted so the connection tests traverse THIS handoff rather than
// repeating the assignment — a test that restates it would pass even if
// provisionHost stopped doing it.
func hookProvisionPinnedRecord(record *hookProvisionRecord) (pinned hookProvisionRecord, host string, port int, err error) {
	host, port, err = hookProvisionHostPort(record)
	if err != nil {
		return hookProvisionRecord{}, "", 0, err
	}
	pinned = *record
	pinned.Host, pinned.Port = host, port
	return pinned, host, port, nil
}

// hookProvisionHostPort resolves a record's address through the SHARED resolver —
// the same one backend=ssh uses — so a record cannot reach a different port
// depending on which backend read it (#3044). Port 0 means unspecified, which
// leaves the choice to the ssh binary.
func hookProvisionHostPort(record *hookProvisionRecord) (string, int, error) {
	return resolveSSHHostPort(record.Host, record.Port)
}

// provisionHostOrReap runs the ssh-host contract end to end, reaping via
// delete_cmd on any failure exactly as the endpoint contract does.
//
// The reap gate is deliberately the SAME one #1955 established for launch_cmd:
// once provision_cmd has STARTED, a sandbox may exist, and "it failed" is not
// evidence that nothing was created.
func (p *hookProvisioner) provisionHostOrReap() (ProvisionResult, error) {
	p.resolveAuthSelectors()
	res, err := p.provisionHost()
	if err == nil {
		return res, nil
	}
	p.quiesceLaunchGroup()
	if reapErr := p.reap(); reapErr != nil {
		return ProvisionResult{}, fmt.Errorf("%w\n\n%s", errors.Join(err, reapErr), p.orphanWarning(reapErr))
	}
	return ProvisionResult{}, err
}

func (p *hookProvisioner) provisionHost() (ProvisionResult, error) {
	// Refuse BEFORE running the script. GitHub is the durable store, so a repo
	// with no origin cannot be cloned onto the sandbox — and running provision_cmd
	// first would create a billable machine to serve a clone guaranteed to fail.
	// The sandbox runtime rejects this up front for the same reason.
	if p.spec.CloneURL == "" {
		return ProvisionResult{}, missingOriginError(BackendHook, p.spec.RepoRoot)
	}
	record, err := p.runProvisionCmd()
	if err != nil {
		return ProvisionResult{}, err
	}

	pinned, host, port, err := hookProvisionPinnedRecord(record)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=hook: provision_cmd returned an unusable address: %w", err)
	}
	dir, err := hookProvisionSessionDir(p.slug)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=hook: %w", err)
	}
	// No failed provision returns a teardown closure to own this directory, so
	// establish its fallback owner before the call that creates it. Keeping one
	// guard around the rest of the function means a new error return below cannot
	// silently add another leak. The guard stays armed through
	// reapProvisionFailure: that reap stops the transport first, but may still need
	// this pin while it does so. It is disarmed only at the successful handoff to
	// the teardown closure below.
	removeKnownHostsOnFailure := true
	defer func() {
		if removeKnownHostsOnFailure {
			_ = os.RemoveAll(dir)
		}
	}()
	knownHosts, err := hookProvisionKnownHosts(dir, host, port, record.HostKey)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=hook: %w", err)
	}
	afBin, err := sshSelfBinary()
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=hook: cannot locate the af binary to stream onto the sandbox: %w", err)
	}
	// Everything below the transport is the sandbox runtime's, unchanged: mktemp,
	// clone, stream `af`, start the agent-server, read its banner, tunnel, and an
	// identity-checked reap. That reuse is the whole point of splitting
	// provisioning from transport.
	sp := newHookSandboxProvisioner(p.spec, hookProvisionSSHCommand(knownHosts, &pinned), afBin, p.program)
	res, err := sp.provision()
	if err != nil {
		// sp owns LOCAL state the hook cleanup cannot touch: a tunnel child whose
		// ssh/ProxyCommand may be alive without ever opening the forward. delete_cmd
		// destroys the machine but not that process group, so reap it here exactly
		// as sandboxRuntime.Provision does.
		return ProvisionResult{}, sp.reapProvisionFailure(err)
	}
	// delete_cmd owns the MACHINE, so the session's teardown reaps the workspace
	// first and then destroys the host.
	//
	// A successful delete_cmd is CONCLUSIVE and outranks an unknown workspace
	// reap. The workspace lives on a machine that no longer exists, so preserving
	// ErrWorkspaceStateUnknown there would retain a row whose later reaps can only
	// fail against a deleted host while the latched hook reap returns nil —
	// leaving the session stuck in cleanup forever. Only a delete_cmd that did NOT
	// confirm can leave the outcome unknown.
	sandboxTeardown := res.Teardown
	teardown := func() error {
		var sandboxErr error
		if sandboxTeardown != nil {
			sandboxErr = sandboxTeardown()
		}
		hookErr := hookProvisionReapUnpinning(p.reap, dir, p.spec.Title)
		if hookErr == nil {
			// The machine is gone, so nothing about the workspace can still be
			// unknown: sandboxErr is dropped here on purpose. (The pin is dropped by
			// the call above, on the same evidence.)
			return nil
		}
		return errors.Join(sandboxErr, hookErr)
	}
	res.Teardown = teardown
	res.Backend = p.provisionedBackend()
	removeKnownHostsOnFailure = false
	return res, nil
}

// hookProvisionReapUnpinning runs a provision_cmd session's delete_cmd reap and
// drops its pinned host-key directory ONLY when that reap returned nil.
//
// It exists so the live teardown and the one rebuilt from a kill tombstone
// cannot drift apart, because the success-only condition is the whole of what
// matters here and it is now spelled once. #3454 was exactly that drift: the
// restored path ran the reap and never removed the directory, so every
// tombstone that outlived its daemon orphaned one.
//
// SUCCESS-ONLY IS LOAD-BEARING, not an optimization. The pin is embedded in this
// session's sandbox ssh command, so removing it while cleanup is still retryable
// would make every later reap fail host-key verification before it could reach
// the machine — a retained row that could never complete. A reap that SUCCEEDED
// settles it the other way: the machine is gone, and the pin has nothing left to
// verify.
//
// A removal that FAILS is logged rather than returned. The machine is genuinely
// gone by then, and promoting a stray directory into a teardown failure would
// retain a row whose delete_cmd can only ever succeed again — trading a leaked
// directory for a session stuck in cleanup. The log line is what keeps a
// persistent leak visible instead of silent. An empty dir means the af home
// could not be resolved at all; the reap still has to run.
func hookProvisionReapUnpinning(reap func() error, dir, title string) error {
	if err := reap(); err != nil {
		return err
	}
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		log.WarningLog.Printf("hook runtime: session %q was reaped, but its pinned host-key directory %q could not be removed: %v",
			title, dir, err)
	}
	return nil
}

// restoredHookProvisionTeardown rebuilds the pin half of the teardown for a
// tombstone whose session created hook-hosts/<slug>, so a kill that outlives its
// daemon reaches the same end state a live one does (#3454).
//
// The directory is resolved per ATTEMPT rather than when the closure is
// composed, for the reason the ssh case documents at its own store: this runs
// while persisted instances are loading, and a transiently unresolvable af home
// must not be captured as a permanently broken cleanup. It resolves through
// hookProvisionSessionDir — the same helper the create path uses — so the two
// cannot come to name different directories.
func restoredHookProvisionTeardown(reap func() error, slug, title string) func() error {
	return func() error {
		dir, err := hookProvisionSessionDir(slug)
		if err != nil {
			// Nothing can be named, so nothing can be removed — but the MACHINE still
			// has to go. Reap anyway and report only the directory left behind;
			// refusing here would leak a billable host to avoid leaking a directory.
			log.WarningLog.Printf("hook runtime: cannot locate the pinned host-key directory for session %q, so a successful reap will leave it behind: %v",
				title, err)
		}
		return hookProvisionReapUnpinning(reap, dir, title)
	}
}

// provisionedBackend builds the Backend a provision_cmd session is recorded as.
//
// Its IDENTITY is the HOOK's, not the sandbox's, and that is the whole point.
// Recording it as `sandbox` would persist only SandboxRuntimeCleanupData, so
// archive/restore would route to sandboxRuntime and demand the unrelated global
// `sandbox_ssh` instead of re-running provision_cmd — and a kill tombstone
// restored after a daemon crash would carry no delete_cmd, leaking the machine
// this hook provisioned.
//
// Split out so a test drives the real constructor rather than a restatement of
// it: asserting on a hand-built HookBackend would pass even if provisionHost
// returned the sandbox one.
func (p *hookProvisioner) provisionedBackend() Backend {
	cleanup := p.cleanupData()
	// The one place this claim is made, and the only path entitled to make it:
	// provisionHost reaches here solely after hookProvisionKnownHosts wrote the
	// pin, so a record built by this constructor genuinely owns hook-hosts/<slug>
	// and its restored teardown may drop it (#3454).
	//
	// Deliberately NOT set inside cleanupData(): that helper is shared with the
	// launch_cmd path, which pins nothing. Setting it there would make a
	// launch_cmd tombstone claim a directory it never created — and under a
	// recycled slug that claim deletes a live provision_cmd session's pin,
	// breaking the very teardown this field exists to complete.
	cleanup.HasKnownHostsDir = true
	return &HookBackend{
		provisioner: p,
		cleanup:     cleanup,
	}
}

// runProvisionCmd invokes provision_cmd with the same flags launch_cmd receives
// and parses its one-record stdout.
func (p *hookProvisioner) runProvisionCmd() (*hookProvisionRecord, error) {
	// The SAME flags launch_cmd receives, because the contract says so — a
	// provisioner that sizes a machine from --program, or delivers approved values
	// named by --session-env, gets incomplete metadata otherwise.
	args := []string{"--name", p.slug, "--title", p.spec.Title, "--repo", p.spec.CloneURL}
	if branch := strings.TrimSpace(p.spec.RestoreBranch); branch != "" {
		args = append(args, "--branch", branch)
	}
	if prog := strings.TrimSpace(p.environmentProgram()); prog != "" {
		args = append(args, "--program", prog, "--program-resolved")
	}
	for _, name := range p.spec.SessionEnvPassthrough {
		args = append(args, "--session-env", name)
	}
	out, cmd, err := runHookScriptWithResolvedEnvironment(hookLaunchTimeout, p.hooks.ProvisionCmd,
		p.environmentAgent(), p.authSelectors, p.spec.SessionEnvPassthrough, args...)
	p.launchStarted = cmd != nil && cmd.Process != nil
	p.launchPgid = 0
	if p.launchStarted {
		p.launchPgid = cmd.Process.Pid
	}
	if err != nil {
		return nil, fmt.Errorf("provision_cmd failed (%s): %w%s", p.hooks.ProvisionCmd, err, hookOutputSuffix(out.Combined()))
	}
	// provision_cmd has EXITED successfully. Everything after this — cloning,
	// streaming the binary, starting the server — can take minutes, and a pgid the
	// kernel has reclaimed may be reissued to an unrelated process in that window.
	// So the group id is spent here: a later failure must never SIGKILL a group
	// this script no longer owns. (A failure of provision_cmd ITSELF still
	// quiesces, because the id is still live at that point.)
	p.launchPgid = 0

	record, sawJSON, violation := parseHookProvisionRecord(string(out.Stdout))
	if record != nil {
		return record, nil
	}
	if violation != nil {
		return nil, fmt.Errorf("provision_cmd (%s) printed something other than its host record on stdout: %s\n"+
			"stdout carries the {\"host\",\"host_key\"} JSON and nothing else. Redirect every other writer off it — "+
			"send progress to stderr — see docs/remote-hooks.md%s",
			p.hooks.ProvisionCmd, hookStdoutExcerpt(violation.offending), hookOutputSuffix(out.Combined()))
	}
	if !sawJSON {
		return nil, fmt.Errorf("provision_cmd (%s) exited 0 but printed no host record on stdout "+
			"(see docs/remote-hooks.md for the recipe)%s", p.hooks.ProvisionCmd, hookOutputSuffix(out.Combined()))
	}
	return nil, fmt.Errorf("provision_cmd (%s) exited 0 and printed JSON on stdout, but it is not a host record; "+
		"it must be {\"host\":\"…\",\"host_key\":\"ssh-… AAAA…\"} with both non-empty (optional \"user\", \"port\"). "+
		"host_key is REQUIRED: af pins it for this session so a machine created seconds ago can be VERIFIED rather "+
		"than trusted on sight — read it from your provider's console output or guest attributes, or inject one via "+
		"cloud-init before first boot%s", p.hooks.ProvisionCmd, hookOutputSuffix(out.Combined()))
}

// HookHostsRoot is the af-home subdirectory holding the per-session pinned
// host-key directories, and HookHostsPinFileName is the single file each of them
// holds.
//
// Exported because a consumer outside this package has to name the same two
// things: `af doctor` collects the directories no session owns any more (#3560),
// and it can only remove a directory it has PROVEN af wrote. #3454's lesson was
// that two paths naming one directory drift apart — that drift WAS the bug — so
// there is one spelling of each and everybody reads it from here.
const (
	HookHostsRoot        = "hook-hosts"
	HookHostsPinFileName = "known_hosts"
)

// hookProvisionSessionDir is where this session's pinned known_hosts lives: under
// the af home, never the repo, since it is machine state rather than project
// state.
func hookProvisionSessionDir(slug string) (string, error) {
	base, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve the af home for the per-session host-key store: %w", err)
	}
	return filepath.Join(base, HookHostsRoot, slug), nil
}
