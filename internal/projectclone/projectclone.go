// Package projectclone clones a git repository onto the DAEMON's host so it can
// be registered as a project (W9.10). The URL comes from a client that may be
// remote and is therefore untrusted: Validate is the gate, and Clone refuses to
// run on anything that has not passed it.
package projectclone

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/afhome"
)

// cloneTimeout bounds one clone. A large repository over a slow link needs
// minutes; a stalled one must not hold a daemon goroutine forever.
const cloneTimeout = 15 * time.Minute

const (
	// originCheckTimeout bounds the "is the existing checkout ours" probe.
	originCheckTimeout = 30 * time.Second
	// waitDelay is how long a finished or killed git may keep the output pipes
	// open (a grandchild that inherited them) before the drain is abandoned.
	waitDelay = 10 * time.Second
	// Budget is the longest one Clone call can take: the probe, the clone, and
	// the drain after each kill, plus slack for the bookkeeping around them. A
	// caller that waits for a clone must wait at least this long — the server
	// side of the contract, so no client keeps a shorter private number (#6093).
	Budget = originCheckTimeout + cloneTimeout + 2*waitDelay + 30*time.Second
)

// scpLike is git's "user@host:path" form. The pieces are restricted to the
// characters a hostname and a repository path can need, which keeps option-like
// values, shell metacharacters and "ext::"-style helpers out by construction.
var scpLike = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9][A-Za-z0-9.-]*:[A-Za-z0-9._~/-]+$`)

// LooksLikeURL reports whether a user-typed project argument is meant as a git
// URL rather than a filesystem path. It decides ROUTING only; Validate decides
// whether the URL is acceptable. A scheme-bearing value or an scp-like value is
// a URL; anything else (including "./repo", "/srv/repo", "~/repo") is a path.
func LooksLikeURL(s string) bool {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") {
		return true
	}
	return scpLike.MatchString(s)
}

// Validate accepts https://host/path, ssh://[user@]host[:port]/path and
// user@host:path, and refuses everything else with a reason. In particular:
//
//   - file://, bare paths, git:// and http:// (a local read, or a transport
//     with no integrity), and git's ext::/fd:: helper syntax, are refused;
//   - a value that could be taken for an option ("-…"), or that contains
//     whitespace, control characters or a backslash, is refused;
//   - an https/ssh URL carrying a PASSWORD is refused, because the clone would
//     persist it in the repository's config; the daemon box's own credentials
//     (credential helper, ssh keys) are what authenticate;
//   - ".." path segments are refused, so the URL cannot name a path outside the
//     repository it points at.
func Validate(raw string) error {
	if raw != strings.TrimSpace(raw) || raw == "" {
		return fmt.Errorf("git URL must not be empty or have surrounding whitespace")
	}
	if strings.HasPrefix(raw, "-") {
		return fmt.Errorf("git URL %q looks like a command-line option", raw)
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f || r == '\\' {
			return fmt.Errorf("git URL contains a space, control character, or backslash")
		}
	}
	if strings.Contains(raw, "::") && !strings.Contains(raw, "://") {
		return fmt.Errorf("git URL %q uses a transport helper, which af does not allow", raw)
	}
	if !strings.Contains(raw, "://") {
		if !scpLike.MatchString(raw) {
			return fmt.Errorf("%q is not a git URL: use https://host/owner/repo, ssh://host/owner/repo or user@host:owner/repo (local paths and file:// are not accepted for a clone)", raw)
		}
		return noDotDot(raw[strings.Index(raw, ":")+1:])
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid git URL: %v", err)
	}
	switch u.Scheme {
	case "https", "ssh":
	case "file":
		return fmt.Errorf("file:// is not accepted: the daemon would read a path on its own disk for a remote caller")
	default:
		return fmt.Errorf("git URL scheme %q is not accepted: use https or ssh", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("git URL has no host")
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			return fmt.Errorf("git URL must not embed a password: the clone would store it in the repository config; authenticate with the daemon box's ssh key or credential helper")
		}
	}
	if strings.Trim(u.Path, "/") == "" {
		return fmt.Errorf("git URL names no repository path")
	}
	return noDotDot(u.Path)
}

func noDotDot(path string) error {
	for _, seg := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' }) {
		if seg == ".." {
			return fmt.Errorf("git URL path must not contain '..'")
		}
	}
	return nil
}

// RepoName derives the checkout's directory name from a validated URL: the last
// path segment without ".git". The result is a single path element made only of
// [A-Za-z0-9._-], never "." or "..", so joining it under a root cannot escape.
func RepoName(raw string) (string, error) {
	path := raw
	if i := strings.Index(raw, "://"); i >= 0 {
		u, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		path = u.Path
	} else if i := strings.Index(raw, ":"); i >= 0 {
		path = raw[i+1:]
	}
	segs := strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
	if len(segs) == 0 {
		return "", fmt.Errorf("git URL names no repository")
	}
	name := strings.TrimSuffix(segs[len(segs)-1], ".git")
	if name == "" || name == "." || name == ".." || strings.Trim(name, "._-") == "" {
		return "", fmt.Errorf("cannot derive a directory name from %q", raw)
	}
	for _, r := range name {
		ok := r == '.' || r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return "", fmt.Errorf("repository name %q has characters a directory name cannot carry", name)
		}
	}
	return name, nil
}

// Runner runs git. A seam so tests can stand in for the network.
type Runner func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error)

func gitRunner(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	// A clone over ssh runs ssh and credential helpers as children of git, and
	// they inherit the output pipes. Killing git alone leaves them holding the
	// pipes, so the drain would never end. Run git as a process-group leader,
	// kill the whole group on cancel, and abandon the drain after waitDelay
	// regardless (#6094).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = waitDelay
	return cmd.CombinedOutput()
}

// RunGit is the production Runner; tests replace the package variable.
var RunGit Runner = gitRunner

// cloneEnv is the daemon's environment narrowed for a clone of an untrusted
// URL: no prompts (a daemon has no terminal), only https and ssh transports,
// and ssh in batch mode so a missing key fails instead of waiting for input.
func cloneEnv() []string {
	env := append([]string(nil), os.Environ()...)
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=https:ssh")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	return env
}

// Clone clones the validated URL into <root>/<repo-name> and returns that path.
// It is idempotent for the same origin: an existing checkout whose origin is
// url is returned as-is, while any other occupant of the path is refused rather
// than overwritten. A failed clone removes the partial directory it created.
func Clone(ctx context.Context, root, rawURL string) (string, error) {
	if err := Validate(rawURL); err != nil {
		return "", err
	}
	name, err := RepoName(rawURL)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("projects root %q must be absolute", root)
	}
	if err := afhome.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create projects root %s: %w", root, err)
	}
	dest := filepath.Join(root, name)
	if rel, err := filepath.Rel(root, dest); err != nil || rel != name {
		return "", fmt.Errorf("refusing clone destination %q outside %s", dest, root)
	}
	if _, err := os.Lstat(dest); err == nil {
		pctx, pcancel := context.WithTimeout(ctx, originCheckTimeout)
		origin, oerr := RunGit(pctx, root, cloneEnv(), "config", "--file", filepath.Join(dest, ".git", "config"), "--get", "remote.origin.url")
		pcancel()
		if oerr == nil && strings.TrimSpace(string(origin)) == rawURL {
			return dest, nil
		}
		return "", fmt.Errorf("%s already exists and is not a clone of %s; remove it or register that path directly", dest, rawURL)
	}
	ctx, cancel := context.WithTimeout(ctx, cloneTimeout)
	defer cancel()
	// "--" ends option parsing, so the URL can never be read as a flag even if
	// Validate regresses; submodules are not fetched (their URLs are the cloned
	// repository's, not the caller's).
	if out, err := RunGit(ctx, root, cloneEnv(), "clone", "--no-recurse-submodules", "--", rawURL, name); err != nil {
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("git clone %s failed on the daemon host: %v: %s", rawURL, err, strings.TrimSpace(string(out)))
	}
	return dest, nil
}

// Flights tracks the clones in progress, keyed by destination, so concurrent
// registrations neither race to create one directory nor queue behind an
// unrelated clone. Its mutex guards the table only: it is never held while git
// runs or its output drains (#6094).
type Flights struct {
	mu sync.Mutex
	m  map[string]*flight
}

type flight struct {
	url  string
	done chan struct{}
	path string
	err  error
}

// Do runs run for dest unless a clone of the same url into dest is already in
// progress, in which case it waits for that clone (bounded by ctx) and shares
// its outcome. A clone of a DIFFERENT url into the same dest is refused at once
// rather than queued: it could only end in "already exists".
func (f *Flights) Do(ctx context.Context, dest, url string, run func() (string, error)) (string, error) {
	f.mu.Lock()
	if cur, ok := f.m[dest]; ok {
		f.mu.Unlock()
		if cur.url != url {
			return "", fmt.Errorf("%s is being cloned from %s right now; wait for that to finish or register a different repository", dest, cur.url)
		}
		select {
		case <-cur.done:
			return cur.path, cur.err
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for the clone of %s already in progress: %w", url, ctx.Err())
		}
	}
	if f.m == nil {
		f.m = map[string]*flight{}
	}
	cur := &flight{url: url, done: make(chan struct{})}
	f.m[dest] = cur
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		delete(f.m, dest)
		f.mu.Unlock()
		close(cur.done)
	}()
	cur.path, cur.err = run()
	return cur.path, cur.err
}
