package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const carryTestRel = "projects/-repo/5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d.jsonl"

func writeCarryFile(t *testing.T, home, rel, content string) string {
	t.Helper()
	path := filepath.Join(home, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func readCarryFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func carryHomes(t *testing.T) (string, string) {
	t.Helper()
	return t.TempDir(), t.TempDir()
}

func TestCarryConversationFileCreatesOwnerOnlyCopy(t *testing.T) {
	src, dst := carryHomes(t)
	const transcript = "{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n"
	srcPath := writeCarryFile(t, src, carryTestRel, transcript)

	require.NoError(t, carryConversationFile(src, dst, carryTestRel, true))

	dstPath := filepath.Join(dst, carryTestRel)
	require.Equal(t, transcript, readCarryFile(t, dstPath))
	info, err := os.Stat(dstPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the provider must be able to append, and nobody else to read")
	for _, dir := range []string{filepath.Join(dst, "projects"), filepath.Join(dst, "projects", "-repo")} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "created store directories must be owner-only: %s", dir)
	}
	require.Equal(t, transcript, readCarryFile(t, srcPath), "a carry must never change the source account")
	entries, err := os.ReadDir(filepath.Dir(dstPath))
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temporary file may be left beside the carried transcript")
}

func TestCarryConversationFileIsIdempotent(t *testing.T) {
	src, dst := carryHomes(t)
	writeCarryFile(t, src, carryTestRel, "line one\n")
	require.NoError(t, carryConversationFile(src, dst, carryTestRel, true))
	before, err := os.Stat(filepath.Join(dst, carryTestRel))
	require.NoError(t, err)

	require.NoError(t, carryConversationFile(src, dst, carryTestRel, true))
	after, err := os.Stat(filepath.Join(dst, carryTestRel))
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "an identical copy must not be rewritten on retry")
}

func TestCarryConversationFileKeepsTheSuperset(t *testing.T) {
	for _, tc := range []struct {
		name, src, dst, want string
	}{
		{
			name: "a retried launch already appended to the destination",
			src:  "turn one\n", dst: "turn one\nturn two\n", want: "turn one\nturn two\n",
		},
		{
			name: "a return trip finds a stale prefix in the destination",
			src:  "turn one\nturn two\n", dst: "turn one\n", want: "turn one\nturn two\n",
		},
		{
			name: "an empty destination is a prefix of everything",
			src:  "turn one\n", dst: "", want: "turn one\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := carryHomes(t)
			writeCarryFile(t, src, carryTestRel, tc.src)
			writeCarryFile(t, dst, carryTestRel, tc.dst)

			require.NoError(t, carryConversationFile(src, dst, carryTestRel, true))
			require.Equal(t, tc.want, readCarryFile(t, filepath.Join(dst, carryTestRel)))
			require.Equal(t, tc.src, readCarryFile(t, filepath.Join(src, carryTestRel)))
		})
	}
}

func TestCarryConversationFileRefusesDivergedDestination(t *testing.T) {
	src, dst := carryHomes(t)
	writeCarryFile(t, src, carryTestRel, "turn one\nturn two\n")
	dstPath := writeCarryFile(t, dst, carryTestRel, "turn one\nsomething else\n")

	err := carryConversationFile(src, dst, carryTestRel, true)
	require.Error(t, err)
	require.Equal(t, "the new account already holds a different copy of this conversation", carryFailureReason(err))
	require.Equal(t, "turn one\nsomething else\n", readCarryFile(t, dstPath),
		"neither history is a superset, so af must not destroy the destination's")
}

func TestCarryConversationFileRefusesSymlinks(t *testing.T) {
	t.Run("destination file", func(t *testing.T) {
		src, dst := carryHomes(t)
		writeCarryFile(t, src, carryTestRel, "turn one\n")
		outside := writeCarryFile(t, t.TempDir(), "target.jsonl", "")
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dst, carryTestRel)), 0o700))
		require.NoError(t, os.Symlink(outside, filepath.Join(dst, carryTestRel)))

		require.Error(t, carryConversationFile(src, dst, carryTestRel, true))
		require.Empty(t, readCarryFile(t, outside), "a planted destination symlink must never be written through")
	})
	t.Run("destination directory", func(t *testing.T) {
		src, dst := carryHomes(t)
		writeCarryFile(t, src, carryTestRel, "turn one\n")
		outside := t.TempDir()
		require.NoError(t, os.Symlink(outside, filepath.Join(dst, "projects")))

		require.Error(t, carryConversationFile(src, dst, carryTestRel, true))
		entries, err := os.ReadDir(outside)
		require.NoError(t, err)
		require.Empty(t, entries, "a symlinked store directory must not redirect the copy outside the account")
	})
	t.Run("destination home", func(t *testing.T) {
		src := t.TempDir()
		writeCarryFile(t, src, carryTestRel, "turn one\n")
		real := t.TempDir()
		dst := filepath.Join(t.TempDir(), "account")
		require.NoError(t, os.Symlink(real, dst))

		require.Error(t, carryConversationFile(src, dst, carryTestRel, true))
		entries, err := os.ReadDir(real)
		require.NoError(t, err)
		require.Empty(t, entries)
	})
	t.Run("source file", func(t *testing.T) {
		src, dst := carryHomes(t)
		secret := writeCarryFile(t, t.TempDir(), ".credentials.json", "secret")
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(src, carryTestRel)), 0o700))
		require.NoError(t, os.Symlink(secret, filepath.Join(src, carryTestRel)))

		require.Error(t, carryConversationFile(src, dst, carryTestRel, true))
		require.NoFileExists(t, filepath.Join(dst, carryTestRel),
			"a symlinked source must not smuggle another file into the new account")
	})
	t.Run("source directory", func(t *testing.T) {
		src, dst := carryHomes(t)
		elsewhere := t.TempDir()
		writeCarryFile(t, elsewhere, "-repo/5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d.jsonl", "turn one\n")
		require.NoError(t, os.Symlink(elsewhere, filepath.Join(src, "projects")))

		require.Error(t, carryConversationFile(src, dst, carryTestRel, true))
		require.NoFileExists(t, filepath.Join(dst, carryTestRel))
	})
}

func TestCarryConversationFileAllowsSymlinkedAmbientSourceRoot(t *testing.T) {
	real, dst := carryHomes(t)
	writeCarryFile(t, real, carryTestRel, "turn one\n")
	src := filepath.Join(t.TempDir(), ".claude")
	require.NoError(t, os.Symlink(real, src))

	require.NoError(t, carryConversationFile(src, dst, carryTestRel, true),
		"an ambient ~/.claude kept in a dotfiles checkout is the provider's real store")
	require.Equal(t, "turn one\n", readCarryFile(t, filepath.Join(dst, carryTestRel)))
}

func TestCarryConversationFileMissingSource(t *testing.T) {
	t.Run("a new carry needs the source", func(t *testing.T) {
		src, dst := carryHomes(t)
		writeCarryFile(t, dst, carryTestRel, "stale prefix\n")
		err := carryConversationFile(src, dst, carryTestRel, true)
		require.Error(t, err)
		require.Equal(t, "its transcript is missing from the previous account's home", carryFailureReason(err),
			"a stale destination copy must not be resumed as though it were the whole conversation")
	})
	t.Run("a committed carry accepts its landed copy", func(t *testing.T) {
		src, dst := carryHomes(t)
		writeCarryFile(t, dst, carryTestRel, "carried\n")
		require.NoError(t, carryConversationFile(src, dst, carryTestRel, false))
	})
	t.Run("a committed carry with no copy anywhere fails", func(t *testing.T) {
		src, dst := carryHomes(t)
		require.Error(t, carryConversationFile(src, dst, carryTestRel, false))
	})
	t.Run("a missing ambient source home", func(t *testing.T) {
		dst := t.TempDir()
		err := carryConversationFile(filepath.Join(t.TempDir(), "absent"), dst, carryTestRel, true)
		require.Error(t, err)
		require.Contains(t, carryFailureReason(err), "missing")
	})
}

func TestCarryConversationFileSameStoreIsANoOp(t *testing.T) {
	home := t.TempDir()
	path := writeCarryFile(t, home, carryTestRel, "turn one\n")
	before, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, carryConversationFile(home, home, carryTestRel, true))
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
}

func TestSplitCarryPathRefusesPathsOutsideTheConversationStore(t *testing.T) {
	for _, rel := range []string{
		"",
		"auth.json",
		".credentials.json",
		"../projects/x.jsonl",
		"/projects/x.jsonl",
		"projects/../auth.json",
		"projects/./x.jsonl",
		"projects",
		"config.toml/x.jsonl",
	} {
		_, _, err := splitCarryPath(rel, true)
		require.Error(t, err, "carry path %q must be refused", rel)
		require.Error(t, carryConversationFile(t.TempDir(), t.TempDir(), rel, true), "carry path %q must be refused", rel)
	}
	dirs, name, err := splitCarryPath("sessions/2026/09/16/rollout-x.jsonl", true)
	require.NoError(t, err)
	require.Equal(t, []string{"sessions", "2026", "09", "16"}, dirs)
	require.Equal(t, "rollout-x.jsonl", name)
}

func TestCarryConversationTreeCopiesRegularFilesOnly(t *testing.T) {
	src, dst := carryHomes(t)
	const aux = "projects/-repo/5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d"
	writeCarryFile(t, src, aux+"/tool-results/one.txt", "result")
	writeCarryFile(t, src, aux+"/subagents/agent-1.jsonl", "sub\n")
	secret := writeCarryFile(t, t.TempDir(), "secret", "secret")
	require.NoError(t, os.Symlink(secret, filepath.Join(src, aux, "link")))

	require.NoError(t, carryConversationTree(src, dst, aux))
	require.Equal(t, "result", readCarryFile(t, filepath.Join(dst, aux, "tool-results", "one.txt")))
	require.Equal(t, "sub\n", readCarryFile(t, filepath.Join(dst, aux, "subagents", "agent-1.jsonl")))
	_, err := os.Lstat(filepath.Join(dst, aux, "link"))
	require.True(t, os.IsNotExist(err), "symlinks in the per-session directory are not carried")

	require.NoError(t, carryConversationTree(t.TempDir(), dst, aux),
		"a conversation with no per-session directory has nothing extra to carry")
}
