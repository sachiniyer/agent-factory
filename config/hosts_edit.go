package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// A per-entry edit of the global host table that is ONE read-modify-write under
// the config file lock (#5855).
//
// `af hosts add/remove` used to read the table, change one entry and write the
// whole table back through `config set hosts`, so the read happened outside the
// lock `config set` holds: two concurrent edits each rewrote the table from a
// stale copy and the later write dropped the other's change. The lock cannot be
// held by the CLI across its own read and the write, because the write is made
// by the daemon (another process, so it would wait on the CLI's lock forever).
// So the edit travels instead: the writer receives "add this entry" / "remove
// that name" and performs the read, the existence and default checks and the
// write inside a single hold of the same exclusive lock every other config
// writer takes. Concurrent writers block on that lock and run one after the
// other; nothing retries and nothing is lost.

// HostsEdit is one edit of the host table.
type HostsEdit struct {
	// Op is "add" or "remove".
	Op string `json:"op"`
	// Name is the host's list name.
	Name string `json:"name"`
	// Host is the entry to add (Op "add").
	Host *HostConfig `json:"host,omitempty"`
	// ClearDefault, for a remove, also clears default_host when it names Name,
	// in the same lock hold. Without it removing the default is refused.
	ClearDefault bool `json:"clear_default,omitempty"`
}

const (
	HostsEditAdd    = "add"
	HostsEditRemove = "remove"
)

func prepareStructuredWrite(key, raw string) (scalarWrite, error) {
	section, leaf, spec, ok := resolveSettable(key)
	if !ok {
		return scalarWrite{}, unsettableConfigKeyError(key, "")
	}
	key = canonicalConfigKey(key)
	structured := spec.structured && section == ""
	canonical, encoded, err := canonicalizeConfigValue(key, spec, structured, raw)
	if err != nil {
		return scalarWrite{}, fmt.Errorf("invalid value for %s: %w", key, err)
	}
	return scalarWrite{key: key, section: section, leaf: leaf, canonical: canonical, encoded: encoded,
		structured: structured, rawStructured: raw, clear: spec.kind == cfgStringList && canonical == ""}, nil
}

// EditGlobalHosts applies edit to the global host table under the config file
// lock and returns the SetResult of the `hosts` write plus the digest of the
// bytes left on disk (as SetGlobalConfigValueWithDigest does).
func EditGlobalHosts(edit HostsEdit) (*SetResult, ConfigDigest, error) {
	if edit.Op != HostsEditAdd && edit.Op != HostsEditRemove {
		return nil, ConfigDigest{}, fmt.Errorf("unknown hosts edit %q", edit.Op)
	}
	if edit.Op == HostsEditAdd {
		if edit.Host == nil {
			return nil, ConfigDigest{}, fmt.Errorf("a hosts add needs an entry")
		}
		if err := ValidateHostEntry(edit.Name, *edit.Host); err != nil {
			return nil, ConfigDigest{}, err
		}
	}
	if _, err := LoadConfig(); err != nil {
		return nil, ConfigDigest{}, fmt.Errorf("refusing to write: the current config does not load: %w", err)
	}
	configDir, err := GetConfigDir()
	if err != nil {
		return nil, ConfigDigest{}, err
	}
	tomlPath := filepath.Join(configDir, TomlConfigFileName)
	prettyPath := prettyHomePath(tomlPath)

	var result *SetResult
	var digest ConfigDigest
	err = withFollowedFileLock(tomlPath, func(locked lockedTarget) error {
		current, err := locked.read()
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to read %s: %w", prettyPath, err)
		}
		current = stripUTF8BOM(current)
		before := DefaultConfig()
		if !isEffectivelyEmptyToml(current) {
			if before, err = parseConfigTOML(current, prettyPath); err != nil {
				return fmt.Errorf("refusing to write: the current config does not load: %w", err)
			}
		}
		hosts := make(map[string]HostConfig, len(before.Hosts)+1)
		for k, v := range before.Hosts {
			hosts[k] = v
		}
		clearDefault := false
		switch edit.Op {
		case HostsEditAdd:
			if _, exists := hosts[edit.Name]; exists {
				return fmt.Errorf("host %q is already listed; remove it first with `af hosts remove %s`, or probe it with `af hosts check %s`", edit.Name, edit.Name, edit.Name)
			}
			hosts[edit.Name] = *edit.Host
		case HostsEditRemove:
			if err := ValidateHostPick(hosts, edit.Name); err != nil {
				return err
			}
			if before.DefaultHost == edit.Name {
				if !edit.ClearDefault {
					return fmt.Errorf("host %q is the default_host; pass --clear-default to remove it and clear the default, or point default_host elsewhere first (`af config set default_host <name>`)", edit.Name)
				}
				clearDefault = true
			}
			delete(hosts, edit.Name)
		}
		// `config set` refuses a list that dangles the default, so the default goes
		// first; both writes are inside this one lock hold.
		if clearDefault {
			dw, err := prepareStructuredWrite("default_host", "")
			if err != nil {
				return err
			}
			if _, _, err := dw.apply(locked, prettyPath); err != nil {
				return fmt.Errorf("could not clear default_host: %w", err)
			}
		}
		raw, err := json.Marshal(hosts)
		if err != nil {
			return err
		}
		hw, err := prepareStructuredWrite("hosts", string(raw))
		if err != nil {
			return err
		}
		result, digest, err = hw.apply(locked, prettyPath)
		return err
	})
	if err != nil {
		return nil, ConfigDigest{}, err
	}
	return result, digest, nil
}
