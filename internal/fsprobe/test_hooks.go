package fsprobe

import (
	"os"
	"time"
)

// Test-only seams exposed to external test packages that need to verify a
// caller routes a metadata lookup through the bounded fsprobe.Stat rather
// than a raw os.Stat. These follow the repo's ForTest convention (see e.g.
// config.SetRegisteredProjectProofRaceHookForTest): the underlying package
// vars stay unexported in production, and the helpers restore the prior value
// via the returned closure so no test can permanently change the bound.

// SetStatPathForTest overrides the underlying metadata syscall Stat runs
// inside its bounded flight. Callers must restore the original via the
// returned function. Useful for standing in for a wedged NFS/FUSE mount
// whose stat blocks in an uninterruptible D state, so an integration test
// can prove a caller's stat is bounded by fsprobe rather than hanging.
func SetStatPathForTest(f func(string) (os.FileInfo, error)) func() {
	old := statPath
	statPath = f
	return func() { statPath = old }
}

// SetProbeTimeoutForTest overrides the per-flight bound. Callers must
// restore the original via the returned function. Useful for keeping an
// integration test that exercises a wedged flight fast.
func SetProbeTimeoutForTest(d time.Duration) func() {
	old := probeTimeout
	probeTimeout = d
	return func() { probeTimeout = old }
}

// SetReadDirForTest overrides the directory read ReadDir and ReadDirInfo run
// inside their bounded flights, so a test can stand in for a conversation
// store on a wedged mount (#7212). Callers must restore the original via the
// returned function.
func SetReadDirForTest(f func(string) ([]os.DirEntry, error)) func() {
	old := readDirPath
	readDirPath = f
	return func() { readDirPath = old }
}

// SetOpenForTest overrides the open ReadFileWith runs inside its bounded
// flight, so a test can stand in for a file on a wedged mount (#7221 review
// R4). Callers must restore the original via the returned function.
func SetOpenForTest(f func(string) (*os.File, error)) func() {
	old := openPath
	openPath = f
	return func() { openPath = old }
}
