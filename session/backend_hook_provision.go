package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("cannot create the per-session host-key directory %q: %w", dir, err)
	}
	path := filepath.Join(dir, "known_hosts")
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

// hookProvisionHostPort splits a "host:port" Host value so a record may spell the
// port either way. An address with no port is returned unchanged.
func hookProvisionHostPort(record *hookProvisionRecord) (string, int) {
	host := strings.TrimSpace(record.Host)
	if record.Port != 0 {
		return host, record.Port
	}
	if h, p, err := net.SplitHostPort(host); err == nil {
		if port, convErr := strconv.Atoi(p); convErr == nil {
			return h, port
		}
	}
	return host, 0
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

	host, port := hookProvisionHostPort(record)
	dir, err := hookProvisionSessionDir(p.slug)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=hook: %w", err)
	}
	knownHosts, err := hookProvisionKnownHosts(dir, host, port, record.HostKey)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=hook: %w", err)
	}
	pinned := *record
	pinned.Host, pinned.Port = host, port

	afBin, err := sshSelfBinary()
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("backend=hook: cannot locate the af binary to stream onto the sandbox: %w", err)
	}
	// Everything below the transport is the sandbox runtime's, unchanged: mktemp,
	// clone, stream `af`, start the agent-server, read its banner, tunnel, and an
	// identity-checked reap. That reuse is the whole point of splitting
	// provisioning from transport.
	sp := &sandboxProvisioner{
		spec:    p.spec,
		sshCmd:  hookProvisionSSHCommand(knownHosts, &pinned),
		afBin:   afBin,
		program: p.program,
	}
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
		hookErr := p.reap()
		if hookErr == nil {
			// The machine is gone: nothing about the workspace can still be unknown,
			// and the pin has nothing left to verify. Both are safe to drop.
			_ = os.RemoveAll(dir)
			return nil
		}
		// Cleanup is still retryable, so the pin MUST survive: it is embedded in the
		// sandbox ssh command, and deleting it would make every later reap fail
		// host-key verification before it could reach the machine — a retained row
		// that can never recover.
		return errors.Join(sandboxErr, hookErr)
	}
	res.Teardown = teardown
	res.Backend = p.provisionedBackend(teardown)
	return res, nil
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
func (p *hookProvisioner) provisionedBackend(teardown func() error) Backend {
	return &HookBackend{
		remoteAgentBackend: remoteAgentBackend{reap: teardown},
		provisioner:        p,
		cleanup:            p.cleanupData(),
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

// hookProvisionSessionDir is where this session's pinned known_hosts lives: under
// the af home, never the repo, since it is machine state rather than project
// state.
func hookProvisionSessionDir(slug string) (string, error) {
	base, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve the af home for the per-session host-key store: %w", err)
	}
	return filepath.Join(base, "hook-hosts", slug), nil
}
