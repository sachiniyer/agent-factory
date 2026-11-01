package config

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/pelletier/go-toml/v2"

	"github.com/sachiniyer/agent-factory/log"
)

// The named host list (#5571 W6.1). `hosts` maps an operator-chosen name to an
// ssh destination, and `default_host` names the one used when a session does not
// pick. The declaration is GLOBAL-ONLY, the sibling of sandbox.ssh: af executes
// against these entries and sends credentials to them, so a repository must not
// be able to set or extend the list (the keys never join InRepoConfig or the
// personal project layer). `sandbox.ssh` and the in-repo `[ssh] host` stay
// working as implicit unnamed hosts; this slice only declares and validates the
// list — composing the ssh command (W6.2) and picking a host (W6.3) consume it.
//
//	default_host = "box1"
//
//	[hosts.box1]
//	host = "box1.tailnet.ts.net"
//	user = "dev"
//	port = 2222
//	identity_file = "/home/dev/.ssh/af_box1"
//	workspace_root = "/home/dev/af-work"
type HostConfig struct {
	// Host is the ssh destination: a hostname, tailnet name or IP address. It is
	// required. It never carries a user or port — those are the fields below — and
	// it can never begin with "-", so it cannot be read as an ssh option.
	Host string `json:"host" toml:"host"`
	// User is the ssh login user. Optional — empty means the daemon's own user.
	User string `json:"user,omitempty" toml:"user,omitempty"`
	// Port is the ssh port. Optional — 0 means the default (22).
	Port int `json:"port,omitempty" toml:"port,omitempty"`
	// IdentityFile is the path to the private key af offers to this host.
	// Optional — empty defers to the ssh agent and default keys, as ssh.identity_file does.
	IdentityFile string `json:"identity_file,omitempty" toml:"identity_file,omitempty"`
	// WorkspaceRoot is the directory on the host under which each session gets
	// its own workspace dir. Optional — empty means the host's default session root.
	WorkspaceRoot string `json:"workspace_root,omitempty" toml:"workspace_root,omitempty"`
	// HostKeyVerification overrides ssh.host_key_verification for this host only:
	// "strict", "accept-new" or "insecure". Optional — empty uses the global
	// setting, whose own default is strict. Incompatible with UseSSHConfig, where
	// the operator's ssh_config is the host-key authority instead.
	HostKeyVerification string `json:"host_key_verification,omitempty" toml:"host_key_verification,omitempty"`
	// UseSSHConfig opts this host into the operator's ssh_config (jump hosts,
	// ProxyJump, ProxyCommand, per-host keys). Off by default: af then composes the
	// whole ssh command itself with `-F none` and its own host-key posture, the way
	// backend = "ssh" does. When on, af still pins the connection to one machine,
	// but host-key checking is ssh's own, exactly as for sandbox.ssh.
	UseSSHConfig bool `json:"use_ssh_config,omitempty" toml:"use_ssh_config,omitempty"`
	// PushCredentials opts this host into push credentials (#5571 W7.8): a
	// session placed here can `git push` its origin and run `gh` as the operator.
	// The token is the daemon box's own GitHub login, fetched by the session over
	// its scoped af callback each time git or gh asks, and never stored on the
	// host. Off by default, so listing a host never starts sending the operator's
	// GitHub authority to it by itself; the daemon reads it live on every request,
	// so turning it off stops the next answer.
	PushCredentials bool `json:"push_credentials,omitempty" toml:"push_credentials,omitempty"`
}

const maxHostNameLen = 62 // after the first character

var (
	hostNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// Hostnames, tailnet names and IPv4 literals. A bracket-free IPv6 literal is
	// checked separately (validHostAddress): its leading "::" cannot satisfy a
	// hostname's alphanumeric first character, and a colon-bearing value must be a
	// real IP, never "host:port".
	hostnameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	sshUserRE  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
)

// ValidateHostName reports why name cannot be a host-list key.
func ValidateHostName(name string) error {
	if name == "" {
		return fmt.Errorf("host name must not be empty")
	}
	if !hostNameRE.MatchString(name) {
		return fmt.Errorf("host name %q must be 1-%d characters of lowercase letters, digits or '-', starting with a letter or digit", name, maxHostNameLen+1)
	}
	return nil
}

// validHostAddress reports whether addr is a hostname, an IPv4 literal or a
// bare IPv6 literal (including "::1" and "::ffff:192.0.2.1"). A value with a
// colon must parse as an IP address: that is what keeps an embedded ":port"
// ("box:2222") out of the host field — it would conflict with the port field at
// create — while still admitting every IPv6 form the ssh runtime supports. A
// zone ("fe80::1%eth0") is refused: "%" is not a character af hands to ssh here.
func validHostAddress(addr string) bool {
	if strings.Contains(addr, ":") {
		ip, err := netip.ParseAddr(addr)
		return err == nil && ip.Zone() == ""
	}
	return hostnameRE.MatchString(addr)
}

// ValidateHostEntry reports why entry cannot be the host named name. Every
// string that later reaches an ssh argv is checked here so a value can never be
// parsed as an ssh option or smuggle whitespace.
func ValidateHostEntry(name string, entry HostConfig) error {
	if err := ValidateHostName(name); err != nil {
		return err
	}
	if entry.Host == "" {
		return fmt.Errorf("hosts.%s.host is required (the ssh destination, e.g. \"box1.tailnet.ts.net\")", name)
	}
	if !validHostAddress(entry.Host) {
		return fmt.Errorf("hosts.%s.host %q must be a hostname or IP address (letters, digits, '.', '-', or a bare IPv6 literal; no user@, no :port, not starting with '-'); use the user and port fields", name, entry.Host)
	}
	if entry.User != "" && !sshUserRE.MatchString(entry.User) {
		return fmt.Errorf("hosts.%s.user %q must be a plain login name (letters, digits, '.', '_', '-'; not starting with '-')", name, entry.User)
	}
	if entry.Port < 0 || entry.Port > 65535 {
		return fmt.Errorf("hosts.%s.port must be between 1 and 65535 (or omitted for 22), got %d", name, entry.Port)
	}
	if entry.HostKeyVerification != "" {
		if !IsValidSSHHostKeyVerification(entry.HostKeyVerification) {
			return fmt.Errorf("hosts.%s.host_key_verification must be one of [%s, %s, %s], got %q",
				name, SSHHostKeyStrict, SSHHostKeyAcceptNew, SSHHostKeyInsecure, entry.HostKeyVerification)
		}
		if entry.UseSSHConfig {
			return fmt.Errorf("hosts.%s sets both host_key_verification and use_ssh_config; with use_ssh_config your ssh_config decides host-key checking, so remove host_key_verification", name)
		}
	}
	for field, v := range map[string]string{"identity_file": entry.IdentityFile, "workspace_root": entry.WorkspaceRoot} {
		if v == "" {
			continue
		}
		if strings.ContainsFunc(v, unicode.IsControl) {
			return fmt.Errorf("hosts.%s.%s must not contain control characters", name, field)
		}
		if !strings.HasPrefix(v, "/") {
			return fmt.Errorf("hosts.%s.%s %q must be an absolute path (starting with '/'; '~' is not expanded)", name, field, v)
		}
	}
	return nil
}

// ValidateHosts validates the whole list and the default_host that must name an
// entry of it. It is the strict form the writer runs; the loader uses
// sanitizeHosts, which warns and drops instead.
func ValidateHosts(hosts map[string]HostConfig, defaultHost string) error {
	for _, name := range sortedHostNames(hosts) {
		if err := ValidateHostEntry(name, hosts[name]); err != nil {
			return err
		}
	}
	if defaultHost != "" {
		if _, ok := hosts[defaultHost]; !ok {
			return fmt.Errorf("default_host %q is not in the host list %v; add it under [hosts.%s] or pick a listed host", defaultHost, sortedHostNames(hosts), defaultHost)
		}
	}
	return nil
}

// SortedHostNames is the host names in order.
func SortedHostNames(hosts map[string]HostConfig) []string { return sortedHostNames(hosts) }

func sortedHostNames(hosts map[string]HostConfig) []string {
	names := make([]string, 0, len(hosts))
	for name := range hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sanitizeHosts drops invalid entries in place, one warning each, and clears a
// default_host that does not name a surviving entry. Warn-and-drop, as
// sanitizeLimitPatterns: a bad optional entry must not block config load and
// with it the whole TUI/CLI. Dropping (not keeping) guarantees every consumer
// only ever sees validated hosts.
func sanitizeHosts(config *Config) {
	for name, entry := range config.Hosts {
		if err := ValidateHostEntry(name, entry); err != nil {
			log.WarningLog.Printf("ignoring host %q: %v", name, err)
			delete(config.Hosts, name)
		}
	}
	if len(config.Hosts) == 0 {
		config.Hosts = nil
	}
	if config.DefaultHost != "" {
		if _, ok := config.Hosts[config.DefaultHost]; !ok {
			log.WarningLog.Printf("default_host %q is not a valid listed host; ignoring it", config.DefaultHost)
			config.DefaultHost = ""
		}
	}
}

// ValidateHostPick reports why name cannot be picked from hosts: it must name an
// entry of the operator's global list. It is the ONE refusal every pick surface
// uses (create, the catalog, send-prompt --create, task definitions, the TUI
// and the web), so they cannot word or decide it differently. An empty name is
// not a pick and is not an error.
func ValidateHostPick(hosts map[string]HostConfig, name string) error {
	if name == "" {
		return nil
	}
	if _, ok := hosts[name]; !ok {
		return fmt.Errorf("host %q is not in the host list %v; add it under [hosts.%s] in the global config or pick a listed host",
			name, sortedHostNames(hosts), name)
	}
	return nil
}

// HostByName returns the named host from the list.
func (c *Config) HostByName(name string) (HostConfig, bool) {
	h, ok := c.Hosts[name]
	return h, ok
}

// ValidateHostsInTOML runs the strict ValidateHosts over the host list and
// default_host exactly as the bytes spell them, BEFORE the loader's sanitizeHosts
// can silently drop a dangling default. The writer calls it so `af config set`
// refuses what a load would discard: success from set must mean the setting
// takes effect.
func ValidateHostsInTOML(data []byte) error {
	var raw struct {
		Hosts       map[string]HostConfig `toml:"hosts"`
		DefaultHost string                `toml:"default_host"`
	}
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil // a parse error is the final gate's to report, with its own wording
	}
	return ValidateHosts(raw.Hosts, raw.DefaultHost)
}

// validateOptionalHostName accepts the empty default_host (no default) and
// otherwise checks the name's shape.
func validateOptionalHostName(v string) error {
	if v == "" {
		return nil
	}
	return ValidateHostName(v)
}
