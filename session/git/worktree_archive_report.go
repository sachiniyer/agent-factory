package git

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// ArchiveSkipReason is the stable, JSON-encoded explanation for a manifest
// entry that an archive deliberately did not copy.
type ArchiveSkipReason string

const (
	// ArchiveSkipPermissionDenied means af could inspect the regular file but
	// the operating system refused its read open. No elevation is attempted.
	ArchiveSkipPermissionDenied ArchiveSkipReason = "permission_denied"

	maxArchiveWarningEntries = 20
	maxArchiveWarningTrees   = 4
)

// ArchiveSkippedEntry describes one source name that is known to be absent from
// an incomplete archive. Path is relative to its retained worktree root.
// PathBytes is populated only when Path is not valid UTF-8; encoding/json cannot
// preserve such a string, so the base64-encoded raw bytes are the durable value.
type ArchiveSkippedEntry struct {
	Path      string            `json:"path"`
	PathBytes []byte            `json:"path_bytes,omitempty"`
	Reason    ArchiveSkipReason `json:"reason"`
}

func newArchiveSkippedEntry(path string, reason ArchiveSkipReason) ArchiveSkippedEntry {
	display, raw := archivePathFields(path)
	return ArchiveSkippedEntry{Path: display, PathBytes: raw, Reason: reason}
}

func (entry ArchiveSkippedEntry) filesystemPath() string {
	return archiveFilesystemPath(entry.Path, entry.PathBytes)
}

func (entry ArchiveSkippedEntry) clone() ArchiveSkippedEntry {
	if len(entry.PathBytes) == 0 {
		entry.Path, entry.PathBytes = archivePathFields(entry.Path)
	}
	entry.PathBytes = append([]byte(nil), entry.PathBytes...)
	return entry
}

func (entry ArchiveSkippedEntry) MarshalJSON() ([]byte, error) {
	type wireEntry ArchiveSkippedEntry
	normalized := entry.clone()
	return json.Marshal(wireEntry(normalized))
}

// ArchiveRetainedTree owns one complete source tree kept because the published
// archive omitted the listed entries. The root identity makes later destructive
// kill cleanup fail closed if that private pathname is replaced.
type ArchiveRetainedTree struct {
	Path          string                `json:"path"`
	PathBytes     []byte                `json:"path_bytes,omitempty"`
	IdentityKnown bool                  `json:"identity_known"`
	Device        uint64                `json:"device"`
	Inode         uint64                `json:"inode"`
	FileType      uint32                `json:"file_type"`
	Skipped       []ArchiveSkippedEntry `json:"skipped"`
}

func newArchiveRetainedTree(path string, identity pathIdentity, skipped []ArchiveSkippedEntry) ArchiveRetainedTree {
	display, raw := archivePathFields(path)
	return ArchiveRetainedTree{
		Path: display, PathBytes: raw, IdentityKnown: true,
		Device: identity.device, Inode: identity.inode, FileType: identity.fileType,
		Skipped: cloneArchiveSkippedEntries(skipped),
	}
}

func (tree ArchiveRetainedTree) filesystemPath() string {
	return archiveFilesystemPath(tree.Path, tree.PathBytes)
}

func (tree ArchiveRetainedTree) identity() pathIdentity {
	return pathIdentity{device: tree.Device, inode: tree.Inode, fileType: tree.FileType}
}

func (tree ArchiveRetainedTree) clone() ArchiveRetainedTree {
	if len(tree.PathBytes) == 0 {
		tree.Path, tree.PathBytes = archivePathFields(tree.Path)
	}
	tree.PathBytes = append([]byte(nil), tree.PathBytes...)
	tree.Skipped = cloneArchiveSkippedEntries(tree.Skipped)
	return tree
}

func (tree ArchiveRetainedTree) MarshalJSON() ([]byte, error) {
	type wireTree ArchiveRetainedTree
	normalized := tree.clone()
	return json.Marshal(wireTree(normalized))
}

func archivePathFields(path string) (string, []byte) {
	if utf8.ValidString(path) {
		return path, nil
	}
	return strings.ToValidUTF8(path, "�"), []byte(path)
}

func archiveFilesystemPath(display string, raw []byte) string {
	if len(raw) > 0 {
		return string(raw)
	}
	return display
}

func cloneArchiveSkippedEntries(entries []ArchiveSkippedEntry) []ArchiveSkippedEntry {
	clone := make([]ArchiveSkippedEntry, len(entries))
	for index := range entries {
		clone[index] = entries[index].clone()
	}
	return clone
}

// ArchiveReport is durable metadata for every archive source retained because
// files could not be read. A restored incomplete session can be archived again;
// keeping one tree per copy preserves the old omissions as well as any new ones.
type ArchiveReport struct {
	RetainedTrees []ArchiveRetainedTree `json:"retained_trees"`
}

// Empty reports whether every archive copy was complete.
func (report ArchiveReport) Empty() bool {
	return len(report.RetainedTrees) == 0
}

// Clone prevents a live relocation from sharing report slices with a concurrent
// session snapshot.
func (report ArchiveReport) Clone() ArchiveReport {
	clone := ArchiveReport{RetainedTrees: make([]ArchiveRetainedTree, len(report.RetainedTrees))}
	for index := range report.RetainedTrees {
		clone.RetainedTrees[index] = report.RetainedTrees[index].clone()
	}
	return clone
}

func (report ArchiveReport) append(newReport ArchiveReport) ArchiveReport {
	combined := report.Clone()
	for _, tree := range newReport.RetainedTrees {
		combined.RetainedTrees = append(combined.RetainedTrees, tree.clone())
	}
	return combined
}

type archiveWarningEntry struct {
	path   string
	reason ArchiveSkipReason
}

// Warning renders a bounded summary for archive or restore results. The full
// lossless list stays in archive_report; transport responses name only a prefix
// so a huge unreadable generated tree cannot turn a committed reply into a
// multi-megabyte timeout.
func (report ArchiveReport) Warning(operation string) string {
	if report.Empty() {
		return ""
	}
	entries := make([]archiveWarningEntry, 0, maxArchiveWarningEntries)
	trees := make([]string, 0, maxArchiveWarningTrees)
	totalEntries := 0
	for _, tree := range report.RetainedTrees {
		trees = insertBoundedArchivePath(trees, tree.filesystemPath(), maxArchiveWarningTrees)
		for _, entry := range tree.Skipped {
			if totalEntries < int(^uint(0)>>1) {
				totalEntries++
			}
			entries = insertBoundedArchiveWarningEntry(entries, archiveWarningEntry{
				path: entry.filesystemPath(), reason: entry.Reason,
			}, maxArchiveWarningEntries)
		}
	}
	details := make([]string, 0, len(entries))
	for _, entry := range entries {
		details = append(details, fmt.Sprintf("%q (%s)", entry.path, skipReasonText(entry.reason)))
	}
	var treeDetails []string
	for _, tree := range trees {
		treeDetails = append(treeDetails, fmt.Sprintf("%q", tree))
	}
	if omitted := len(report.RetainedTrees) - len(trees); omitted > 0 {
		treeDetails = append(treeDetails, fmt.Sprintf("and %d more in archive_report", omitted))
	}

	noun := "files"
	if totalEntries == 1 {
		noun = "file"
	}
	pathLabel := "skipped paths"
	if len(entries) < totalEntries {
		pathLabel = fmt.Sprintf("skipped paths (showing first %d of %d)", len(entries), totalEntries)
	}
	return fmt.Sprintf(
		"%s completed with an incomplete archive: af skipped %d unreadable %s; "+
			"complete original tree(s) were retained at %s; %s: %s",
		operation, totalEntries, noun, strings.Join(treeDetails, ", "), pathLabel, strings.Join(details, ", "),
	)
}

func insertBoundedArchiveWarningEntry(
	entries []archiveWarningEntry, entry archiveWarningEntry, limit int,
) []archiveWarningEntry {
	position := sort.Search(len(entries), func(index int) bool {
		return entries[index].path >= entry.path
	})
	if position >= limit {
		return entries
	}
	if len(entries) < limit {
		entries = append(entries, archiveWarningEntry{})
	}
	copy(entries[position+1:], entries[position:])
	entries[position] = entry
	return entries
}

func insertBoundedArchivePath(paths []string, path string, limit int) []string {
	position := sort.Search(len(paths), func(index int) bool {
		return paths[index] >= path
	})
	if position >= limit {
		return paths
	}
	if len(paths) < limit {
		paths = append(paths, "")
	}
	copy(paths[position+1:], paths[position:])
	paths[position] = path
	return paths
}

func skipReasonText(reason ArchiveSkipReason) string {
	if reason == ArchiveSkipPermissionDenied {
		return "permission denied"
	}
	return string(reason)
}
