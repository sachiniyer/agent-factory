package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ListenAddrInHome reads another home's config.toml inside the daemon's
// listener lock — so it must be bounded (size, blocking) and must only answer
// the listen_addr question, never re-log the loader's warnings for a file it
// does not own.
func TestListenAddrInHome(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, TomlConfigFileName)

	t.Run("no config.toml is not found, not an error", func(t *testing.T) {
		addr, found, err := ListenAddrInHome(home)
		require.NoError(t, err)
		require.False(t, found)
		require.Empty(t, addr)
	})

	t.Run("the grouped [network] leaf wins over the flat spelling", func(t *testing.T) {
		require.NoError(t, os.WriteFile(path, []byte(
			"listen_addr = \"127.0.0.1:1111\"\n[network]\nlisten_addr = \"127.0.0.1:2222\"\n"), 0600))
		addr, found, err := ListenAddrInHome(home)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "127.0.0.1:2222", addr)
	})

	t.Run("the flat spelling fills in when the group is absent", func(t *testing.T) {
		require.NoError(t, os.WriteFile(path, []byte(
			"listen_addr = \"127.0.0.1:1111\"\n"), 0600))
		addr, found, err := ListenAddrInHome(home)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "127.0.0.1:1111", addr)
	})

	t.Run("an empty grouped value means the listener is disabled", func(t *testing.T) {
		require.NoError(t, os.WriteFile(path, []byte(
			"[network]\nlisten_addr = \"\"\n"), 0600))
		addr, found, err := ListenAddrInHome(home)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "", addr)
	})

	t.Run("an absent key resolves to the built-in default", func(t *testing.T) {
		require.NoError(t, os.WriteFile(path, []byte("schema_version = 1\n"), 0600))
		addr, found, err := ListenAddrInHome(home)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, DefaultListenAddr(), addr)
	})

	t.Run("unparseable config is found-but-errored, not silent", func(t *testing.T) {
		require.NoError(t, os.WriteFile(path, []byte("this is [ not toml"), 0600))
		_, found, err := ListenAddrInHome(home)
		require.Error(t, err)
		require.True(t, found)
	})

	t.Run("a symlinked config.toml is not followed", func(t *testing.T) {
		require.NoError(t, os.Remove(path))
		target := filepath.Join(home, "planted.toml")
		require.NoError(t, os.WriteFile(target, []byte(
			"[network]\nlisten_addr = \"127.0.0.1:3333\"\n"), 0600))
		require.NoError(t, os.Symlink(target, path))
		_, found, err := ListenAddrInHome(home)
		require.Error(t, err, "a symlink is not the home's config — O_NOFOLLOW")
		require.False(t, found)
		require.NoError(t, os.Remove(path))
	})

	t.Run("an oversized file is rejected, not read through", func(t *testing.T) {
		require.NoError(t, os.WriteFile(path,
			make([]byte, listenAddrInHomeMaxBytes+1), 0600))
		_, found, err := ListenAddrInHome(home)
		require.Error(t, err)
		require.False(t, found)
		require.NoError(t, os.Remove(path))
	})
}

// A FIFO standing in for config.toml must not block the read — the daemon's
// listener reconcile calls this under its mutex, and a blocking open on a
// special file would wedge every listener decision that follows.
func TestReadFileNoFollowBoundedNeverBlocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fifo semantics")
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "config.toml")
	require.NoError(t, syscall.Mkfifo(fifo, 0600))

	done := make(chan error, 1)
	go func() {
		_, err := readFileNoFollowBounded(fifo, 1024)
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err, "a FIFO is not a regular file")
	case <-time.After(3 * time.Second):
		t.Fatal("readFileNoFollowBounded blocked on a FIFO")
	}
}

// A dangling read must still distinguish "no file" cleanly enough for the
// claim gate's found/not-found split.
func TestReadFileNoFollowBoundedNotExist(t *testing.T) {
	_, err := readFileNoFollowBounded(filepath.Join(t.TempDir(), "missing.toml"), 1024)
	require.True(t, errors.Is(err, fs.ErrNotExist))
}
