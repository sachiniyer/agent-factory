package proctree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// This file decodes darwin's KERN_PROCARGS2 buffer, but it carries NO build
// tag, and that is deliberate.
//
// The parse is the most intricate part of the darwin backend — a length
// prefix, a skipped exec_path, variable NUL padding, then two NUL-separated
// string regions that must be split at exactly the right boundary. It is also
// the part where a bug is quietest: mis-split argv still looks like argv, and
// the resulting daemon-detection failure surfaces as "af reset does nothing"
// rather than a parse error (#1942).
//
// Keeping it platform-independent means every runner tests it, so the logic is
// covered on Linux CI even though the sysctl that feeds it exists only on
// macOS. The syscall stays in proctree_darwin.go; only the byte-shuffling
// lives here.

// parseProcArgs2 decodes a KERN_PROCARGS2 buffer, whose layout is:
//
//	int32   argc
//	char[]  exec_path, NUL-terminated
//	char[]  NUL padding up to the next argument
//	char[]  argv[0..argc-1], each NUL-terminated
//	char[]  envp[...], each NUL-terminated, ending at an empty string
//
// The argv boundaries it recovers are the entire point: they are what a
// pre-joined string (`ps -o args=`) has already destroyed. See Argv.
func parseProcArgs2(buf []byte) (argv []string, env []string, err error) {
	if len(buf) < 4 {
		return nil, nil, fmt.Errorf("procargs2 buffer is %d bytes, too short to hold argc", len(buf))
	}
	argc := int(int32(binary.NativeEndian.Uint32(buf[:4])))
	if argc < 0 {
		return nil, nil, fmt.Errorf("procargs2 reported a negative argc (%d)", argc)
	}
	rest := buf[4:]

	// Step over exec_path and the NUL padding after it. exec_path is NOT
	// argv[0]: it is the resolved executable, and argv[0] repeats after the
	// padding. Consuming it as argv[0] would shift every argument by one.
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return nil, nil, errors.New("procargs2 has no NUL-terminated exec_path")
	}
	rest = rest[end:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}

	// argv: exactly argc NUL-terminated strings. Running out early means the
	// buffer was truncated — report it rather than returning a short argv,
	// which a caller would read as a complete command line.
	for i := 0; i < argc; i++ {
		s, remainder, ok := nextCString(rest)
		if !ok {
			return nil, nil, fmt.Errorf("procargs2 ran out of data at argv[%d] of %d", i, argc)
		}
		argv = append(argv, s)
		rest = remainder
	}

	// envp: everything left, up to the first empty string.
	for len(rest) > 0 {
		s, remainder, ok := nextCString(rest)
		if !ok || s == "" {
			break
		}
		env = append(env, s)
		rest = remainder
	}
	return argv, env, nil
}

// nextCString splits one NUL-terminated string off the front of buf. An
// unterminated trailing run of bytes is not a string: the kernel always
// terminates, so a missing NUL means truncation and the value cannot be
// trusted.
func nextCString(buf []byte) (s string, rest []byte, ok bool) {
	i := bytes.IndexByte(buf, 0)
	if i < 0 {
		return "", nil, false
	}
	return string(buf[:i]), buf[i+1:], true
}
