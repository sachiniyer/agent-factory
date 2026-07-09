package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"github.com/sachiniyer/agent-factory/log"
)

// LoadConfig reads the user's config file, validates it, and returns the
// resulting Config.
//
// Format resolution (#1030): config.toml is the canonical global config.
// When it exists it is authoritative and config.json is ignored (with a
// warning naming the shadowed file) — the two are never merged. When only a
// legacy config.json exists it is converted to config.toml once, on this
// load (convertJSONToTOML); from then on config.toml is the file to edit.
// First run, with neither file present, materializes config.toml directly.
//
// Error handling distinguishes "no config yet" from "config present but
// unusable" so a user whose settings are being ignored gets told why instead
// of silently inheriting defaults (#734):
//   - Neither file exists → DefaultConfig() is materialized as config.toml,
//     with no error. This is the first-run path and must keep working.
//   - A contentless config file (zero-byte or, for TOML, whitespace/BOM-only)
//     with no other config present is the fingerprint of a failed/partial
//     first-run write (#864, a regression of #838): the stub is removed and
//     defaults regenerated rather than wedging every future startup. A
//     contentless config.toml sitting beside a real config.json is instead a
//     hand-made shadow and stays a loud error, so re-materializing can never
//     silently discard the config.json's settings.
//   - A file exists but cannot be read (permission denied, disk error), or is
//     non-empty yet fails to parse → an error naming the file and the
//     underlying cause is returned. Defaults are NOT substituted, since doing
//     so would hide the user's broken config behind a working-looking app; a
//     config.json that fails to parse is also NOT converted (nothing is
//     renamed), so the user can fix it in place.
//   - A file parses but fails enum validation → an actionable migration error
//     is returned.
//
// This mirrors the error-propagation contract already adopted by
// LoadRepoConfig, where only os.IsNotExist yields defaults.
func LoadConfig() (*Config, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get config directory: %w", err)
	}

	configPath := filepath.Join(configDir, ConfigFileName)
	prettyConfigPath := prettyHomePath(configPath)
	tomlPath := filepath.Join(configDir, TomlConfigFileName)
	prettyTomlPath := prettyHomePath(tomlPath)

	// 1. config.toml is canonical whenever it exists.
	tomlData, tomlErr := os.ReadFile(tomlPath)
	if tomlErr == nil {
		jsonExists := fileExists(configPath)
		if isEffectivelyEmptyToml(tomlData) {
			// A contentless config.toml is ambiguous now that first run
			// materializes TOML: it is either a failed first-run write (#864)
			// or a hand-made stub. Disambiguate by config.json — with a real
			// config.json beside it, re-materializing would silently discard
			// those settings, so keep the loud shadow error; with no
			// config.json it is a failed first-run stub, so drop it and
			// re-materialize, exactly as the JSON path does for an empty
			// config.json.
			if jsonExists {
				return parseConfigTOML(tomlData, prettyTomlPath)
			}
			if rmErr := os.Remove(tomlPath); rmErr != nil && !os.IsNotExist(rmErr) {
				return nil, fmt.Errorf("failed to remove empty config file %s: %w", prettyTomlPath, rmErr)
			}
			return materializeDefaultConfig(configDir, tomlPath, prettyTomlPath)
		}
		if jsonExists {
			log.WarningLog.Printf("both %s and %s exist; %s is canonical and %s is ignored — delete or rename %s to silence this warning",
				prettyTomlPath, prettyConfigPath, prettyTomlPath, prettyConfigPath, prettyConfigPath)
		}
		return parseConfigTOML(tomlData, prettyTomlPath)
	}
	if !os.IsNotExist(tomlErr) {
		return nil, fmt.Errorf("failed to read config file %s: %w", prettyTomlPath, tomlErr)
	}

	// 2. No config.toml. Read the legacy config.json.
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Neither file exists → first run: materialize config.toml.
			return materializeDefaultConfig(configDir, tomlPath, prettyTomlPath)
		}
		return nil, fmt.Errorf("failed to read config file %s: %w", prettyConfigPath, err)
	}

	// A zero-byte config.json with no config.toml is a failed first-run write
	// from a pre-TOML af (#864). Drop the stub and materialize fresh defaults
	// as config.toml rather than wedging on the #758 "config is empty" error.
	if len(data) == 0 {
		if rmErr := os.Remove(configPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return nil, fmt.Errorf("failed to remove empty config file %s: %w", prettyConfigPath, rmErr)
		}
		return materializeDefaultConfig(configDir, tomlPath, prettyTomlPath)
	}

	// 3. A real config.json with no config.toml → one-time conversion.
	return convertJSONToTOML(configDir, configPath, tomlPath, prettyConfigPath, prettyTomlPath)
}

// ReadOnlyConfigLoad is a no-write config snapshot for diagnostics.
type ReadOnlyConfigLoad struct {
	Config       *Config
	Path         string
	Missing      bool
	LegacyJSON   bool
	ShadowedJSON bool
}

// LoadConfigReadOnly reads and validates the active global config without
// materializing defaults, converting config.json, removing empty stubs, or
// writing any file. It is intended for diagnostics such as `af doctor`.
func LoadConfigReadOnly() (ReadOnlyConfigLoad, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return ReadOnlyConfigLoad{}, fmt.Errorf("failed to get config directory: %w", err)
	}

	configPath := filepath.Join(configDir, ConfigFileName)
	prettyConfigPath := prettyHomePath(configPath)
	tomlPath := filepath.Join(configDir, TomlConfigFileName)
	prettyTomlPath := prettyHomePath(tomlPath)

	tomlData, tomlErr := os.ReadFile(tomlPath)
	if tomlErr == nil {
		cfg, err := parseConfigTOML(tomlData, prettyTomlPath)
		return ReadOnlyConfigLoad{
			Config:       cfg,
			Path:         tomlPath,
			ShadowedJSON: fileExists(configPath),
		}, err
	}
	if !os.IsNotExist(tomlErr) {
		return ReadOnlyConfigLoad{Path: tomlPath}, fmt.Errorf("failed to read config file %s: %w", prettyTomlPath, tomlErr)
	}

	data, err := os.ReadFile(configPath)
	if err == nil {
		cfg, parseErr := parseConfig(data, prettyConfigPath)
		return ReadOnlyConfigLoad{
			Config:     cfg,
			Path:       configPath,
			LegacyJSON: true,
		}, parseErr
	}
	if !os.IsNotExist(err) {
		return ReadOnlyConfigLoad{Path: configPath}, fmt.Errorf("failed to read config file %s: %w", prettyConfigPath, err)
	}

	return ReadOnlyConfigLoad{Path: tomlPath, Missing: true}, nil
}

// fileExists reports whether path exists (any stat error other than
// not-exist — e.g. permission denied — counts as "exists" so a shadowed
// config.json is still flagged rather than silently assumed gone).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !os.IsNotExist(err)
}

// availableBackupPath returns the first backup path that does not yet exist:
// base, then base.1, base.2, … so an existing backup is never overwritten.
// The original backup (base) is left untouched, preserving the oldest — and
// most likely real-settings — copy across repeated conversions. Returns an
// error only if an absurd number of backups already exist, in which case the
// caller leaves config.json in place with a warning rather than clobbering.
func availableBackupPath(base string) (string, error) {
	if !fileExists(base) {
		return base, nil
	}
	for i := 1; i < 1000; i++ {
		candidate := fmt.Sprintf("%s.%d", base, i)
		if !fileExists(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s and %s.1..999 all exist; refusing to overwrite any backup", base, base)
}

// GlobalConfigPath returns the path of the global config file that LoadConfig
// reads: config.toml when it exists, otherwise the legacy config.json when
// that exists, otherwise the config.toml path (the file first run creates).
// For display in `af debug` and diagnostics, so they name the real file
// rather than a hardcoded one (#1030).
func GlobalConfigPath() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	tomlPath := filepath.Join(configDir, TomlConfigFileName)
	if fileExists(tomlPath) {
		return tomlPath, nil
	}
	jsonPath := filepath.Join(configDir, ConfigFileName)
	if fileExists(jsonPath) {
		return jsonPath, nil
	}
	return tomlPath, nil
}

// convertRenameFailForTest, when non-nil, replaces the config.json → .bak
// rename in convertJSONToTOML with the returned error. Tests use it to
// simulate a crash after the config.toml write but before the rename, and to
// assert the next load treats config.toml as canonical (both files present).
var convertRenameFailForTest func() error

// convertRaceHookForTest, when non-nil, runs at the top of the conversion
// file-lock body — the point at which a concurrent process that won the lock
// first would already have written config.toml (and renamed config.json).
// Tests use it to materialize that winner state and pin the lost-race
// behavior: this process must adopt the winner's config.toml, not re-convert.
var convertRaceHookForTest func()

// convertJSONToTOML performs the one-time json→toml migration (#1030): it
// parses and validates the legacy config.json with the frozen JSON reader,
// writes an equivalent config.toml atomically, then moves config.json aside to
// config.json.bak (or the first free config.json.bak.N — an existing backup is
// never overwritten) so config.toml becomes the single canonical file. The
// whole sequence runs under the config file lock so a CLI and the daemon
// racing the first post-upgrade load produce exactly one conversion; the lock
// body re-checks for a config.toml the winner already wrote and simply loads it.
//
// A config.json that fails to parse or validate is NOT converted and NOT
// renamed: the error is returned so the user can fix the file in place.
//
// The write-then-rename order makes a crash mid-conversion safe: a crash
// before the toml write changes nothing (config.json is still canonical next
// run); a crash after it leaves both files, and LoadConfig's canonical-toml
// rule resolves that to config.toml with a warning. The rename is therefore
// best-effort — a failure is logged, not fatal.
func convertJSONToTOML(configDir, configPath, tomlPath, prettyConfigPath, prettyTomlPath string) (*Config, error) {
	var result *Config
	lockErr := WithFileLock(tomlPath, func() error {
		if convertRaceHookForTest != nil {
			convertRaceHookForTest()
		}
		// The winner of a concurrent conversion already wrote config.toml.
		// (Our writes are atomic, so a non-empty config.toml here is complete.)
		if td, err := os.ReadFile(tomlPath); err == nil {
			if !isEffectivelyEmptyToml(td) {
				cfg, perr := parseConfigTOML(td, prettyTomlPath)
				if perr != nil {
					return perr
				}
				result = cfg
				return nil
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read config file %s: %w", prettyTomlPath, err)
		}

		// Re-read config.json under the lock: a racer may have renamed it to
		// .bak, or it may have changed since our pre-lock read.
		data, err := os.ReadFile(configPath)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to read config file %s: %w", prettyConfigPath, err)
		}
		if os.IsNotExist(err) || len(data) == 0 {
			// The racer renamed config.json to .bak (and its config.toml is
			// gone/incomplete), or the file is an empty first-run stub — either
			// way there is nothing to convert, so materialize fresh defaults.
			cfg, mErr := materializeDefaultConfig(configDir, tomlPath, prettyTomlPath)
			if mErr != nil {
				return mErr
			}
			result = cfg
			return nil
		}

		cfg, err := parseConfig(data, prettyConfigPath)
		if err != nil {
			return err // invalid config.json: do not convert or rename it.
		}

		tomlBytes, err := toml.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("failed to marshal config %s as TOML: %w", prettyConfigPath, err)
		}
		if err := AtomicWriteFile(tomlPath, tomlBytes, 0644); err != nil {
			return fmt.Errorf("failed to write %s during conversion: %w", prettyTomlPath, err)
		}

		// Never overwrite an existing backup: a downgrade can regenerate a
		// defaults config.json that a *second* conversion would otherwise
		// rename onto config.json.bak, clobbering the user's ORIGINAL
		// real-settings backup from the first conversion (Greptile on #1148).
		// Pick the first free config.json.bak / .bak.1 / .bak.2 … so the
		// oldest backup — the one most likely to hold real settings — is
		// preserved and each later conversion lands beside it.
		bakPath, bakErr := availableBackupPath(configPath + ".bak")
		renameErr := bakErr
		if renameErr == nil {
			renameErr = func() error {
				if convertRenameFailForTest != nil {
					return convertRenameFailForTest()
				}
				return os.Rename(configPath, bakPath)
			}()
		}
		if renameErr != nil {
			// config.toml is already written and will be canonical next load;
			// leaving config.json in place only costs a "both files" warning.
			log.WarningLog.Printf("migrated config to %s, but could not move the original %s aside: %v — %s is now canonical; delete %s yourself to silence the duplicate-config warning",
				prettyTomlPath, prettyConfigPath, renameErr, prettyTomlPath, prettyConfigPath)
		} else {
			log.InfoLog.Printf("migrated config to TOML: wrote %s and moved the original to %s — edit %s from now on",
				prettyTomlPath, prettyHomePath(bakPath), prettyTomlPath)
		}
		result = cfg
		return nil
	})
	if lockErr != nil {
		return nil, lockErr
	}
	return result, nil
}

// materializeRaceHookForTest, when non-nil, runs between LoadConfig observing
// no config file and the exclusive create below. Tests use it to recreate the
// file in that window and pin the lost-race behavior.
var materializeRaceHookForTest func()

// materializeDefaultConfig handles the no-config-file branch of LoadConfig,
// writing DefaultConfig() to config.toml (#1030). A missing config is only
// expected on first run; when the config dir visibly already carries state,
// the user's settings file was deleted out from under us, and regenerating
// defaults silently would disguise the loss as normal operation (#837) — so
// that case logs at ERROR level before materializing (the app still needs a
// config to run). The write itself is create-exclusive: if another process
// writes config.toml between our read and our create, that file wins and is
// returned instead of being clobbered.
func materializeDefaultConfig(configDir, tomlPath, prettyTomlPath string) (*Config, error) {
	if configDirInitialized(configDir) {
		log.ErrorLog.Printf("no config file (config.toml/config.json) in an initialized config dir (%s) — materializing defaults; previous settings are lost", prettyHomePath(configDir))
	}
	if materializeRaceHookForTest != nil {
		materializeRaceHookForTest()
	}

	defaultCfg := DefaultConfig()
	created, saveErr := writeConfigIfMissing(tomlPath, defaultCfg)
	if saveErr != nil {
		log.WarningLog.Printf("failed to save default config: %v", saveErr)
		return defaultCfg, nil
	}
	if !created {
		// Lost the create race: a concurrent process wrote config.toml after
		// our read. Treat its file as authoritative.
		if data, err := os.ReadFile(tomlPath); err == nil && !isEffectivelyEmptyToml(data) {
			return parseConfigTOML(data, prettyTomlPath)
		}
		// The concurrent file vanished or is empty; fall back to in-memory
		// defaults without another write attempt.
	}
	return defaultCfg, nil
}

// configDirInitialized reports whether configDir already carries application
// state — an instances/ or repos/ subdirectory, or a daemon.pid — meaning a
// missing config file there is a settings loss, not a first run.
func configDirInitialized(configDir string) bool {
	for _, marker := range []string{"instances", "repos"} {
		if fi, err := os.Stat(filepath.Join(configDir, marker)); err == nil && fi.IsDir() {
			return true
		}
	}
	_, err := os.Stat(filepath.Join(configDir, "daemon.pid"))
	return err == nil
}

// writeConfigForceFailForTest, when non-nil, replaces the f.Write call in
// writeConfigIfMissing with the returned error. Tests use it to exercise the
// "write failed after O_EXCL created the file" path and assert the zero-byte
// stub is cleaned up (#864) — a failure the real os.File.Write cannot be
// induced to produce deterministically.
var writeConfigForceFailForTest func() error

// writeConfigIfMissing persists config to configPath (a config.toml path,
// #1030) with O_CREATE|O_EXCL semantics. Returns created=false (and no error)
// when the file already exists, so a concurrently written config is never
// overwritten.
func writeConfigIfMissing(configPath string, config *Config) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return false, fmt.Errorf("failed to create config directory: %w", err)
	}
	data, err := toml.Marshal(config)
	if err != nil {
		return false, fmt.Errorf("failed to marshal config: %w", err)
	}

	f, err := os.OpenFile(configPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to create config file: %w", err)
	}
	writeErr := func() error {
		if writeConfigForceFailForTest != nil {
			return writeConfigForceFailForTest()
		}
		_, err := f.Write(data)
		return err
	}()
	if writeErr != nil {
		_ = f.Close()
		// Remove the freshly created stub so a failed write never leaves a
		// zero-byte (or partial) config.toml behind to wedge the next startup
		// (#864). Only the write path cleans up: a Close error means the bytes
		// already reached the file, so it may be a complete config worth
		// keeping rather than discarding.
		_ = os.Remove(configPath)
		return true, fmt.Errorf("failed to write config file: %w", writeErr)
	}
	if err := f.Close(); err != nil {
		return true, fmt.Errorf("failed to close config file: %w", err)
	}
	return true, nil
}
