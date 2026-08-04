package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// The directory read behind the web Add-project picker (#2788).
//
// A browser cannot see the daemon host's filesystem, so "type an absolute path"
// was the only Add-project affordance the web could offer — unusable from a
// phone, where there is no tab-completion and a typo is only discovered after
// the round trip. This is the read that lets the web SHOW what exists instead.
//
// SCOPE, stated plainly because this is a new reachable surface: it exposes
// directory names on the daemon host to any client the daemon already
// authenticates. That is not a new capability — af runs agents with full
// filesystem access on that same host, and RegisterProject already resolves
// arbitrary paths there — but it is newly *reachable*, so it deliberately
// invents no auth rule of its own. It is an ordinary entry in httpRoutes and
// therefore sits behind exactly the same bearer-token gate as every other route
// (see daemon/httpauth.go); there is no unauthenticated path to it, and there is
// no confinement root, because confining the browse tree while `af projects add`
// accepts any path would be theatre rather than a boundary.
//
// It reads only NAMES and directory-ness. No file contents, no sizes, no
// ownership — the minimum a picker needs to navigate and to mark which entries
// can actually become a project.

// maxDirectoryEntries caps one listing. A home directory holding tens of
// thousands of children must not become a tens-of-megabytes response on a phone.
// The cap is REPORTED (ListDirectoryResponse.Truncated), never silent: a
// truncated listing that looked complete would be the same confident wrong
// answer as an empty one.
const maxDirectoryEntries = 500

// ListDirectoryRequest asks the daemon to list the DIRECTORIES inside one
// directory on ITS filesystem.
//
// Path names a directory on the DAEMON host — absolute, or "~"-prefixed, which
// the daemon expands against its own home. An empty Path means "start
// somewhere sensible", which is that home. A relative path is REFUSED rather
// than resolved: the daemon has no access to the caller's working directory
// (and under systemd its own cwd is /), so resolving one would silently list a
// different tree than the caller meant — the same reasoning
// RegisterProjectRequest.Path carries.
type ListDirectoryRequest struct {
	Path string `json:"path"`
}

// ListDirectoryResponse is one directory's navigable children.
//
// Path is the CANONICAL path that was actually listed (symlinks resolved, "."
// and ".." applied), not the spelling that was requested — so a client that
// echoes it in a header is showing where the user really is, and a client that
// sends it back round-trips to the same directory.
//
// Parent is the canonical parent, computed here with filepath.Dir so no client
// ever has to do ".." string surgery on a path it cannot resolve. It is empty at
// the filesystem root, where an up affordance would loop.
//
// IsRepo reports whether the LISTED directory is itself a git checkout, so a
// user who has navigated into their repo can select it without backing out.
type ListDirectoryResponse struct {
	Path      string           `json:"path"`
	Parent    string           `json:"parent"`
	Home      string           `json:"home"`
	IsRepo    bool             `json:"is_repo"`
	Entries   []DirectoryEntry `json:"entries"`
	Truncated bool             `json:"truncated"`
}

// DirectoryEntry is one navigable child directory.
//
// Path is that child's CANONICAL path. For a symlinked entry that is the
// TARGET's path, not the link's — so descending through a link lands the client
// (and the header it draws) on where it actually is, rather than on a spelling
// that resolves elsewhere on the next request. IsSymlink is set alongside it so
// the redirection is visible rather than silent.
//
// IsRepo marks the entries that can actually become a project. Everything else
// stays navigable but is not a target.
type DirectoryEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	IsRepo    bool   `json:"is_repo"`
	IsSymlink bool   `json:"is_symlink"`
}

// ListDirectory answers the picker's read. Like ListProjects it takes no
// admission gate: it touches neither the manager nor any daemon-owned state, so
// there is nothing for a warm-up or upgrade probation to protect, and a client
// building its Add-project view must not have to wait on the session restore.
func (s *controlServer) ListDirectory(req ListDirectoryRequest, resp *ListDirectoryResponse) error {
	listing, err := listDirectory(req.Path)
	if err != nil {
		return err
	}
	*resp = listing
	return nil
}

// listDirectory resolves and lists one directory, or fails.
//
// It NEVER answers "success, no entries" for a directory it could not read.
// os.ReadDir hands back partial entries ALONGSIDE its error, so the natural
// misuse — take the entries, log the error — produces a 200 with an empty list
// that the UI renders as fact: "your repos are not in here". That is this
// repo's fabricated-negative shape, and it is the reason every failure below
// returns an error with the offending path in the message and a zero response.
//
// (An error TYPE would buy nothing over the wire: the HTTP/RPC boundary flattens
// sentinels to text (#2512), so what a client can actually act on is a non-200
// with an actionable message. The typing that matters is the wire-level one —
// error, not empty success.)
func listDirectory(requested string) (ListDirectoryResponse, error) {
	// Best-effort: a daemon whose home is unresolvable can still browse absolute
	// paths, so this only costs the empty-path default and the Home shortcut.
	//
	// Canonicalized through the same resolver as everything else, so a client
	// comparing Home against the listed Path (to hide a Home shortcut that would
	// be a no-op) compares two spellings of the same canon. A raw $HOME under a
	// symlinked home directory would differ from the resolved Path on every
	// listing — the #2110 spelling trap, one layer up.
	home, homeErr := os.UserHomeDir()
	if homeErr == nil {
		home = pathutil.ResolveForCompare(home)
	}

	raw := strings.TrimSpace(requested)
	if raw == "" {
		if homeErr != nil {
			return ListDirectoryResponse{}, fmt.Errorf(
				"cannot pick a starting directory: the daemon host's home is unresolvable (%w) — pass an absolute path", homeErr)
		}
		raw = home
	}

	expanded := config.ExpandTilde(raw)
	if !filepath.IsAbs(expanded) {
		return ListDirectoryResponse{}, fmt.Errorf(
			"directory path %q must be absolute (or start with ~/): the daemon resolves it on its own filesystem and has no access to your working directory", requested)
	}

	// pathutil.ResolveForCompare, not hand-rolled cleaning: it resolves the
	// deepest existing ancestor through EvalSymlinks and re-joins the rest, which
	// is the missing-leaf/symlink canon that has bitten path identity twice
	// (#2110, #2551). Everything downstream — the parent, each entry's path, the
	// header the client draws — is derived from THIS value, so a client never
	// manipulates a path component itself.
	dir := pathutil.ResolveForCompare(expanded)

	info, err := os.Stat(dir)
	if err != nil {
		return ListDirectoryResponse{}, directoryError(dir, err)
	}
	if !info.IsDir() {
		return ListDirectoryResponse{}, fmt.Errorf("%s is not a directory", dir)
	}

	children, err := os.ReadDir(dir)
	if err != nil {
		return ListDirectoryResponse{}, directoryError(dir, err)
	}

	entries := make([]DirectoryEntry, 0, len(children))
	for _, child := range children {
		entry, ok := directoryEntry(dir, child)
		if !ok {
			continue
		}
		entries = append(entries, entry)
	}
	sortDirectoryEntries(entries)

	truncated := false
	if len(entries) > maxDirectoryEntries {
		entries = entries[:maxDirectoryEntries]
		truncated = true
	}

	// The .git probe runs only on the entries that SURVIVE the cap. It is one
	// syscall per row, so doing it during the scan above would cost a stat per
	// child of a 50k-entry home directory to decorate at most 500 of them.
	for i := range entries {
		entries[i].IsRepo = isGitCheckout(entries[i].Path)
	}

	// filepath.Dir of a canonical absolute path: "/" is its own parent, which is
	// the one place an up affordance must disappear rather than loop.
	parent := filepath.Dir(dir)
	if parent == dir {
		parent = ""
	}
	// Defensive: os.UserHomeDir returns "" alongside its error today, but a Home
	// the daemon could not resolve must never reach a client as a real path.
	if homeErr != nil {
		home = ""
	}

	return ListDirectoryResponse{
		Path:      dir,
		Parent:    parent,
		Home:      home,
		IsRepo:    isGitCheckout(dir),
		Entries:   entries,
		Truncated: truncated,
	}, nil
}

// directoryEntry builds one row, or reports that this child is not a navigable
// directory (a file, a dangling symlink, a symlink to a file).
func directoryEntry(dir string, child fs.DirEntry) (DirectoryEntry, bool) {
	name := child.Name()
	path := filepath.Join(dir, name)
	symlink := child.Type()&fs.ModeSymlink != 0

	isDir := child.IsDir()
	if symlink {
		// The dirent type of a symlink describes the LINK, so its target's
		// directory-ness needs a Stat (which follows). A target that is missing or
		// unreadable simply is not navigable — dropping it here is not a hidden
		// failure, because the row would lead nowhere if kept.
		target, err := os.Stat(path)
		if err != nil {
			return DirectoryEntry{}, false
		}
		isDir = target.IsDir()
		path = pathutil.ResolveForCompare(path)
	}
	if !isDir {
		return DirectoryEntry{}, false
	}
	// IsRepo is deliberately left unset here; listDirectory fills it after the
	// cap, so the probe cost is bounded by what is actually returned.
	return DirectoryEntry{Name: name, Path: path, IsSymlink: symlink}, true
}

// sortDirectoryEntries orders a listing the way someone looking for a repo reads
// it: the ordinary directories first, case-insensitively, with dot-directories
// after them. Hidden entries are SORTED down, never filtered out — a picker that
// silently omits rows is the same lie as one that reports an unreadable
// directory as empty.
func sortDirectoryEntries(entries []DirectoryEntry) {
	sort.Slice(entries, func(i, j int) bool {
		hiddenI, hiddenJ := strings.HasPrefix(entries[i].Name, "."), strings.HasPrefix(entries[j].Name, ".")
		if hiddenI != hiddenJ {
			return hiddenJ
		}
		lowerI, lowerJ := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if lowerI != lowerJ {
			return lowerI < lowerJ
		}
		return entries[i].Name < entries[j].Name
	})
}

// isGitCheckout reports whether dir holds a .git entry — a directory in a main
// checkout, a FILE in a linked worktree, and both count. It is the cheap marker,
// not the authority: config.RegisterProject still resolves and validates the
// path the user submits, so a false negative here costs a missing badge rather
// than an unregistrable repo (the free-text path field remains the escape
// hatch). Lstat, so a symlinked .git counts without following it.
func isGitCheckout(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// directoryError turns a filesystem failure into a message that names the path
// and the reason. "permission denied" and "no such directory" are distinct on
// purpose: they are the two answers a user acts on differently, and neither may
// be flattened into an empty listing.
func directoryError(dir string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("no such directory: %s", dir)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("cannot read %s: permission denied", dir)
	default:
		return fmt.Errorf("cannot read %s: %w", dir, err)
	}
}
