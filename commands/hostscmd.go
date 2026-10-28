package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
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
	hostsRemoveClearDefault   bool
	hostsCheckProbe           = daemon.CheckHost
	hostsConfigSet            = globalConfigSet
	hostsLoad                 = loadGlobalHosts
	errHostsNotReady          = errors.New("the host is saved but not ready to run a session")
	errHostsCheckHasProblems  = errors.New("one or more hosts are not ready to run a session")
	errHostsUnreachableOrBusy = errors.New("one or more hosts could not be probed")
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

// parseHostDestination splits `[user@]host[:port]` into the entry's fields. A
// bare IPv6 literal (more than one colon) is a host, not host:port.
func parseHostDestination(dest string) (config.HostConfig, error) {
	var h config.HostConfig
	dest = strings.TrimSpace(dest)
	if user, rest, ok := strings.Cut(dest, "@"); ok {
		h.User, dest = user, rest
	}
	if strings.Count(dest, ":") == 1 {
		host, port, _ := strings.Cut(dest, ":")
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return h, fmt.Errorf("the port in %q must be a number from 1 to 65535", dest)
		}
		h.Port, dest = n, host
	}
	h.Host = dest
	return h, nil
}

func writeHostList(hosts map[string]config.HostConfig) error {
	raw, err := json.Marshal(hosts)
	if err != nil {
		return err
	}
	_, err = hostsConfigSet("hosts", string(raw))
	return err
}

func hostsFailJSON(cmd *cobra.Command, err error) error {
	return jsonWrapError(cmd, hostsJSONFlag, err)
}

var hostsAddCmd = &cobra.Command{
	Use:   "add <name> <ssh-destination>",
	Short: "Save a host and check that it can run a session",
	Long: `Save <name> as a sandbox host and probe it.

<ssh-destination> is [user@]host[:port], for example ubuntu@box1.tailnet.ts.net.
The name is what 'af sessions create --host <name>' takes. The host is probed
for git, tmux (3.2 or newer) and gh, and every missing tool is reported with
its fix. The entry is kept either way; use --no-check to skip the probe.`,
	Example: `  af hosts add box1 ubuntu@box1.tailnet.ts.net
  af hosts add gpu 10.0.0.7:2222 --identity-file /home/me/.ssh/gpu --default`,
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
		if err := config.ValidateHostEntry(name, entry); err != nil {
			return hostsFailJSON(cmd, err)
		}
		hosts, _, err := hostsLoad()
		if err != nil {
			return hostsFailJSON(cmd, fmt.Errorf("cannot read the host list: %w", err))
		}
		if _, exists := hosts[name]; exists {
			return hostsFailJSON(cmd, fmt.Errorf("host %q is already listed; remove it first with `af hosts remove %s`, or probe it with `af hosts check %s`", name, name, name))
		}
		hosts[name] = entry
		if err := writeHostList(hosts); err != nil {
			return hostsFailJSON(cmd, err)
		}
		if hostsAddDefault {
			if _, err := hostsConfigSet("default_host", name); err != nil {
				return hostsFailJSON(cmd, fmt.Errorf("saved host %q, but could not make it the default: %w", name, err))
			}
		}
		result := hostsAddResult{Name: name, Host: entry, Default: hostsAddDefault}
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
		if hostsJSONFlag {
			if err := apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(result)); err != nil {
				return err
			}
		} else {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "saved host %s (%s)%s\n", name, describeHost(entry), map[bool]string{true: " as the default", false: ""}[hostsAddDefault])
			if result.Check != nil {
				renderHostCheck(out, *result.Check, entry)
			}
		}
		if checkErr != nil {
			return checkErr
		}
		if result.Check != nil && !hostReady(*result.Check) {
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

// hostReady reports a probed host that answered, with no blocking problem.
func hostReady(r daemon.HostCheckResult) bool {
	return r.Probed && r.Reachable && len(r.Problems) == 0
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
		fmt.Fprintf(w, "\n    fix: make `ssh %s` work non-interactively from this machine (bring the tailnet or VPN up, load your key, trust the host key), then run `af hosts check %s`. af opens no tunnel and edits no ssh config.\n", describeHost(entry), r.Name)
	case len(r.Problems) > 0:
		fmt.Fprintf(w, "  %s: cannot run a session yet\n", r.Name)
		for _, p := range r.Problems {
			fmt.Fprintf(w, "    fix: %s\n", p)
		}
		fmt.Fprintf(w, "    then run `af hosts check %s`\n", r.Name)
	default:
		fmt.Fprintf(w, "  %s: ready (git, tmux and gh answered)\n", r.Name)
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "    note: %s\n", warn)
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
		if hostsJSONFlag {
			if err := apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(resp)); err != nil {
				return err
			}
		} else if len(resp.Hosts) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no hosts saved; add one with `af hosts add <name> <ssh-destination>`")
		}
		hosts, _, _ := hostsLoad()
		blocked, unreachable := false, false
		for _, r := range resp.Hosts {
			if !hostsJSONFlag {
				renderHostCheck(cmd.OutOrStdout(), r, hosts[r.Name])
			}
			switch {
			case r.Probed && r.Reachable && len(r.Problems) > 0:
				blocked = true
			case !r.Probed || !r.Reachable:
				unreachable = true
			}
		}
		switch {
		case blocked:
			return errHostsCheckHasProblems
		case unreachable:
			return errHostsUnreachableOrBusy
		}
		return nil
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
		hosts, def, err := hostsLoad()
		if err != nil {
			return hostsFailJSON(cmd, fmt.Errorf("cannot read the host list: %w", err))
		}
		if err := config.ValidateHostPick(hosts, name); err != nil {
			return hostsFailJSON(cmd, err)
		}
		if def == name {
			if !hostsRemoveClearDefault {
				return hostsFailJSON(cmd, fmt.Errorf("host %q is the default_host; pass --clear-default to remove it and clear the default, or point default_host elsewhere first (`af config set default_host <name>`)", name))
			}
			if _, err := hostsConfigSet("default_host", ""); err != nil {
				return hostsFailJSON(cmd, fmt.Errorf("could not clear default_host: %w", err))
			}
		}
		delete(hosts, name)
		if err := writeHostList(hosts); err != nil {
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
	hostsRemoveCmd.Flags().BoolVar(&hostsRemoveClearDefault, "clear-default", false, "Also clear default_host when it names this host")
	hostsCmd.AddCommand(hostsAddCmd, hostsListCmd, hostsCheckCmd, hostsRemoveCmd)
}
