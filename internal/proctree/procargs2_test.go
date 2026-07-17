package proctree

import (
	"encoding/binary"
	"testing"
)

// buildProcArgs2 assembles a KERN_PROCARGS2 buffer the way the darwin kernel
// does, so the parse can be exercised on any runner. padding reproduces the
// variable run of NULs the kernel leaves between exec_path and argv[0].
func buildProcArgs2(execPath string, argv, env []string, padding int) []byte {
	buf := make([]byte, 4)
	binary.NativeEndian.PutUint32(buf, uint32(int32(len(argv))))
	buf = append(buf, []byte(execPath)...)
	buf = append(buf, 0)
	for i := 0; i < padding; i++ {
		buf = append(buf, 0)
	}
	for _, a := range argv {
		buf = append(buf, []byte(a)...)
		buf = append(buf, 0)
	}
	for _, e := range env {
		buf = append(buf, []byte(e)...)
		buf = append(buf, 0)
	}
	return buf
}

// TestParseProcArgs2PreservesSpacedArgv is the #1942 regression at the parse
// layer: an af installed under a path with a space is ONE argv entry, and the
// old darwin path (`ps -o args=` re-split on whitespace) turned it into two.
// A Mac user with af under "/Users/John Smith/…" — an ordinary macOS home —
// had `af reset`, health and foreign-daemon scans all fail to recognise their
// own daemon.
func TestParseProcArgs2PreservesSpacedArgv(t *testing.T) {
	want := []string{"/Users/John Smith/.local/bin/af", "--daemon", "af-test"}
	buf := buildProcArgs2("/Users/John Smith/.local/bin/af", want, []string{"PATH=/usr/bin"}, 3)

	argv, env, err := parseProcArgs2(buf)
	if err != nil {
		t.Fatalf("parseProcArgs2: %v", err)
	}
	if len(argv) != len(want) {
		t.Fatalf("argv = %q (%d entries), want %q (%d entries) — a spaced install path must stay ONE argument",
			argv, len(argv), want, len(want))
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
	if len(env) != 1 || env[0] != "PATH=/usr/bin" {
		t.Errorf("env = %q, want [PATH=/usr/bin]", env)
	}
}

// TestParseProcArgs2SkipsExecPath locks the boundary that is easiest to get
// wrong: exec_path precedes argv[0] and repeats it, so consuming it as argv[0]
// shifts every argument by one and still looks like a plausible command line.
func TestParseProcArgs2SkipsExecPath(t *testing.T) {
	buf := buildProcArgs2("/usr/bin/sleep", []string{"sleep", "300"}, nil, 7)
	argv, _, err := parseProcArgs2(buf)
	if err != nil {
		t.Fatalf("parseProcArgs2: %v", err)
	}
	if len(argv) != 2 || argv[0] != "sleep" || argv[1] != "300" {
		t.Errorf("argv = %q, want [sleep 300] — exec_path must be skipped, not read as argv[0]", argv)
	}
}

func TestParseProcArgs2NoPadding(t *testing.T) {
	buf := buildProcArgs2("/bin/x", []string{"x", "-v"}, nil, 0)
	argv, _, err := parseProcArgs2(buf)
	if err != nil {
		t.Fatalf("parseProcArgs2: %v", err)
	}
	if len(argv) != 2 || argv[0] != "x" || argv[1] != "-v" {
		t.Errorf("argv = %q, want [x -v]", argv)
	}
}

// TestParseProcArgs2EmptyEnv pins a fact that is easy to misread, so read the
// caveat: a buffer with no env section parses fine and yields no variables.
//
// That is CORRECT here and DANGEROUS one layer up. An env-less buffer is what
// `env -i cmd` produces — and it is also what darwin produces when it REDACTS a
// foreign process's environment, without saying so. The two are byte-for-byte
// identical, so this parse cannot tell them apart and must not pretend to.
// Noticing that no answer came back is Environ's job — it classifies a
// zero-variable result as unreadable, which covers every ground the kernel
// might have withheld on at once, without this layer (or any layer) trying to
// predict which.
//
// An earlier version of this test asserted "env must come back empty rather
// than error" as if empty were proof of absence. It is not proof of anything.
func TestParseProcArgs2EmptyEnv(t *testing.T) {
	buf := buildProcArgs2("/bin/x", []string{"x"}, nil, 1)
	argv, env, err := parseProcArgs2(buf)
	if err != nil {
		t.Fatalf("parseProcArgs2: %v", err)
	}
	if len(argv) != 1 || argv[0] != "x" {
		t.Errorf("argv = %q, want [x]", argv)
	}
	if len(env) != 0 {
		t.Errorf("env = %q, want empty", env)
	}
}

// TestParseProcArgs2RejectsTruncated makes sure a short buffer is an ERROR and
// not a short argv. A truncated read that returned the arguments it managed to
// find would be indistinguishable from a process that really was invoked with
// fewer arguments — and classification (#1214) turns on exactly that.
func TestParseProcArgs2RejectsTruncated(t *testing.T) {
	full := buildProcArgs2("/bin/x", []string{"x", "--daemon", "af-test"}, nil, 1)
	for _, tc := range []struct {
		name string
		buf  []byte
	}{
		{"empty", nil},
		{"argc only", full[:4]},
		{"mid-argv", full[:len(full)-6]},
		{"no exec_path terminator", []byte{1, 0, 0, 0, 'a', 'b'}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if argv, _, err := parseProcArgs2(tc.buf); err == nil {
				t.Errorf("parseProcArgs2(%s) = %q, want an error", tc.name, argv)
			}
		})
	}
}

// TestParseProcArgs2ZeroArgc covers a kernel thread: argc 0 is legitimate and
// must not error, but it yields no argv, which readArgv reports as "no argv".
func TestParseProcArgs2ZeroArgc(t *testing.T) {
	buf := buildProcArgs2("/kernel/thread", nil, nil, 2)
	argv, _, err := parseProcArgs2(buf)
	if err != nil {
		t.Fatalf("parseProcArgs2: %v", err)
	}
	if len(argv) != 0 {
		t.Errorf("argv = %q, want empty for argc=0", argv)
	}
}
