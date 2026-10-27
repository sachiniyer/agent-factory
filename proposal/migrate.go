package proposal

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
	"golang.org/x/sys/unix"
)

// legacyGitDir is where builds before the git dir moved out of the docs
// folder kept it.
func (s *Store) legacyGitDir() string { return filepath.Join(s.dir, ".git") }

// importLegacyGitDir copies the history of a legacy in-folder git dir into the
// fresh repository dst. An agent could have written anything in that folder,
// so only object and ref data is carried over: its config, hooks, attributes,
// alternates and gitdir pointers are left behind, and any symlink or special
// file aborts the import. It reports false when there is no legacy git dir.
func (s *Store) importLegacyGitDir(dst string) (bool, error) {
	src := s.legacyGitDir()
	info, err := os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !info.IsDir()) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	root, err := os.OpenRoot(src)
	if err != nil {
		return false, err
	}
	defer root.Close()
	if opened, err := root.Stat("."); err != nil || !os.SameFile(info, opened) {
		return false, fmt.Errorf("%s changed while it was opened", src)
	}
	for _, name := range []string{"objects", "refs", "packed-refs"} {
		if err := copyTreeFromRoot(root, name, dst); err != nil {
			return false, err
		}
	}
	if _, err := s.gitIn(dst, nil, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return false, fmt.Errorf("no commit on the main branch: %w", err)
	}
	return true, nil
}

var legacySkip = map[string]bool{
	"objects/info/alternates":      true,
	"objects/info/http-alternates": true,
}

func copyTreeFromRoot(root *os.Root, name, dst string) error {
	if _, err := root.Lstat(name); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fs.WalkDir(root.FS(), name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if legacySkip[p] {
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(p))
		switch {
		case d.IsDir():
			return config.MkdirAllUnderAFHome(target, 0o755)
		case d.Type().IsRegular():
			return copyRegularFromRoot(root, p, target)
		}
		return fmt.Errorf("%s is not a regular file or folder", path.Join(".git", p))
	})
}

func copyRegularFromRoot(root *os.Root, name, target string) error {
	in, err := root.OpenFile(name, os.O_RDONLY|noFollowFlag, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	if info, err := in.Stat(); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path.Join(".git", name))
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

const noFollowFlag = unix.O_NOFOLLOW | unix.O_NONBLOCK
