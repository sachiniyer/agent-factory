package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// validateArchiveTitleLocked covers local, relocatable worktree name claims.
// The caller has already established that the candidate uses a local worktree.
// Branch names keep slashes, but archive directories fold them into dashes.
// ignore is the archived row the create is about to rename out of the way.
func (m *Manager) validateArchiveTitleLocked(repoID, title string, disk []session.InstanceData, ignore *session.Instance, inPlace bool) error {
	if inPlace {
		return nil
	}
	collision := func(existing string) error {
		return fmt.Errorf("session titled %q already maps to archive directory %q", existing, sanitizeArchiveTitle(title))
	}
	for key := range m.reservedArchiveTitles {
		rid, existing := splitDaemonInstanceKey(key)
		if rid == repoID && archiveTitlesCollide(existing, title) {
			return collision(existing)
		}
	}
	for key, inst := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil || inst == ignore || inst.Capabilities().Workspace != session.WorkspaceLocalWorktree || inst.IsExternalWorktree() {
			continue
		}
		if archiveRecordClaimsTitle(inst.ToInstanceData(), title) {
			return collision(inst.Title)
		}
	}
	for _, data := range disk {
		if !data.UsesLocalTmux() || data.Status == session.Loading || (ignore != nil && data.Title == ignore.Title) {
			continue
		}
		if !archiveRecordClaimsTitle(data, title) {
			continue
		}
		owned, err := ownsArchiveDirectory(data)
		if err != nil {
			return err
		}
		if owned {
			return collision(data.Title)
		}
	}
	return nil
}

// archiveRecordClaimsTitle reserves both the current archive location and the
// future destination after restore, which derives from the session's title.
func archiveRecordClaimsTitle(data session.InstanceData, title string) bool {
	key := archiveTitleKey(title)
	for _, name := range archiveRecordClaimNames(data) {
		if archiveDiskNameKey(name) == key {
			return true
		}
	}
	return false
}

// archiveRecordClaimNames returns literal destination spellings for both claims.
func archiveRecordClaimNames(data session.InstanceData) [2]string {
	return [2]string{archiveClaimName(data), sanitizeArchiveTitle(data.Title)}
}

// archiveClaimName preserves the directory owned by an archived row even when
// title reuse has renamed the row without moving its worktree.
func archiveClaimName(data session.InstanceData) string {
	if session.RecordedLiveness(data) == session.LiveArchived && data.Worktree.WorktreePath != "" {
		return filepath.Base(data.Worktree.WorktreePath)
	}
	return sanitizeArchiveTitle(data.Title)
}

// archiveTitlesCollide compares session titles in the portable archive namespace.
func archiveTitlesCollide(a, b string) bool {
	return archiveTitleKey(a) == archiveTitleKey(b)
}

// ownsArchiveDirectory decodes the same ownership projections as FromInstanceData.
// ForStorage sets ExternalWorktree even for af-owned trees to protect unresolved
// archives from older releases.
func ownsArchiveDirectory(data session.InstanceData) (bool, error) {
	if !data.UsesLocalTmux() || data.Status == session.Loading {
		return false, nil
	}
	decoded := data.RestoreArchiveRollbackFence()
	decoded, err := decoded.RestoreRelocationRecoveryOriginals()
	if err != nil {
		return false, fmt.Errorf("%w: cannot restore archive ownership for session %q: %v", errTitleCheckFatal, data.Title, err)
	}
	return !decoded.Worktree.ExternalWorktree, nil
}

// archiveTitleKey deliberately uses one portable comparison on every platform,
// including case-sensitive Linux filesystems. Keep sanitizeArchiveTitle as the
// on-disk spelling; only namespace admission folds case and Unicode composition.
func archiveTitleKey(title string) string {
	return archiveDiskNameKey(sanitizeArchiveTitle(title))
}

// archiveDiskNameKey compares literal disk spellings without changing punctuation.
// Sanitization belongs only to the title-to-disk direction.
func archiveDiskNameKey(name string) string {
	folded := cases.Fold().String(norm.NFC.String(name))
	// Folding can decompose NFC input, so normalize the result as well.
	return norm.NFC.String(folded)
}

// archiveDestinationKey uses the same portable directory comparison as create
// admission. Legacy sessions can still have case/normalization-equivalent names.
func archiveDestinationKey(repoID, dest string) string {
	return daemonInstanceKey(repoID, filepath.Join(filepath.Dir(dest), archiveDiskNameKey(filepath.Base(dest))))
}

// releaseArchiveDestination cannot release a different instance's reservation.
// The archive caller defers it only after successful admission; failed probes
// release their own claim inside checkArchiveDestination.
func (m *Manager) releaseArchiveDestination(repoID string, inst *session.Instance, dest string) {
	key := archiveDestinationKey(repoID, dest)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reservedArchiveDestinations[key] == inst {
		delete(m.reservedArchiveDestinations, key)
	}
}

// checkArchiveDestination claims the destination before any filesystem probe or
// teardown and returns the pathname to pass to the move. The caller must defer
// releaseArchiveDestination with the original dest on success.
func (m *Manager) checkArchiveDestination(repoID string, inst *session.Instance, dest, source string) (moveDest string, err error) {
	if dest == "" {
		return "", fmt.Errorf("cannot archive session %q: archive destination is empty", inst.Title)
	}
	key := archiveDestinationKey(repoID, dest)
	m.mu.Lock()
	if owner := m.reservedArchiveDestinations[key]; owner != nil {
		ownerTitle := owner.Title
		m.mu.Unlock()
		return "", fmt.Errorf("cannot archive session %q: destination %s is being claimed by session %q", inst.Title, dest, ownerTitle)
	}
	if m.reservedArchiveDestinations == nil {
		m.reservedArchiveDestinations = make(map[string]*session.Instance)
	}
	m.reservedArchiveDestinations[key] = inst
	m.mu.Unlock()
	defer func() {
		if err != nil {
			m.releaseArchiveDestination(repoID, inst, dest)
		}
	}()
	return m.inspectArchiveDestination(repoID, inst, dest, source)
}

// inspectArchiveDestination runs before editors, hooks, or tabs are stopped.
// A retry already at its destination keeps the identity-checked source spelling
// so the git layer takes its repair path instead of attempting a no-replace move
// between two names for the same directory.
func (m *Manager) inspectArchiveDestination(repoID string, inst *session.Instance, dest, source string) (string, error) {
	if dest == source {
		return source, nil
	}
	destInfo, err := sessiongit.BoundedLstat(dest)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return "", fmt.Errorf("cannot archive session %q: cannot inspect destination %s: %w", inst.Title, dest, err)
	}
	// Lstat follows parent-directory aliases without accepting a symlink that
	// occupies the final destination entry. Both probes retain their deadlines.
	if !missing && destInfo.IsDir() {
		sourceInfo, statErr := sessiongit.BoundedLstat(source)
		if statErr == nil && sourceInfo.IsDir() && os.SameFile(destInfo, sourceInfo) {
			return source, nil
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("cannot archive session %q: cannot inspect source %s: %w", inst.Title, source, statErr)
		}
	}
	// Case-insensitive filesystems may resolve a different portable spelling
	// during Lstat. Scan after identity retries, regardless of probe outcome,
	// before falling back to exact-path owner diagnostics.
	if err := inspectArchivePortableNamespace(repoID, inst, dest, !missing); err != nil {
		return "", err
	}
	if missing {
		return dest, nil
	}
	owner, err := m.archiveDestinationOwner(repoID, inst, dest, destInfo)
	if err != nil {
		return "", fmt.Errorf("cannot archive session %q: destination %s already exists; cannot determine its owner: %w", inst.Title, dest, err)
	}
	if owner != "" {
		return "", fmt.Errorf("cannot archive session %q: destination %s already exists and belongs to session %q", inst.Title, dest, owner)
	}
	return "", fmt.Errorf("cannot archive session %q: destination %s already exists; no existing session owns it", inst.Title, dest)
}

// archiveLeafNameMax is the Linux per-component filesystem limit (NAME_MAX). The
// archive leaf is a single path segment under <AF_HOME>/archived/<repoID>/, so it
// must stay within it or the directory create / move fails with "file name too
// long" — the same class #2528 bounded for the worktree/branch/slug paths.
const archiveLeafNameMax = 255

// archiveLeafDigestLen is the number of hex characters appended as a uniqueness
// digest when sanitizeArchiveTitle must truncate a long title. 16 hex chars
// encode 8 bytes of SHA-256, matching the pattern used in session/git/hooks.go
// and session/tmux/session.go. The separator ("-") plus the digest consume 17
// bytes of the archiveLeafNameMax budget.
const archiveLeafDigestLen = 16

// sanitizeArchiveTitle makes a session title safe as a single path segment,
// mirroring NewGitWorktree's safeSessionName handling (strip "..", "/"→"-",
// trim leading separators), falling back to "session" when nothing remains.
//
// When the sanitized form is strictly shorter than archiveLeafNameMax, it is
// returned as-is (the "direct" namespace). When the sanitized form is
// archiveLeafNameMax or longer, the leaf becomes <prefix>-<digest> where the
// digest is a short SHA-256 of the full sanitized form (the "digest"
// namespace). Using a strict boundary keeps the two namespaces disjoint: a
// direct leaf is always < archiveLeafNameMax bytes, and a digest leaf is always
// exactly archiveLeafNameMax bytes. Without this separation, the digest output
// for a long title L could equal the direct output for a short title S —
// sanitizeArchiveTitle(L) == S == sanitizeArchiveTitle(S) — producing a silent
// collision without any prefix or digest coincidence.
//
// This guarantees four properties simultaneously:
//
//   - Non-empty: the "session" fallback and the digest alone path ensure a
//     non-empty result for any input, including all-continuation-byte titles.
//   - Within NAME_MAX: the prefix is trimmed to leave room for "-" + digest.
//   - Injective: two distinct sanitized titles derive distinct digests, so two
//     long titles sharing the same prefix (e.g. "foo (archived 2)" vs "foo
//     (archived 3)") produce different leaves and can both be archived.
//   - Suffix-stable: the ` (archived N)` suffix appended by
//     uniqueArchivedTitleLocked is captured in the digest rather than being
//     truncated away, so the suffix walk converges quickly even for a 300-byte
//     base title.
func sanitizeArchiveTitle(title string) string {
	s := strings.ReplaceAll(title, "..", "")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.TrimLeft(s, "-.")
	if s == "" {
		s = "session"
	}
	if len(s) < archiveLeafNameMax {
		return s
	}
	// Title exceeds NAME_MAX. Produce <prefix>-<digest> where digest is a
	// short SHA-256 of the full sanitized form. The digest makes the leaf
	// injective across titles that share a long prefix, and captures any
	// disambiguating suffix (e.g. " (archived N)") that prefix-cutting would
	// remove. This mirrors the approach at session/git/hooks.go:399 and
	// session/tmux/session.go:299.
	sum := sha256.Sum256([]byte(s))
	digest := hex.EncodeToString(sum[:archiveLeafDigestLen/2])
	// Budget: NAME_MAX minus "-" separator minus digest length.
	budget := archiveLeafNameMax - 1 - len(digest)
	if budget < 0 {
		budget = 0
	}
	prefix := s
	if len(prefix) > budget {
		cut := budget
		for cut > 0 && !utf8.RuneStart(prefix[cut]) {
			cut--
		}
		prefix = prefix[:cut]
	}
	prefix = strings.TrimLeft(prefix, "-.")
	if prefix == "" {
		// All bytes were continuation bytes or separators; use the digest alone.
		// It is well within NAME_MAX and uniquely identifies the original title.
		return digest
	}
	return prefix + "-" + digest
}
