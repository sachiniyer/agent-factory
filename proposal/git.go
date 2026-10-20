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
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME="+committerName,
		"GIT_AUTHOR_EMAIL="+committerEmail,
		"GIT_COMMITTER_NAME="+committerName,
		"GIT_COMMITTER_EMAIL="+committerEmail,
	)
}

var gitFixedConfig = []string{
	"-c", "core.hooksPath=" + os.DevNull,
	"-c", "commit.gpgSign=false",
	"-c", "core.autocrlf=false",
	"-c", "core.quotePath=false",
	"-c", "init.defaultBranch=main",
}

func (s *Store) git(stdin []byte, args ...string) (string, error) {
	full := append(append([]string{"-C", s.dir}, gitFixedConfig...), args...)
	cmd := exec.Command("git", full...)
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
	_, err := os.Stat(filepath.Join(s.dir, ".git"))
	return err == nil
}

// ensureRepo creates the store repository with one empty root commit, so HEAD
// always names a commit that cursors can point at. Call it with the lock held.
func (s *Store) ensureRepo() error {
	if s.initialized() {
		return nil
	}
	if _, err := s.git(nil, "init", "-q", "--template="); err != nil {
		return err
	}
	info := filepath.Join(s.dir, ".git", "info")
	if err := config.MkdirAllUnderAFHome(info, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(info, "exclude"), []byte(excludePatterns), 0o644); err != nil {
		return err
	}
	_, err := s.git(nil, "commit", "-q", "--allow-empty", "-m", commitMessage("initialize proposal store", AuthorExternal, ""))
	return err
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

// commitPaths stages pathspecs and commits them when anything changed. It
// returns the new commit, or "" when there was nothing to commit.
func (s *Store) commitPaths(subject string, author Author, sessionID string, pathspecs ...string) (string, error) {
	addArgs := append([]string{"add", "-A", "--"}, pathspecs...)
	if _, err := s.git(nil, addArgs...); err != nil {
		return "", err
	}
	diffArgs := append([]string{"diff", "--cached", "--quiet", "--"}, pathspecs...)
	_, err := s.git(nil, diffArgs...)
	if err == nil {
		return "", nil
	}
	if !isExitCode(err, 1) {
		return "", err
	}
	commitArgs := append([]string{"commit", "-q", "-m", commitMessage(subject, author, sessionID), "--"}, pathspecs...)
	if _, err := s.git(nil, commitArgs...); err != nil {
		return "", err
	}
	return s.head()
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
