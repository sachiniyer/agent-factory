// Package throwawaysandbox holds the marker and registry types behind the
// lifetime contract for throwaway sandboxes (#6844): play-test and audit AF
// homes that must never outlive their stated lifetime or their creator.
//
// A sandbox is identified by the explicit marker it was created with —
// throwaway-sandbox.json at the top of its AF home — never by a path prefix
// or a process name. The marker carries the deadline and, optionally, the
// creator's process identity, so a later daemon start can prove the owner is
// gone. Each marked daemon registers its home in a per-user directory under
// os.TempDir, so the next marked daemon start can find marked homes without
// guessing at path patterns. The daemon package owns enforcement; this
// package owns only the schema both sides share, and
// scripts/container/sandbox-lifetime.sh writes the same schema from the
// scaffolding that creates these homes.
package throwawaysandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// MarkerFileName is the file a throwaway sandbox's AF home is created with.
// It is the ONLY identity the contract recognizes: a home without a valid,
// readable marker is not a sandbox and is never touched by the reaper.
const MarkerFileName = "throwaway-sandbox.json"

const (
	// DefaultLifetimeSeconds is the shared six-hour default. It mirrors
	// playtest_lifetime in scripts/container/playtest-lifetime.sh and
	// doctor's defaultPlaytestMaxLifetime.
	DefaultLifetimeSeconds int64 = 21600
	// MaxLifetimeSeconds is the largest accepted lifetime — the positive
	// int32 range, matching the shell policy's GNU timeout bound.
	MaxLifetimeSeconds int64 = 2147483647

	markerVersion = 1
)

// Marker is one AF home's lifetime contract. Only Version, CreatedUnix and
// MaxLifetimeSeconds are mandatory; the owner identity and tmux fields are
// optional evidence the enforcing daemon uses when present.
type Marker struct {
	// Version is the schema version; anything other than 1 is invalid.
	Version int `json:"version"`
	// Kind names what created the sandbox ("playtest-standin",
	// "audit", …) for evidence and log lines.
	Kind string `json:"kind,omitempty"`
	// CreatedUnix is when the sandbox was marked, in seconds since epoch.
	// The deadline is measured from creation, not from any later daemon
	// start, so a sandbox cannot extend its life by restarting.
	CreatedUnix int64 `json:"created_unix"`
	// MaxLifetimeSeconds is the sandbox's stated lifetime in seconds.
	MaxLifetimeSeconds int64 `json:"max_lifetime_seconds"`
	// OwnerPID names the creating process instance when the scaffold
	// declared one (AF_SANDBOX_OWNER_PID). Zero means no owner was declared;
	// the deadline still applies.
	OwnerPID int `json:"owner_pid,omitempty"`
	// OwnerStartID is the owner's proctree StartID: the PID can only be
	// proven dead-or-recycled when this was recorded too. Zero means the
	// writer could not read it.
	OwnerStartID uint64 `json:"owner_start_id,omitempty"`
	// OwnerBootID scopes the owner identity to the boot that issued it.
	OwnerBootID string `json:"owner_boot_id,omitempty"`
	// OwnerNsID scopes the owner identity to the PID namespace that issued
	// it, written in proctree's PIDNamespaceID form.
	OwnerNsID string `json:"owner_ns_id,omitempty"`
	// TmuxTmpdir is the sandbox's private TMUX_TMPDIR when it has one. The
	// teardown kills tmux servers through the sockets under this directory.
	TmuxTmpdir string `json:"tmux_tmpdir,omitempty"`
	// SandboxRoot is the sandbox's top-level scratch directory. It is the
	// anchor for the tmux-dir proof: the reaper trusts the recorded tmux
	// dir only when the root carries the RootSentinelName file this marker
	// created it with.
	SandboxRoot string `json:"sandbox_root,omitempty"`
}

// MarkerPath returns where home's marker lives.
func MarkerPath(home string) string { return filepath.Join(home, MarkerFileName) }

// RootSentinelName is the file the scaffolding writes into a sandbox root
// when it creates the sandbox (#6844): positive proof — never inferred —
// that THIS sandbox instance made the tree. Its content is the marker's
// created_unix in decimal, binding the root's creation to the marker
// instance that claims it. A recorded sandbox_root without a sentinel is
// just a path: it proves nothing and the reaper refuses the tmux metadata
// entirely.
const RootSentinelName = ".af-throwaway-sandbox"

// WriteRootSentinel plants the creation sentinel inside a sandbox root the
// scaffolding is making. createdUnix must be the same value the marker
// records — that equality is the whole proof, so the scaffold writes both
// from one timestamp.
func WriteRootSentinel(root string, createdUnix int64) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, RootSentinelName),
		[]byte(strconv.FormatInt(createdUnix, 10)+"\n"), 0o600)
}

// TrustedOwnedFile reads path only when it is a regular, non-symlink file
// owned by this uid — the same anchor trustedFileOwner applies to markers
// and registry entries. Sandbox evidence a process could redirect (a
// sentinel, a recorded-pid file) is parsed only under that bar.
func TrustedOwnedFile(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := trustedFileOwner(fi); err != nil {
		return nil, fmt.Errorf("untrusted file %s: %w", path, err)
	}
	return os.ReadFile(path)
}

// PidsFileName is where a marked home lists the (pid, start id) instances
// the sandbox recorded at spawn time — membership evidence for the
// teardown's recorded-pid path, the only membership path that survives
// where socket evidence cannot be read.
const PidsFileName = "throwaway-pids.json"

// RecordedPID names one process instance the sandbox started. StartID is
// proctree's platform start stamp; it is compared for equality only.
type RecordedPID struct {
	PID     int    `json:"pid"`
	StartID uint64 `json:"start_id"`
}

type recordedPIDs struct {
	Version int           `json:"version"`
	PIDs    []RecordedPID `json:"pids"`
}

// RecordedPIDsPath is where home's recorded-pid list lives.
func RecordedPIDsPath(home string) string { return filepath.Join(home, PidsFileName) }

// ReadRecordedPIDs returns the process instances home has recorded. The
// file must pass the same trust bar as the marker — a recorded pid is a
// teardown target, so a list anyone else could have planted is not read.
// ErrNotExist means nothing was recorded; malformed or untrusted content
// is an error the caller must fail closed on.
func ReadRecordedPIDs(home string) ([]RecordedPID, error) {
	data, err := TrustedOwnedFile(RecordedPIDsPath(home))
	if err != nil {
		return nil, err
	}
	var r recordedPIDs
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing recorded pids: %w", err)
	}
	return r.PIDs, nil
}

// WriteRecordedPID merges (pid, startID) into home's recorded-pid list
// atomically. A list that exists but fails the trust/parse bar is refused
// rather than rewritten over — the reaper would have refused it anyway,
// and clobbering it would erase the evidence of the tampering.
func WriteRecordedPID(home string, pid int, startID uint64) error {
	existing, err := ReadRecordedPIDs(home)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, r := range existing {
		if r.PID == pid && r.StartID == startID {
			return nil
		}
	}
	data, err := json.Marshal(recordedPIDs{Version: markerVersion, PIDs: append(existing, RecordedPID{PID: pid, StartID: startID})})
	if err != nil {
		return err
	}
	path := RecordedPIDsPath(home)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Validate checks the mandatory contract fields. Unknown fields are ignored
// so older readers tolerate markers written by newer scaffolds.
func (m *Marker) Validate() error {
	if m.Version != markerVersion {
		return fmt.Errorf("unsupported marker version %d (want %d)", m.Version, markerVersion)
	}
	if m.CreatedUnix <= 0 {
		return errors.New("marker has no created_unix")
	}
	if m.MaxLifetimeSeconds < 1 || m.MaxLifetimeSeconds > MaxLifetimeSeconds {
		return fmt.Errorf("marker lifetime %d out of range 1..%d",
			m.MaxLifetimeSeconds, MaxLifetimeSeconds)
	}
	if m.OwnerPID < 0 || (m.OwnerPID == 0 && m.OwnerStartID != 0) {
		return errors.New("marker owner identity is inconsistent")
	}
	return nil
}

// Deadline is the instant the sandbox must not outlive.
func (m *Marker) Deadline() time.Time {
	return time.Unix(m.CreatedUnix, 0).Add(time.Duration(m.MaxLifetimeSeconds) * time.Second)
}

// Expired reports whether the sandbox is past its deadline at now.
func (m *Marker) Expired(now time.Time) bool { return !now.Before(m.Deadline()) }

// trustedFileOwner reports whether fi names a regular file owned by this
// uid and reachable without crossing a symlink. A marker or registry entry
// reachable only through a symlink — or owned by someone else — is not
// evidence this user planted: it is refused, never parsed.
func trustedFileOwner(fi os.FileInfo) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("path is a symlink")
	}
	if !fi.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("path is owned by uid %d", st.Uid)
	}
	return nil
}

// trustedRegistryDir proves the registry directory itself is the one this
// user created: a real directory (never a symlink), owned by this uid, and
// owner-only. Anything looser is refused — an attacker who can write the
// registry can aim a sweep at any home it names.
func trustedRegistryDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("registry dir %s is a symlink", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("registry path %s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("registry dir %s is owned by uid %d", dir, st.Uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("registry dir %s is not owner-only (mode %#o)", dir, fi.Mode().Perm())
	}
	return nil
}

// LoadMarker reads and validates home's marker. The error is os.ErrNotExist
// when the home carries no marker; any parse or validation failure is also
// returned as an error so callers can fail closed — an unreadable marker
// never authorizes anything. A marker reachable only through a symlink, or
// owned by a different uid, is refused the same way: the marker is the trust
// anchor, so it must be a file this user actually wrote.
func LoadMarker(home string) (*Marker, error) {
	path := MarkerPath(home)
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := trustedFileOwner(fi); err != nil {
		return nil, fmt.Errorf("untrusted marker %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &m, nil
}

// WriteMarker persists m into home, atomically: a reader sees either no
// marker or a complete one. The file is created mode 0600 — the owner
// identity inside is only meaningful to the user who owns the home.
func WriteMarker(home string, m *Marker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := MarkerPath(home)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// registryDirPrefix prefixes the per-user registry directory under
// os.TempDir. The uid suffix keeps one user's registry private to them —
// another user cannot list or write it, so it cannot redirect a sweep.
const registryDirPrefix = "af-throwaway-sandboxes-"

// RegistryDir is where marked daemons list the homes they serve. tmpdir is
// the process's os.TempDir value — the directory that /tmp-bound sandboxes
// already share.
func RegistryDir(tmpdir string, uid int) string {
	return filepath.Join(tmpdir, registryDirPrefix+strconv.Itoa(uid))
}

// registryEntry is what a registry file holds: the canonical AF home path.
// The file name is a hash of the home, so two entries cannot collide and a
// path with any spelling always maps to one file.
type registryEntry struct {
	Version int    `json:"version"`
	Home    string `json:"home"`
}

// RegistryEntryPath names the file holding home's registration. home must
// already be canonicalized by the caller; the hash makes the lookup
// spelling-independent.
func RegistryEntryPath(dir, home string) string {
	sum := sha256.Sum256([]byte(home))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
}

// WriteRegistryEntry records canonicalHome in the registry dir, creating the
// directory private (0700) on first use. Writes are atomic so a sweep never
// reads a half-written entry. A pre-existing dir that fails the trust check
// — symlink, wrong owner, or group/world access — is refused: writing into
// it would let whoever controls it redirect the next sweep.
func WriteRegistryEntry(dir, canonicalHome string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := trustedRegistryDir(dir); err != nil {
		return err
	}
	data, err := json.Marshal(registryEntry{Version: markerVersion, Home: canonicalHome})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".entry-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, RegistryEntryPath(dir, canonicalHome))
}

// ReadRegistryHomes lists the homes named by trusted registry entries.
// The directory must pass trustedRegistryDir — a registry anyone else could
// have planted is not read at all. Within it, each entry must be a regular
// file owned by this uid and not a symlink; untrusted or malformed entries
// are counted and skipped, not fatal: one bad entry must not hide the rest
// of the registry. A missing directory is not an error — it simply means
// nothing has registered.
func ReadRegistryHomes(dir string) (homes []string, malformed int, err error) {
	if err := trustedRegistryDir(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		fi, err := entry.Info()
		if err != nil || trustedFileOwner(fi) != nil {
			malformed++
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return homes, malformed, err
		}
		var e registryEntry
		if err := json.Unmarshal(data, &e); err != nil || e.Home == "" {
			malformed++
			continue
		}
		homes = append(homes, e.Home)
	}
	return homes, malformed, nil
}

// RemoveRegistryEntry drops canonicalHome's registration; a missing entry is
// success.
func RemoveRegistryEntry(dir, canonicalHome string) error {
	err := os.Remove(RegistryEntryPath(dir, canonicalHome))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
