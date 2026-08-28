package upgradetxn

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Fixture modes have to be STATED, not inherited (#3470, following #3465).
//
// os.WriteFile and os.Mkdir take a mode argument, and the process umask masks it.
// A fixture that relies on that argument therefore does not have the mode it
// names: under umask 077 a 0755 write lands 0700 and a 0710 mkdir lands 0700. A
// test that then asserts the named mode is asserting bits that were never set —
// so it fails on a hardened box, and, worse, it PASSES for the wrong reason
// everywhere else, because the property it claims to check was never established.
//
// sharedInstallDir has done exactly this dance for the install directory since
// #3011, and its comment already predicted this defect by name: "under umask 077
// the write lands on 0700, and the hard-link test then asserts a 0755 that was
// never there." These helpers make that pattern available to every fixture whose
// mode an assertion depends on, rather than each one re-deriving it.
//
// Each one writes, chmods, and then PROVES the result — the proof is the point,
// since the whole failure mode is a mode you believe you set and did not.

func writeFileWithMode(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, data, perm))
	require.NoError(t, os.Chmod(path, perm))
	requireMode(t, path, perm)
}

func mkdirWithMode(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	require.NoError(t, os.Mkdir(path, perm))
	require.NoError(t, os.Chmod(path, perm))
	requireMode(t, path, perm)
}

// mkdirAllWithMode creates path and stamps perm on the leaf. Intermediate parents
// keep whatever MkdirAll gave them; callers that assert on a parent should create
// it explicitly with mkdirWithMode.
func mkdirAllWithMode(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, perm))
	require.NoError(t, os.Chmod(path, perm))
	requireMode(t, path, perm)
}

func requireMode(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, perm, info.Mode().Perm(),
		"fixture %s must actually carry the mode it names whatever the umask; an assertion "+
			"against a mode that was never set proves nothing", filepath.Base(path))
}
