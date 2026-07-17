// Package sockpath owns one fact: how long a Unix socket path may be.
//
// It exists because that fact was previously a hardcoded 103 copy-pasted into
// two packages, applied to the VS Code editor socket and NOT to the daemon's
// own control sockets — so the secondary feature failed with an actionable
// message while the core control plane failed with a bare kernel errno (#1940).
//
// # Where the numbers come from
//
// A Unix socket path lives in sockaddr_un.sun_path, a fixed-size char array,
// and the kernel rejects anything longer with EINVAL — "invalid argument",
// naming nothing. The array is 108 bytes on Linux and 104 on darwin, so the
// longest usable path differs BY PLATFORM.
//
// Max is read from x/sys's RawSockaddrUnix rather than hardcoded, because
// x/sys generates that type from each platform's own headers. A new GOOS gets
// the right number for free, and nobody has to remember to update a table.
package sockpath

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Max is the longest path THIS platform's kernel will bind: sun_path minus the
// NUL terminator. 107 on Linux, 103 on darwin.
//
// This is the bound for PRODUCT code, and it must be exact rather than
// conservative. A guard that rejected Linux's legal 104–107 byte paths would
// break installs that work today — turning a fix for an unactionable error into
// an outage for people who never had the problem. Reject what the kernel
// rejects; nothing more.
const Max = len(unix.RawSockaddrUnix{}.Path) - 1

// Portable is the longest path that fits on EVERY platform we ship (darwin's
// 104-byte sun_path is the binding constraint; Linux's 108 is roomier).
//
// This is the bound for TEST harnesses, and the difference from Max is the
// point. A test that builds a 105-byte socket path passes on Linux and fails on
// macOS — which is this repo's entire bug class in miniature: written on Linux,
// never run elsewhere. Harness code should hold itself to what works
// everywhere, so a portability break surfaces on the runner that finds it
// first rather than on a contributor's Mac.
//
// Unlike Max this is a policy value, not an ABI reading: it is the minimum
// across the platforms we ship, so it needs updating only if we ship a platform
// with a smaller sun_path than darwin's.
const Portable = 103

// Check returns an actionable error when path is too long to bind on this
// platform, and nil otherwise. what names the socket for the operator ("daemon
// control socket").
//
// The error names the path, its length, the ceiling and the knob to turn,
// because the kernel's own answer is "invalid argument" and a user staring at
// that has no way to connect it to their AGENT_FACTORY_HOME. That is the whole
// reason this check exists at all: not to prevent a bind failure — the kernel
// does that — but to make it diagnosable.
func Check(what, path string) error {
	if len(path) <= Max {
		return nil
	}
	return fmt.Errorf("the %s path %q is %d bytes, over this platform's %d-byte limit for a "+
		"unix socket path: set AGENT_FACTORY_HOME to a shorter directory",
		what, path, len(path), Max)
}
