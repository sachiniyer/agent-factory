package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

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
//	identity_file = "~/.ssh/af_box1"
//	workspace_root = "~/af-work"
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
}

const maxHostNameLen = 63

var (
	hostNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	// Hostnames, tailnet names, IPv4 and bracket-free IPv6 literals.
	hostAddrRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	sshUserRE  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
)

// ValidateHostName reports why name cannot be a host-list key.
func ValidateHostName(name string) error {
	if name == "" {
		return fmt.Errorf("host name must not be empty")
	}
	if len(name) > maxHostNameLen || !hostNameRE.MatchString(name) {
		return fmt.Errorf("host name %q must be 1-%d characters of letters, digits, '_' or '-', starting with a letter or digit", name, maxHostNameLen)
	}
	return nil
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
	if !hostAddrRE.MatchString(entry.Host) {
		return fmt.Errorf("hosts.%s.host %q must be a hostname or IP address (letters, digits, '.', '-', ':'; no user@, no port, not starting with '-'); use the user and port fields", name, entry.Host)
	}
	if entry.User != "" && !sshUserRE.MatchString(entry.User) {
		return fmt.Errorf("hosts.%s.user %q must be a plain login name (letters, digits, '.', '_', '-'; not starting with '-')", name, entry.User)
	}
	if entry.Port < 0 || entry.Port > 65535 {
		return fmt.Errorf("hosts.%s.port must be between 1 and 65535 (or omitted for 22), got %d", name, entry.Port)
	}
	for field, v := range map[string]string{"identity_file": entry.IdentityFile, "workspace_root": entry.WorkspaceRoot} {
		if strings.ContainsAny(v, "\x00\r\n") {
			return fmt.Errorf("hosts.%s.%s must not contain control characters", name, field)
		}
		if strings.HasPrefix(v, "-") {
			return fmt.Errorf("hosts.%s.%s %q must not start with '-'", name, field, v)
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

// HostByName returns the named host from the list.
func (c *Config) HostByName(name string) (HostConfig, bool) {
	h, ok := c.Hosts[name]
	return h, ok
}
