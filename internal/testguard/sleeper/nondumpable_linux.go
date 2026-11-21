//go:build linux

package sleeper

import "testing"

// nonDumpableSource drops its own dumpable bit, then binds a unix listener on
// argv[1] — when one is given — and sleeps argv[2] seconds like the plain
// sleeper. prctl(PR_SET_DUMPABLE, 0) is the state a same-uid ssh-agent or
// gpg-agent lives in, and the state #7689's per-process reads regressed on:
// /proc/<pid>/fd and /proc/<pid>/exe refuse to the same uid while
// /proc/<pid>/net/unix still reads in full (measured on Linux 6.17, #7695).
// The fixture makes CI carry that shape — runners have no non-dumpable
// same-uid processes by default.
const nonDumpableSource = `package main

import (
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
)

func main() {
	// PR_SET_DUMPABLE is 4 on linux.
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, 4, 0, 0); errno != 0 {
		os.Exit(2)
	}
	if len(os.Args) > 1 && os.Args[1] != "-" {
		ln, err := net.Listen("unix", os.Args[1])
		if err != nil {
			os.Exit(3)
		}
		defer ln.Close()
	}
	secs := 300.0
	if len(os.Args) > 2 {
		if v, err := strconv.ParseFloat(os.Args[2], 64); err == nil {
			secs = v
		}
	}
	time.Sleep(time.Duration(secs * float64(time.Second)))
}
`

// NonDumpablePath returns the non-dumpable sleeper binary, built once like
// Path. Run it as `NonDumpablePath(t) <sock|"-"> <secs>`: with a socket path
// it binds a unix listener there (the holder whose fd table the sweep
// cannot read, #7695); with "-" it only sleeps (the process that must not
// make every path read unknown).
func NonDumpablePath(t testing.TB) string {
	t.Helper()
	return build(t, nonDumpableSource)
}
