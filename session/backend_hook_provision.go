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
		return ProvisionResult{}, err
	}
	// delete_cmd owns the machine's life, so the session's teardown must reap the
	// SANDBOX WORKSPACE first and then the machine. Chaining them keeps a single
	// Teardown while neither step can be skipped.
	sandboxTeardown := res.Teardown
	teardown := func() error {
		var sandboxErr error
		if sandboxTeardown != nil {
			sandboxErr = sandboxTeardown()
		}
		hookErr := p.reap()
		_ = os.RemoveAll(dir)
		return errors.Join(sandboxErr, hookErr)
	}
	res.Teardown = teardown
	if b, ok := res.Backend.(*sandboxBackend); ok {
		b.remoteAgentBackend.reap = teardown
	}
	return res, nil
}

// runProvisionCmd invokes provision_cmd with the same flags launch_cmd receives
// and parses its one-record stdout.
func (p *hookProvisioner) runProvisionCmd() (*hookProvisionRecord, error) {
	args := []string{"--name", p.slug, "--title", p.spec.Title, "--repo", p.spec.CloneURL}
	if branch := strings.TrimSpace(p.spec.RestoreBranch); branch != "" {
		args = append(args, "--branch", branch)
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
