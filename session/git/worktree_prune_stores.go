package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxAlternatesDepth = 16
	maxSymlinkHops     = 40
)

// objectStoresInsideDeletedRoots reports every way the kept branch's repository
// reaches an object store THROUGH a deletion root (the worktree tree or the
// registration leaf), so that deleting the root would leave the repository unable
// to read its objects. The dependency is stated as a path to be walked, not a
// destination to be compared:
//
//   - the stores are the repository's objects directory plus its whole alternates
//     chain, taken from TWO sources whose union is checked: git's own listing
//     (`count-objects -v`: relative, quoted and transitive entries as git resolved
//     them), and the alternates files read as WRITTEN (relative entries resolved
//     against the store that lists them, C-quoted entries strictly decoded), because
//     git's listing no longer shows which symlinks the path traversed;
//   - each store path is walked component by component, physically (each ".." is
//     applied to the resolved prefix, each symlink is read and followed, bounded),
//     and EVERY prefix and every hop is checked against both roots — a symlink
//     inside a root that points to a store outside it is refused, because deleting
//     the root deletes the link.
//
// Anything unreadable, undecodable or unresolvable refuses (an error); a path that
// simply does not exist is walked lexically (git ignores such an alternate).
func objectStoresInsideDeletedRoots(worktreePath string) ([]string, error) {
	leaf, err := registrationLeaf(worktreePath)
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, p := range []string{worktreePath, leaf} {
		real, err := boundedEvalSymlinks(p)
		if err != nil {
			return nil, fmt.Errorf("could not resolve %s: %w", p, err)
		}
		roots = append(roots, real)
	}
	out, err := runBoundedWorktreeGit(worktreePath, false, "--no-optional-locks", "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return nil, fmt.Errorf("could not locate the object store of %s: %w", worktreePath, err)
	}
	objects := strings.TrimSpace(string(out))

	// ONE helper, called for EVERY path read while computing the dependency set —
	// each objects directory, each objects/info/alternates file, each listed entry,
	// at every level of the chain: walk it component by component and record the
	// first prefix or hop that lies inside a deletion root. It returns the resolved
	// location to read from.
	seen := map[string]bool{}
	var inside []string
	lookup := func(path string) (string, error) {
		resolved, hit, err := walkPhysical(path, roots)
		if err != nil {
			return "", err
		}
		if hit != "" && !seen[path] {
			seen[path] = true
			inside = append(inside, fmt.Sprintf("%s, through %s", path, hit))
		}
		return resolved, nil
	}

	// Source 1: git's own listing.
	stores := []string{objects}
	listing, err := runBoundedWorktreeGit(worktreePath, true, "--no-optional-locks", "-c", "core.quotePath=false", "count-objects", "-v")
	if err != nil {
		return nil, fmt.Errorf("could not list the object stores of %s: %w", worktreePath, err)
	}
	listed, err := parseCountObjectsAlternates(string(listing))
	if err != nil {
		return nil, fmt.Errorf("cannot decode an alternate object store of %s: %w", worktreePath, err)
	}
	stores = append(stores, listed...)

	// Source 2: the alternates files as written.
	visited := map[string]bool{}
	var follow func(store string, depth int) error
	follow = func(store string, depth int) error {
		if depth > maxAlternatesDepth {
			return fmt.Errorf("the alternates chain of %s is deeper than %d stores", worktreePath, maxAlternatesDepth)
		}
		real, err := lookup(store)
		if err != nil {
			return err
		}
		if visited[real] {
			return nil
		}
		visited[real] = true
		// The alternates FILE is a path read like any other: it can be reached
		// through a symlink inside a root even when the store it sits in is not.
		file, err := lookup(store + string(filepath.Separator) + filepath.Join("info", "alternates"))
		if err != nil {
			return err
		}
		raw, err := boundedReadFile(file)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("could not read the alternates of %s: %w", real, err)
		}
		entries, err := parseAlternatesFile(string(raw))
		if err != nil {
			return fmt.Errorf("cannot decode the alternates of %s: %w", real, err)
		}
		for _, entry := range entries {
			// A relative entry is judged under every reading of it, against the
			// CANONICAL form of the store that lists it (a temp root or home reached
			// through a symlink — /var -> /private/var on macOS — makes the as-written
			// and canonical spellings differ): ".." applied physically to the as-written
			// store, physically to the canonical store, and lexically to the canonical
			// store (git normalizes the joined path lexically).
			candidates := []string{entry}
			if !filepath.IsAbs(entry) {
				sep := string(filepath.Separator)
				candidates = []string{
					store + sep + entry,
					real + sep + entry,
					filepath.Join(real, entry),
				}
			}
			for _, cand := range candidates {
				stores = append(stores, cand)
				if err := follow(cand, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := follow(objects, 0); err != nil {
		return nil, err
	}

	for _, store := range stores {
		if _, err := lookup(store); err != nil {
			return nil, err
		}
	}
	return inside, nil
}

// walkPhysical walks path component by component from the filesystem root. The
// prefix it carries is always PHYSICAL (no symlinks), so each ".." is applied to a
// real directory and each entry's location is a real location. Every entry reached
// — including each symlink itself and every directory the walk passes through — is
// checked against roots; the first one inside a root is returned as hit. Symlinks
// are read (bounded) and followed, up to maxSymlinkHops. It returns the resolved
// destination. A component that does not exist ends the symlink-following: the
// rest is appended lexically.
func walkPhysical(path string, roots []string) (resolved, hit string, err error) {
	if !filepath.IsAbs(path) {
		return "", "", fmt.Errorf("object store path %q is not absolute", path)
	}
	pending := splitPath(path)
	resolved = string(filepath.Separator)
	hops := 0
	missing := false
	for len(pending) > 0 {
		elem := pending[0]
		pending = pending[1:]
		switch elem {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		cand := filepath.Join(resolved, elem)
		for _, r := range roots {
			if hit == "" && within(cand, r) {
				hit = cand
			}
		}
		if missing {
			resolved = cand
			continue
		}
		info, lerr := BoundedLstat(cand)
		switch {
		case lerr == nil:
		case os.IsNotExist(lerr):
			missing = true
			resolved = cand
			continue
		default:
			return "", "", fmt.Errorf("could not inspect %s: %w", cand, lerr)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = cand
			continue
		}
		hops++
		if hops > maxSymlinkHops {
			return "", "", fmt.Errorf("too many symlinks resolving %s", path)
		}
		target, rerr := BoundedFSOp("reading link "+cand, func() (string, error) { return os.Readlink(cand) })
		if rerr != nil {
			return "", "", fmt.Errorf("could not read the symlink %s: %w", cand, rerr)
		}
		if filepath.IsAbs(target) {
			resolved = string(filepath.Separator)
		}
		pending = append(splitPath(target), pending...)
	}
	for _, r := range roots {
		if hit == "" && within(resolved, r) {
			hit = resolved
		}
	}
	return resolved, hit, nil
}

func splitPath(p string) []string { return strings.Split(p, string(filepath.Separator)) }

// parseCountObjectsAlternates reads `git count-objects -v` the way git writes it:
// ONLY lines starting with exactly "alternate: " matter; the prefix is stripped and
// the rest of the line is the path, verbatim (no trimming: an unquoted path may end
// in a space) unless it starts with a double quote, in which case it is C-quoted.
// Every other line — whatever keys this git version prints, and any warning — is
// ignored. A quoted value that does not decode is an error (refuses).
func parseCountObjectsAlternates(out string) ([]string, error) {
	const prefix = "alternate: "
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		value, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		path, err := unquoteGitPath(value)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", value, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// parseAlternatesFile reads an objects/info/alternates file as git does: one entry
// per line, empty lines and lines starting with '#' skipped, nothing trimmed, a
// leading double quote means C-quoted. An entry that does not decode is an error.
func parseAlternatesFile(raw string) ([]string, error) {
	var entries []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entry, err := unquoteGitPath(line)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", line, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// unquoteGitPath decodes a path as git prints it: literal unless it starts with a
// double quote, in which case it is C-quoted (\a \b \f \n \r \t \v \\ \" and
// three-digit octal \NNN) and must end at the closing quote. Anything else refuses.
func unquoteGitPath(s string) (string, error) {
	if !strings.HasPrefix(s, `"`) {
		return s, nil
	}
	var out []byte
	i := 1
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"':
			if i != len(s)-1 {
				return "", fmt.Errorf("text after the closing quote")
			}
			return string(out), nil
		case c != '\\':
			out = append(out, c)
			i++
		default:
			i++
			if i >= len(s) {
				return "", fmt.Errorf("dangling backslash")
			}
			switch e := s[i]; e {
			case 'a':
				out = append(out, '\a')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'v':
				out = append(out, '\v')
			case '\\', '"':
				out = append(out, e)
			case '0', '1', '2', '3':
				if i+2 >= len(s) || !isOctal(s[i+1]) || !isOctal(s[i+2]) {
					return "", fmt.Errorf("bad octal escape")
				}
				out = append(out, (e-'0')<<6|(s[i+1]-'0')<<3|(s[i+2]-'0'))
				i += 2
			default:
				return "", fmt.Errorf("unknown escape \\%c", e)
			}
			i++
		}
	}
	return "", fmt.Errorf("no closing quote")
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
