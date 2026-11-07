package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pelletier/go-toml/v2"
)

// validateListenAddrValue reports whether value is an acceptable listen_addr.
// It is the FIRST validation this key has ever had: unlike
// default_program / worktree_root / update_channel, listen_addr has no
// validateConfig rule to reuse — the loader accepts any string and the only gate
// is net.Listen at daemon bind time (daemon/tcpserver.go startTCPListener), which
// fails long after `af config set` returned success.
//
// So rather than invent a second, divergent rule, this reproduces net.Listen's
// own ADDRESS-PARSING verdict exactly, using the two stdlib calls net.Listen
// parses a TCP address with. Verified case by case against net.Listen:
//
//	"127.0.0.1:8443", ":8443" (all interfaces), "[::1]:8443", "localhost:8443",
//	"0.0.0.0:0" (random port), "127.0.0.1:" (random port), "localhost:http"
//	                                                     → accepted by both
//	"8443" (no port), "foo:bar" (unknown service), "127.0.0.1:99999"
//	                                                     → rejected by both
//
// Note the two easy-to-get-wrong cases: an EMPTY port ("127.0.0.1:") and a
// service-name port ("localhost:http") are both valid to net.Listen, so a
// hand-rolled "must be a numeric port" rule would reject addresses a hand-edit
// accepts. net.LookupPort resolves both the way net.Listen does.
//
// What it deliberately does NOT check is anything that can only fail at bind
// time and would need the network (or the future) to answer: whether the host
// resolves, whether the port is free, whether we may bind it. "not a host:8443"
// parses fine here and fails at bind — the same as a hand-edit, which is the
// point. A bind failure is logged and skipped without blocking the daemon, so
// the cost of that residue is a warning, not a wedged startup.
//
// An empty value is valid and load-bearing: it is the documented opt-out that
// disables the web server entirely (a pure-unix daemon). See the ListenAddr doc
// comment in config_types.go and docs/configuration.md.
func validateListenAddrValue(value string) error {
	if value == "" {
		return nil
	}
	// Validate the exact bytes that will be written and later handed to
	// net.Listen — no trimming, so `config set` can never accept a value the
	// daemon would then reject.
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("listen_addr must be a host:port address (e.g. 127.0.0.1:8443, 0.0.0.0:8443) "+
			"or \"\" to disable the web server, got %q: %w", value, err)
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return fmt.Errorf("listen_addr port is not valid in %q: %w", value, err)
	}
	return nil
}

// DefaultListenAddr is the built-in network.listen_addr every install gets
// unless the operator sets another: the loopback web/API port the docs,
// bookmarks and proxies assume. The daemon's no-takeover gate compares a
// contested bind against it — "my listen_addr is only the default" is a
// value question, not a file-presence one, because first-run materialization
// writes the key into every config.toml (#6858).
func DefaultListenAddr() string { return defaultListenAddr }

// listenAddrInHomeMaxBytes bounds the config.toml bytes ListenAddrInHome will
// read. A daemon's own full parse has no bound, but this read happens inside
// the daemon's listener reconcile lock, on a file that lives in ANOTHER home
// and can be anything — including a special file. 64KiB is ~50x a real
// config.toml; past it the answer stops being worth the wait.
const listenAddrInHomeMaxBytes = 64 << 10

// ListenAddrInHome reports the network.listen_addr a daemon running in homeDir
// (an AF home OTHER than the caller's own) would take: config.toml when
// present is authoritative — the grouped [network] listen_addr leaf when it
// exists (it wins over the flat spelling, matching applyGroupedConfigAliases),
// else the flat listen_addr, else the built-in default. With no config.toml
// the legacy config.json answers the same question the loader's conversion
// path would: flat listen_addr, else the default. found is false only when
// the home has no config file at all. A file that exists but fails to read or
// parse reports an error, so the caller can tell "no claim" apart from
// "claim we could not read". Read-only: no materialization, no writes, no
// provenance.
//
// This deliberately does NOT call parseConfigTOML or parseConfigJSON. The
// probe runs while the daemon holds its listener lock, so it must be bounded
// (a symlinked or special config cannot block it — O_NOFOLLOW|O_NONBLOCK,
// regular-file check, size cap) and quiet (the file is another home's, so its
// unknown-key and schema warnings are not ours to re-log on every reconcile).
// Used by the daemon's #6858 no-takeover gate to ask whether a claimed-home
// daemon would bind the contested port.
func ListenAddrInHome(homeDir string) (addr string, found bool, err error) {
	data, err := readFileNoFollowBounded(filepath.Join(homeDir, TomlConfigFileName), listenAddrInHomeMaxBytes)
	switch {
	case err == nil:
		addr, err = listenAddrFromTOML(data)
	case errors.Is(err, fs.ErrNotExist):
		data, err = readFileNoFollowBounded(filepath.Join(homeDir, ConfigFileName), listenAddrInHomeMaxBytes)
		switch {
		case err == nil:
			addr, err = listenAddrFromJSON(data)
		case errors.Is(err, fs.ErrNotExist):
			return "", false, nil
		default:
			return "", false, err
		}
	default:
		return "", false, err
	}
	if err != nil {
		return "", true, err
	}
	return addr, true, nil
}

// listenAddrFromTOML resolves the effective network.listen_addr a config.toml
// would give a daemon: the grouped [network] leaf when present (it wins over
// the flat spelling, matching applyGroupedConfigAliases), else the flat
// listen_addr, else the built-in default. Nothing else in the file is
// relevant — an unreadable value anywhere else is not this read's business,
// and none of the loader's warnings apply to a file we do not own. The BOM
// strip matches the production loader — go-toml/v2 does not skip it on its
// own, and a BOM-prefixed file must parse the same here as it does there.
func listenAddrFromTOML(data []byte) (string, error) {
	var shape map[string]any
	if err := toml.Unmarshal(stripUTF8BOM(data), &shape); err != nil {
		return "", err
	}
	if table, ok := shape["network"].(map[string]any); ok {
		if v, present := table["listen_addr"]; present {
			return listenAddrLeaf(v, "network.listen_addr")
		}
	}
	if v, present := shape["listen_addr"]; present {
		return listenAddrLeaf(v, "listen_addr")
	}
	return DefaultListenAddr(), nil
}

// listenAddrFromJSON resolves the effective listen_addr a legacy config.json
// would give a daemon: the flat listen_addr key — the grouped [network]
// spelling is TOML-only — else the built-in default. A zero-byte file is the
// empty-stub shape the loader answers with all defaults. An explicit null is
// the JSON no-op: production unmarshals onto a defaults-filled Config, where
// null leaves the prefilled default standing.
func listenAddrFromJSON(data []byte) (string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return DefaultListenAddr(), nil
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(data, &shape); err != nil {
		return "", err
	}
	raw, present := shape["listen_addr"]
	if !present {
		return DefaultListenAddr(), nil
	}
	var s *string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("listen_addr is not a string")
	}
	if s == nil {
		return DefaultListenAddr(), nil
	}
	return *s, nil
}

func listenAddrLeaf(v any, key string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s is not a string", key)
	}
	return s, nil
}

// readFileNoFollowBounded reads path without following a final symlink and
// without ever blocking on a special file: O_NOFOLLOW|O_NONBLOCK at open,
// a regular-file fstat check, and a byte cap on the read. The path here can
// name anything — a config.toml inside another AF home is a file this daemon
// did not create — so the managed-file rules readManagedFileNoFollow encodes
// for daemon-owned files apply here too: never block the listener lock on a
// FIFO, never chase a link swapped in between stat and open, never trust a
// file bigger than the config this function exists to answer for.
func readFileNoFollowBounded(path string, maxBytes int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxBytes)
	}
	return data, nil
}

// IsLoopbackListenAddr reports whether a listen_addr binds ONLY the loopback
// interface (127.0.0.1 / ::1 / localhost). It governs the loopback token
// exemption, and the distinction is load-bearing for security now that the
// recommended way to add TLS is a same-host reverse proxy:
//
// A reverse proxy on the same host (nginx/Caddy terminating TLS) connects to the
// daemon from 127.0.0.1, so EVERY request it forwards has a loopback RemoteAddr —
// indistinguishable from a genuine same-machine user. If the loopback exemption
// applied on a network-bound listener, all proxied traffic would skip the token
// and reach the control plane unauthenticated. So the exemption is safe ONLY when
// the listener itself is loopback-bound (where a loopback RemoteAddr truly is a
// local peer). On a NETWORK bind (0.0.0.0, a routable/Tailscale IP, or an empty
// host = every interface) the exemption is withheld and the token is required for
// all peers, loopback-origin included. An unparseable address fails safe to "not
// loopback" (token enforced).
func IsLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		host = strings.TrimSpace(addr)
	}
	if host == "" {
		return false // empty host binds every interface — network-reachable
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
