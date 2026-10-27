package proposal

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

const (
	committerName  = "Agent Factory"
	committerEmail = "af@localhost"
)

// excludePatterns keep the lock file and common editor droppings out of
// snapshots.
const excludePatterns = ".lock\n*.swp\n*.swx\n*~\n.#*\n#*#\n.DS_Store\n.write-*\n"

// gitEnv isolates the store from the caller's git environment. GIT_DIR and
// friends leak in when af runs from a git hook or inside a worktree, and the
// user's global config may set hooks, signing, or templates.
func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME="+committerName,
		"GIT_AUTHOR_EMAIL="+committerEmail,
		"GIT_COMMITTER_NAME="+committerName,
		"GIT_COMMITTER_EMAIL="+committerEmail,
	)
}

// gitFixedConfig pins the settings that could make git run a program. The
// repository config already lives outside the folder agents write; this is
// defence in depth.
var gitFixedConfig = gitConfigArgs(
	"core.hooksPath="+os.DevNull,
	"core.fsmonitor=false",
	"core.attributesFile="+os.DevNull,
	"commit.gpgSign=false",
	"core.autocrlf=false",
	"core.quotePath=false",
	"init.defaultBranch=main",
)

func gitConfigArgs(settings ...string) []string {
	var args []string
	for _, kv := range settings {
		args = append(args, "-c", kv)
	}
	return args
}

// gitDir is the store's git directory. It sits beside the docs folder, not in
// it, so nothing an agent writes there (a .git dir or file, a config) can
// choose the repository af commits to or a program git runs.
func (s *Store) gitDir() string { return s.dir + ".git" }

func (s *Store) git(stdin []byte, args ...string) (string, error) {
	return s.gitIn(s.gitDir(), stdin, args...)
}

func (s *Store) gitIn(gitDir string, stdin []byte, args ...string) (string, error) {
	full := append([]string{"--git-dir=" + gitDir, "--work-tree=" + s.dir, "-C", s.dir}, gitFixedConfig...)
	cmd := exec.Command("git", append(full, args...)...)
	cmd.Env = gitEnv()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (s *Store) gitTrim(args ...string) (string, error) {
	out, err := s.git(nil, args...)
	return strings.TrimSpace(out), err
}

func (s *Store) initialized() bool {
	info, err := os.Lstat(s.gitDir())
	return err == nil && info.IsDir()
}

// present reports whether the store has a repository. A legacy in-folder
// repository is imported first, so read paths see its history.
func (s *Store) present() (bool, error) {
	if s.initialized() {
		return true, nil
	}
	if info, err := os.Lstat(s.legacyGitDir()); err != nil || !info.IsDir() {
		return false, nil
	}
	if err := s.withLock(func() error { return nil }); err != nil {
		return false, err
	}
	return true, nil
}

// ensureRepo creates the store repository with one empty root commit, so HEAD
// always names a commit that cursors can point at. A store made by a build
// that kept its git dir inside the docs folder is imported instead. The
// repository is built in a temporary directory and renamed into place, so a
// crash never leaves a half-made one. Call it with the lock held.
func (s *Store) ensureRepo() error {
	if s.initialized() {
		return nil
	}
	tmp, err := os.MkdirTemp(filepath.Dir(s.dir), filepath.Base(s.gitDir())+".init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := s.gitIn(tmp, nil, "init", "-q", "--template="); err != nil {
		return err
	}
	info := filepath.Join(tmp, "info")
	if err := config.MkdirAllUnderAFHome(info, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(info, "exclude"), []byte(excludePatterns), 0o644); err != nil {
		return err
	}
	legacy, err := s.importLegacyGitDir(tmp)
	if err != nil {
		return fmt.Errorf("import proposal history from %s: %w; move that folder away to start a new history", s.legacyGitDir(), err)
	}
	if legacy {
		if _, err := s.gitIn(tmp, nil, "read-tree", "HEAD"); err != nil {
			return err
		}
	} else {
		if _, err := s.gitIn(tmp, nil, "commit", "-q", "--allow-empty", "-m", commitMessage("initialize proposal store", AuthorExternal, "")); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, s.gitDir()); err != nil {
		return err
	}
	if legacy {
		return os.RemoveAll(s.legacyGitDir())
	}
	return nil
}

func (s *Store) head() (string, error) {
	return s.gitTrim("rev-parse", "--verify", "HEAD^{commit}")
}

func commitMessage(subject string, author Author, sessionID string) string {
	msg := subject + "\n\nAf-Author: " + string(author) + "\n"
	if author == AuthorAgent && sessionID != "" {
		msg += "Af-Session: " + sessionID + "\n"
	}
	return msg
}

// commitPaths stages the docs that pathspecs name and commits them when
// anything changed. It returns the new commit, or "" when there was nothing to
// commit. On failure the index goes back to HEAD. It uses only plumbing that
// reads the index, so git never scans the docs folder (see stage).
func (s *Store) commitPaths(subject string, author Author, sessionID string, pathspecs ...string) (string, error) {
	commit, err := s.commitStaged(subject, author, sessionID, pathspecs)
	if err != nil {
		_, _ = s.git(nil, "read-tree", "HEAD")
	}
	return commit, err
}

func (s *Store) commitStaged(subject string, author Author, sessionID string, pathspecs []string) (string, error) {
	if err := s.stage(pathspecs); err != nil {
		return "", err
	}
	head, err := s.head()
	if err != nil {
		return "", err
	}
	tree, err := s.gitTrim("write-tree")
	if err != nil {
		return "", err
	}
	headTree, err := s.gitTrim("rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return "", err
	}
	if tree == headTree {
		return "", nil
	}
	out, err := s.git([]byte(commitMessage(subject, author, sessionID)), "commit-tree", tree, "-p", head)
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(out)
	if _, err := s.git(nil, "update-ref", "HEAD", commit, head); err != nil {
		return "", err
	}
	return commit, nil
}

// snapshot commits every uncommitted change outside the excluded pathspecs as
// an external edit.
func (s *Store) snapshot(exclude ...string) (string, error) {
	specs := []string{"."}
	for _, e := range exclude {
		specs = append(specs, ":(exclude)"+e)
	}
	return s.commitPaths("snapshot external edits", AuthorExternal, "", specs...)
}
