package commands

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// `af hosts` (#5571 W10.5): add a sandbox host in one command.
//
// The host list is the operator's global `hosts` table (W6.1). `add` writes the
// entry through the same admission-gated `config set hosts` path every other
// writer uses — never a second writer — and then asks the daemon to probe the
// host (CheckHost, W2.3), printing what is missing with the fix.
//
// Connectivity is the operator's: af opens no tunnel and edits no ssh config.
// An entry is kept even when the host is unreachable or lacks a tool, because
// the fix is on the host or the network, and re-running `af hosts check <name>`
// is the loop.
//
// Local-only: the list lives in this machine's global config, and the probe is
// run by this machine's daemon, so there is no honest remote reading of either.

var (
	hostsJSONFlag             bool
	hostsAddIdentityFile      string
	hostsAddWorkspaceRoot     string
	hostsAddHostKeyPosture    string
	hostsAddDefault           bool
	hostsAddNoCheck           bool
	hostsAddPushCredentials   bool
	hostsRemoveClearDefault   bool
	hostsCheckProbe           = daemon.CheckHost
	hostsConfigSet            = globalConfigSet
	hostsEditApply            = globalHostsEdit
	hostsLoad                 = loadGlobalHosts
	hostsPosture              = globalHostKeyPosture
	errHostsNotReady          = errors.New("the host is saved but not ready to run a session")
	errHostsCheckHasProblems  = errors.New("one or more hosts are not ready to run a session")
	errHostsUnreachableOrBusy = errors.New("one or more hosts could not be probed")
	errHostsCheckIncomplete   = errors.New("one or more hosts answered but not with a recognisable git, tmux and gh report")
)

var hostsCmd = &cobra.Command{
	Use:   "hosts",
	Short: "Add, list, check and remove sandbox hosts",
	Long: `Manage the global host list that sandbox sessions run on.

  af hosts add box1 ubuntu@box1.tailnet.ts.net   save the host, then check it
  af hosts check [box1]                          probe git, tmux and gh on the host
  af hosts list                                  show the saved hosts
  af hosts remove box1                           drop the host from the list

You own connectivity (a tailnet, a VPN, a key in your agent): af opens no tunnel
and edits no ssh config. If the host cannot be reached or is missing a tool, the
entry is still saved and the command says what to fix; rerun 'af hosts check'.`,
}

// hostsAddResult is what `af hosts add --json` reports.
type hostsAddResult struct {
	Name    string                  `json:"name"`
	Host    config.HostConfig       `json:"host"`
	Default bool                    `json:"default"`
	Check   *daemon.HostCheckResult `json:"check,omitempty"`
}

type hostListEntry struct {
	Name    string            `json:"name"`
	Host    config.HostConfig `json:"host"`
	Default bool              `json:"default"`
}

// globalHostKeyPosture is the global ssh.host_key_verification, "" when unreadable
// (the composer then applies strict, its default).
func globalHostKeyPosture() string {
	load, err := config.LoadConfigReadOnly()
	if err != nil || load.Config == nil {
		return ""
	}
	return load.Config.SSHHostKeyVerification
}

// sshRemedyCommand is the command printed for the operator to test a host's
// connection: af's own ssh invocation for the entry (session.OperatorSSHCommand),
// so every shape the list accepts — a bare host, a user, a custom port, a key —
// is valid as printed. Never `ssh user@host:port`, which OpenSSH reads as the
// hostname "host:port" on port 22 (#5852).
func sshRemedyCommand(entry config.HostConfig) string {
	cmd, err := session.OperatorSSHCommand(entry, hostsPosture())
	if err != nil {
		// An entry that cannot compose cannot be saved either (ValidateHostEntry).
		return "ssh <the saved host> true"
	}
	return cmd + " true"
}

func loadGlobalHosts() (map[string]config.HostConfig, string, error) {
	load, err := config.LoadConfigReadOnly()
	if err != nil {
		return nil, "", err
	}
	if load.Config == nil {
		return map[string]config.HostConfig{}, "", nil
	}
	hosts := make(map[string]config.HostConfig, len(load.Config.Hosts))
	for k, v := range load.Config.Hosts {
		hosts[k] = v
	}
	return hosts, load.Config.DefaultHost, nil
}

// parseHostDestination splits `[user@]host[:port]`, or an `ssh://[user@]host[:port]`
// URL, into the entry's fields. The stored host is always the bare hostname or
// address: brackets around an IPv6 literal are URL syntax, not part of the host
// (ssh takes `-p 2222 user@2001:db8::1`, which is how af composes its argv), and
// the port is split out. A bare IPv6 literal without brackets (more than one
// colon) is a host, not host:port.
func parseHostDestination(dest string) (config.HostConfig, error) {
	dest = strings.TrimSpace(dest)
	if strings.HasPrefix(dest, "ssh://") {
		return parseSSHURLDestination(dest)
	}
	var h config.HostConfig
	if user, rest, ok := strings.Cut(dest, "@"); ok {
		h.User, dest = user, rest
	}
	dest = strings.TrimSuffix(dest, "/")
	if strings.HasPrefix(dest, "[") {
		return finishBracketedHost(h, dest)
	}
	if strings.Count(dest, ":") == 1 {
		host, port, _ := strings.Cut(dest, ":")
		n, err := hostPort(port, dest)
		if err != nil {
			return h, err
		}
		h.Port, dest = n, host
	}
	h.Host = dest
	return h, nil
}

// finishBracketedHost reads `[address]` or `[address]:port` (no scheme).
func finishBracketedHost(h config.HostConfig, dest string) (config.HostConfig, error) {
	end := strings.IndexByte(dest, ']')
	if end < 0 {
		return h, fmt.Errorf("%q has an opening [ with no closing ]", dest)
	}
	h.Host = dest[1:end]
	switch rest := dest[end+1:]; {
	case rest == "":
	case strings.HasPrefix(rest, ":"):
		n, err := hostPort(rest[1:], dest)
		if err != nil {
			return h, err
		}
		h.Port = n
	default:
		return h, fmt.Errorf("%q: only a :port may follow the closing ]", dest)
	}
	return h, nil
}

// parseSSHURLDestination parses an ssh:// URL with net/url, which owns the
// bracketed-IPv6 and port rules.
func parseSSHURLDestination(dest string) (config.HostConfig, error) {
	var h config.HostConfig
	if err := refuseUnbracketedIPv6URL(dest); err != nil {
		return h, err
	}
	u, err := url.Parse(dest)
	if err != nil {
		return h, fmt.Errorf("%q is not a valid ssh:// URL: %w", dest, err)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return h, fmt.Errorf("%q: an ssh:// destination is [user@]host[:port], with no path, query or fragment", dest)
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			return h, fmt.Errorf("%q: put no password in a host destination; ssh authenticates with keys", dest)
		}
		h.User = u.User.Username()
	}
	h.Host = u.Hostname()
	if port := u.Port(); port != "" {
		n, err := hostPort(port, dest)
		if err != nil {
			return h, err
		}
		h.Port = n
	}
	return h, nil
}

// refuseUnbracketedIPv6URL refuses an ssh:// destination whose host part has a
// colon outside brackets and more than one colon overall. RFC 3986 puts an IPv6
// literal in brackets, so `ssh://user@2001:db8::1` is not a URL authority:
// net/url would read the last group as a port and af would save, and probe, a
// different machine (#6067). Refusing is the only reading that cannot guess
// wrong. A single colon (`host:port`) is still a port, and the scheme-less
// `user@2001:db8::1` keeps its documented whole-address reading.
func refuseUnbracketedIPv6URL(dest string) error {
	authority := strings.TrimPrefix(dest, "ssh://")
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}
	userinfo := ""
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		userinfo, authority = authority[:i+1], authority[i+1:]
	}
	if strings.HasPrefix(authority, "[") || strings.Count(authority, ":") < 2 {
		return nil
	}
	return fmt.Errorf("%q has an IPv6 address outside brackets, which a URL cannot express; "+
		"put it in brackets and add the port after them, for example ssh://%s[%s]:22", dest, userinfo, authority)
}

func hostPort(port, dest string) (int, error) {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("the port in %q must be a number from 1 to 65535", dest)
	}
	return n, nil
}

// applyHostsEdit sends one per-entry edit of the host list to the writer, which
// performs the read, the exists/default checks and the write as ONE
// read-modify-write under the exclusive config file lock (config.EditGlobalHosts).
// Concurrent add, remove and `af config set` block on that lock and run one after
// the other, so none is lost and none retries (#5855). The CLI never reads the
// table and writes it back itself: the write is made by the daemon, another
// process, so the CLI could not hold the lock across its own read.
func applyHostsEdit(edit config.HostsEdit) error {
	_, err := hostsEditApply(edit)
	return err
}

// hostsFailJSON ends a failed command. With --json the error rides in the one
// envelope on STDOUT, where a script reads the outcome; nothing else is printed
// on either stream. The returned error still sets the exit code.
func hostsFailJSON(cmd *cobra.Command, err error) error {
	return hostsEnvelopeError(cmd, nil, err, "")
}

// hostsEnvelopeError emits {data, error} for an outcome that is partly done: the
// host was saved (data) and a later step failed (error, with its code). A script
// can then tell "saved but unchecked" from "saved and verified" from a hard
// failure without scraping stderr. Off --json it is a passthrough.
func hostsEnvelopeError(cmd *cobra.Command, data any, err error, code string) error {
	if !hostsJSONFlag || err == nil {
		return err
	}
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if root := cmd.Root(); root != nil {
		root.SilenceUsage = true
		root.SilenceErrors = true
	}
	log.CloseQuiet()
	_ = apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Envelope{
		Data:  data,
		Error: &apiproto.EnvelopeError{Message: err.Error(), Code: code},
	})
	return err
}

// Error codes in the --json envelope of `af hosts` (additive; absent on an
// ordinary failure).
const (
	// hostsCodeNotReady: the host is saved, the probe answered, and it cannot run a
	// session yet (or could not be reached, or answered unrecognisably). data
	// carries the probe result.
	hostsCodeNotReady = "host_not_ready"
	// apiproto.ErrorCodeMutationCommitted: the host is saved but a later step (the
	// default pick, or the probe itself) could not run.
)

var hostsAddCmd = &cobra.Command{
	Use:   "add <name> <ssh-destination>",
	Short: "Save a host and check that it can run a session",
	Long: `Save <name> as a sandbox host and probe it.

<ssh-destination> is [user@]host[:port] (an ssh:// URL works too), for example ubuntu@box1.tailnet.ts.net.
The name is what 'af sessions create --host <name>' takes. The host is probed
for git, tmux (3.2 or newer) and gh, and every missing tool is reported with
its fix. The entry is kept either way; use --no-check to skip the probe.

--push-credentials opts the host into push credentials in the same write, so it
is never listed without them: a listed host that is not opted in is one a
sandbox may create sessions on. Opt in only a fresh, dedicated machine that
runs nothing but your own af sessions (see "Push credentials" in
docs/backends.md).`,
	Example: `  af hosts add box1 ubuntu@box1.tailnet.ts.net
  af hosts add gpu 10.0.0.7:2222 --identity-file /home/me/.ssh/gpu --default
  af hosts add build af-push@build.tailnet.ts.net --push-credentials`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requireLocalTarget("af hosts add", "writes this machine's host list"); err != nil {
			return hostsFailJSON(cmd, err)
		}
		name := strings.TrimSpace(args[0])
		entry, err := parseHostDestination(args[1])
		if err != nil {
			return hostsFailJSON(cmd, err)
		}
		entry.IdentityFile = hostsAddIdentityFile
		entry.WorkspaceRoot = hostsAddWorkspaceRoot
		entry.HostKeyVerification = hostsAddHostKeyPosture
		entry.PushCredentials = hostsAddPushCredentials
		if err := config.ValidateHostEntry(name, entry); err != nil {
			return hostsFailJSON(cmd, err)
		}
		if err := applyHostsEdit(config.HostsEdit{Op: config.HostsEditAdd, Name: name, Host: &entry}); err != nil {
			return hostsFailJSON(cmd, err)
		}
		result := hostsAddResult{Name: name, Host: entry}
		if hostsAddDefault {
			if _, err := hostsConfigSet("default_host", name); err != nil {
				return hostsEnvelopeError(cmd, result, fmt.Errorf("saved host %q, but could not make it the default: %w", name, err), apiproto.ErrorCodeMutationCommitted)
			}
			result.Default = true
		}
		var checkErr error
		if !hostsAddNoCheck {
			resp, err := hostsCheckProbe(daemon.CheckHostRequest{Host: name})
			switch {
			case err != nil:
				checkErr = fmt.Errorf("saved host %q, but the check could not run: %w; run `af hosts check %s` once the daemon answers (an older daemon needs `af daemon restart`)", name, err, name)
			case len(resp.Hosts) == 1:
				result.Check = &resp.Hosts[0]
			}
		}
		notReady := result.Check != nil && !hostReady(*result.Check)
		if hostsJSONFlag {
			// ONE envelope on stdout, whatever happened after the save.
			switch {
			case checkErr != nil:
				return hostsEnvelopeError(cmd, result, checkErr, apiproto.ErrorCodeMutationCommitted)
			case notReady:
				return hostsEnvelopeError(cmd, result, errHostsNotReady, hostsCodeNotReady)
			}
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(result))
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "saved host %s (%s)%s\n", name, describeHost(entry), map[bool]string{true: " as the default", false: ""}[result.Default])
		if result.Check != nil {
			renderHostCheck(out, *result.Check, entry)
		}
		if checkErr != nil {
			return checkErr
		}
		if notReady {
			return errHostsNotReady
		}
		return nil
	},
}

func describeHost(h config.HostConfig) string {
	s := h.Host
	if h.User != "" {
		s = h.User + "@" + s
	}
	if h.Port != 0 {
		s += ":" + strconv.Itoa(h.Port)
	}
	return s
}

// hostReady reports a probed host that answered recognisably (git, tmux and gh
// each gave a recognised answer) with no blocking problem. A reachable host
// whose probe output was garbled/truncated has Complete false and is unknown,
// not ready — the same contract doctor honors (#5841). Absent from an older
// daemon's reply, Complete decodes false, so it too reads as not ready.
func hostReady(r daemon.HostCheckResult) bool {
	return r.Probed && r.Reachable && r.Complete && len(r.Problems) == 0
}

// renderHostCheck prints one host's probe result and, for anything short of
// ready, the exact remedy. Problems already carry their fix (git, tmux); an
// unreachable host gets the operator-owned-connectivity remedy.
func renderHostCheck(w io.Writer, r daemon.HostCheckResult, entry config.HostConfig) {
	switch {
	case !r.Probed:
		fmt.Fprintf(w, "  %s: another probe is already running (cached state: %s); rerun in a moment\n", r.Name, r.Readiness)
	case !r.Reachable:
		fmt.Fprintf(w, "  %s: could not be probed", r.Name)
		if r.Error != "" {
			fmt.Fprintf(w, ": %s", r.Error)
		}
		fmt.Fprintf(w, "\n    fix: make `%s` succeed from this machine (bring the tailnet or VPN up, load your key, trust the host key), then run `af hosts check %s`. af opens no tunnel and edits no ssh config.\n", sshRemedyCommand(entry), r.Name)
	case len(r.Problems) > 0:
		fmt.Fprintf(w, "  %s: cannot run a session yet\n", r.Name)
		for _, p := range r.Problems {
			fmt.Fprintf(w, "    fix: %s\n", p)
		}
		fmt.Fprintf(w, "    then run `af hosts check %s`\n", r.Name)
	case !r.Complete:
		// The host answered something af could not read (truncated/garbled
		// output, banner or profile noise on a probe line, a broken tmux -V).
		// That is unknown, never ready — the same arm doctor renders (#5841).
		fmt.Fprintf(w, "  %s: unknown: the host answered but not with a recognisable git, tmux and gh report", r.Name)
		if r.Readiness != "" {
			fmt.Fprintf(w, "; cached state: %s", r.Readiness)
		}
		fmt.Fprintf(w, "\n    fix: check that a non-interactive `%s` prints no banner or extra output for a plain command, then run `af hosts check %s`\n", sshRemedyCommand(entry), r.Name)
	default:
		fmt.Fprintf(w, "  %s: ready (git, tmux and gh answered)\n", r.Name)
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "    note: %s\n", warn)
	}
	renderPushCredentialBlockers(w, r.PushCredentialBlockers)
}

// renderPushCredentialBlockers names the sessions keeping a push-licensed host
// from getting push credentials (#5571 W7.8). Not a readiness problem: the host
// still runs sessions, so it changes no exit code.
func renderPushCredentialBlockers(w io.Writer, blockers []string) {
	for _, b := range blockers {
		fmt.Fprintf(w, "    push credentials withheld: %s\n", b)
	}
}

var hostsListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show the saved hosts",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requireLocalTarget("af hosts list", "reads this machine's host list"); err != nil {
			return hostsFailJSON(cmd, err)
		}
		hosts, def, err := hostsLoad()
		if err != nil {
			return hostsFailJSON(cmd, fmt.Errorf("cannot read the host list: %w", err))
		}
		names := sortedKeys(hosts)
		entries := make([]hostListEntry, 0, len(names))
		for _, n := range names {
			entries = append(entries, hostListEntry{Name: n, Host: hosts[n], Default: n == def})
		}
		if hostsJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(entries))
		}
		out := cmd.OutOrStdout()
		if len(entries) == 0 {
			fmt.Fprintln(out, "no hosts saved; add one with `af hosts add <name> <ssh-destination>`")
			return nil
		}
		for _, e := range entries {
			mark := ""
			if e.Default {
				mark = " (default)"
			}
			fmt.Fprintf(out, "%s\t%s%s\n", e.Name, describeHost(e.Host), mark)
		}
		fmt.Fprintln(out, "run `af hosts check` to probe them")
		return nil
	},
}

func sortedKeys(m map[string]config.HostConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var hostsCheckCmd = &cobra.Command{
	Use:   "check [name]",
	Short: "Probe a host (or every saved host) for what a session needs",
	Long: `Ask the daemon to probe a saved host for git, tmux (3.2 or newer) and gh, and
record the answer so the host pickers show it. With no name every saved host is
probed. Exits non-zero when a host is not ready or could not be reached.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requireLocalTarget("af hosts check", "asks this machine's daemon to probe its host list"); err != nil {
			return hostsFailJSON(cmd, err)
		}
		req := daemon.CheckHostRequest{}
		if len(args) == 1 {
			req.Host = strings.TrimSpace(args[0])
		}
		resp, err := hostsCheckProbe(req)
		if err != nil {
			return hostsFailJSON(cmd, err)
		}
		if !hostsJSONFlag && len(resp.Hosts) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no hosts saved; add one with `af hosts add <name> <ssh-destination>`")
		}
		hosts, _, _ := hostsLoad()
		blocked, unreachable, incomplete := false, false, false
		if !hostsJSONFlag && len(resp.SandboxPushCredentialBlockers) > 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "  sandbox.ssh target:")
			renderPushCredentialBlockers(cmd.OutOrStdout(), resp.SandboxPushCredentialBlockers)
		}
		for _, r := range resp.Hosts {
			if !hostsJSONFlag {
				renderHostCheck(cmd.OutOrStdout(), r, hosts[r.Name])
			}
			switch {
			case r.Probed && r.Reachable && len(r.Problems) > 0:
				blocked = true
			case !r.Probed || !r.Reachable:
				unreachable = true
			case !r.Complete:
				// A reachable, problem-free host whose probe af could not read is
				// unknown, not a silent exit 0 (#5841).
				incomplete = true
			}
		}
		var outcome error
		switch {
		case blocked:
			outcome = errHostsCheckHasProblems
		case unreachable:
			outcome = errHostsUnreachableOrBusy
		case incomplete:
			outcome = errHostsCheckIncomplete
		}
		if hostsJSONFlag {
			// One envelope on stdout: the probe results are the data, and a host
			// that is not ready is the error beside them.
			if outcome != nil {
				return hostsEnvelopeError(cmd, resp, outcome, hostsCodeNotReady)
			}
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(resp))
		}
		return outcome
	},
}

var hostsRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Drop a host from the list",
	Long: `Remove <name> from the saved hosts. Sessions already running on it keep
running: each one carries its own connection settings, so a tunnel re-dial or a
daemon restart does not need the entry; only new sessions can no longer pick it. A host that is the default is
refused unless you pass --clear-default.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requireLocalTarget("af hosts remove", "writes this machine's host list"); err != nil {
			return hostsFailJSON(cmd, err)
		}
		name := strings.TrimSpace(args[0])
		if err := applyHostsEdit(config.HostsEdit{Op: config.HostsEditRemove, Name: name, ClearDefault: hostsRemoveClearDefault}); err != nil {
			return hostsFailJSON(cmd, err)
		}
		if hostsJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(map[string]string{"removed": name}))
		}
		fmt.Fprintf(cmd.OutOrStdout(), "removed host %s\n", name)
		return nil
	},
}

func init() {
	hostsCmd.PersistentFlags().BoolVar(&hostsJSONFlag, "json", false, "Print the result as a JSON envelope")
	hostsAddCmd.Flags().StringVar(&hostsAddIdentityFile, "identity-file", "", "Absolute path of the ssh key for this host")
	hostsAddCmd.Flags().StringVar(&hostsAddWorkspaceRoot, "workspace-root", "", "Absolute directory on the host under which each session's dir is created (default ~/.af-sessions)")
	hostsAddCmd.Flags().StringVar(&hostsAddHostKeyPosture, "host-key-verification", "", "strict | accept-new | insecure (default: the global ssh.host_key_verification)")
	hostsAddCmd.Flags().BoolVar(&hostsAddDefault, "default", false, "Make this the default host")
	hostsAddCmd.Flags().BoolVar(&hostsAddNoCheck, "no-check", false, "Save the host without probing it")
	hostsAddCmd.Flags().BoolVar(&hostsAddPushCredentials, "push-credentials", false, "Opt the host into push credentials in the same write (a dedicated machine only)")
	hostsRemoveCmd.Flags().BoolVar(&hostsRemoveClearDefault, "clear-default", false, "Also clear default_host when it names this host")
	hostsCmd.AddCommand(hostsAddCmd, hostsListCmd, hostsCheckCmd, hostsRemoveCmd)
}
