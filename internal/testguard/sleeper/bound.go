package sleeper

import "testing"

// boundSource binds a unix listener on argv[1] — verbatim, so a relative
// spelling reaches bind() relative to the process's own working directory
// and the kernel records it as spelled — then sleeps argv[2] seconds like
// the plain sleeper. It is the holder a relative bound row is attributed
// to (#7695 R2): run it with Cmd.Dir set to somewhere af's own cwd is not,
// and "rel/x.sock" is a row only that cwd can place.
const boundSource = `package main

import (
	"net"
	"os"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-" {
		os.Exit(2)
	}
	ln, err := net.Listen("unix", os.Args[1])
	if err != nil {
		os.Exit(3)
	}
	defer ln.Close()
	secs := 300.0
	if len(os.Args) > 2 {
		if v, err := strconv.ParseFloat(os.Args[2], 64); err == nil {
			secs = v
		}
	}
	time.Sleep(time.Duration(secs * float64(time.Second)))
}
`

// BoundPath returns the bound-socket sleeper binary, built once like Path.
// Run it as `BoundPath(t) <sock> <secs>`: the child binds argv[1] as spelled
// — an absolute path, or a relative one resolved against ITS OWN cwd, so set
// Cmd.Dir to give the row a holder whose working directory is not af's —
// then sleeps. Unlike NonDumpablePath it keeps its dumpable bit, so the
// sweep's fd-table attribution names it the row's holder.
func BoundPath(t testing.TB) string {
	t.Helper()
	return build(t, boundSource)
}
