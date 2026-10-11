package sessionenv

import (
	"bytes"
	"debug/elf"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// pathEntryHoldsExecutable reports whether dir contains an executable file
// named name that execve can actually start — the local-filesystem probe
// the search-path walk uses for execvp's "first entry that holds it"
// question. Mode bits alone are not the answer: execvp's walk is EACCES-aware
// and skips a file it cannot actually execute, so the probe is the daemon's
// own effective access — a bit-check that ignores it would attribute a hit
// the real exec never makes and let resolution fall through to a later
// relative entry (#5265 review). Access is not enough either: a script whose
// shebang interpreter is missing, or a dynamic ELF whose PT_INTERP loader is
// gone, stats and access-checks cleanly but execve returns ENOENT — and the
// walk continues past it exactly as if the file were absent, reaching a later
// repository-controlled entry. "Holds" therefore means the file exists AND
// the loader chain it names exists (#5265 review).
func pathEntryHoldsExecutable(dir, name string, environ []string, roots []string, depth int) bool {
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() &&
		info.Mode().Perm()&0o111 != 0 && executableAccess(path) &&
		execCanStart(path, environ, roots, depth)
}

// execCanStart reports whether execve can start path without handing the
// grant to the repository — the part of "holds the executable" stat and
// access cannot see. execve answers ENOENT after a successful stat in
// exactly the interpreter-dependency cases: a script whose shebang names an
// interpreter that does not exist, and a dynamic ELF whose PT_INTERP loader
// does not. Every other failure keeps the hit — an ENOEXEC file is run by
// the shell itself, and a loader that starts but errors inside has already
// begun the operator file, not fallen through to the repository. A started
// interpreter can still delegate: a "#!env node" script begins env, which
// PATH-resolves node — possibly into the repository — so an env
// interpreter's argument is attributed with the same rules the top-level
// command gets (#5265 review). Interpreters and delegates are followed
// transitively with one shared depth bound so a cyclic chain denies rather
// than walks forever.
func execCanStart(path string, environ []string, roots []string, depth int) bool {
	const maxAttributionDepth = 8
	if depth > maxAttributionDepth {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	switch {
	case startsWithShebang(file):
		interpreter, arg := shebangInterpreter(file)
		if interpreter == "" {
			return true
		}
		if !executableFileExists(interpreter) ||
			!execCanStart(interpreter, environ, roots, depth+1) {
			return false
		}
		return envDelegateAttributable(interpreter, arg, environ, roots, depth+1)
	case startsWithELF(file):
		loader := elfInterpreter(file)
		return loader == "" ||
			(executableFileExists(loader) && execCanStart(loader, environ, roots, depth+1))
	default:
		return true
	}
}

// envDelegateAttributable reports whether the file an env interpreter
// delegates to attributes to the operator. The kernel splits a shebang line
// into interpreter plus a SINGLE argument, so `#!/usr/bin/env node` starts
// env with the one argument "node" and env then PATH-resolves it — the
// delegate is attributed by the same rules a typed command gets: absolute
// outside the repository, or a PATH hit whose winning entry is outside the
// repository and itself startable. An interpreter is "env" by resolved
// basename, so a symlinked or hardlinked env delegates the same way. An env
// argument the static parser cannot model (env -S, env -i) denies rather
// than guesses at a different resolution.
func envDelegateAttributable(interpreter, arg string, environ []string, roots []string, depth int) bool {
	if arg == "" {
		return true
	}
	resolved := pathutil.ResolveForCompare(interpreter)
	if filepath.Base(interpreter) != "env" && filepath.Base(resolved) != "env" {
		return true
	}
	invocation, err := envcommand.Parse([]string{arg}, envcommand.Policy{AllowAssignments: true})
	if err != nil || invocation.CommandIndex < 0 {
		return err == nil
	}
	if invocation.ClearEnvironment || invocation.Chdir != "" || len(invocation.Mutations) > 0 {
		return false
	}
	// The kernel delivers the whole shebang remainder as ONE env argument,
	// so the command token is the argument itself: `#!/usr/bin/env node`
	// delegates "node". A multi-word argument env would exec as one
	// impossible filename only if no -S splitting applies — and -S was
	// refused by the parser above.
	return spellingAttributableToOperator(arg, environ, roots, depth+1)
}

// executableFileExists reports whether path names a file the loader could
// execute — an absolute spelling whose target exists, is regular, and passes
// the same mode/access probe a PATH walk applies. A relative interpreter
// name cannot exec (execve answers ENOENT for it), so it reports false.
func executableFileExists(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() &&
		info.Mode().Perm()&0o111 != 0 && executableAccess(path)
}

// startsWithShebang reports whether the file's first two bytes are "#!".
func startsWithShebang(file *os.File) bool {
	var head [2]byte
	if _, err := file.ReadAt(head[:], 0); err != nil {
		return false
	}
	return head[0] == '#' && head[1] == '!'
}

// startsWithELF reports whether the file carries the ELF magic.
func startsWithELF(file *os.File) bool {
	var head [4]byte
	if _, err := file.ReadAt(head[:], 0); err != nil {
		return false
	}
	return head == [4]byte{0x7f, 'E', 'L', 'F'}
}

// shebangInterpreter extracts the interpreter path and its single argument
// from a "#!" script, matching the kernel's binfmt_script parsing: the
// interpreter is everything up to the first space or tab, and the entire
// trimmed remainder — not whitespace-split — is delivered as ONE argument.
func shebangInterpreter(file *os.File) (interpreter, arg string) {
	buf := make([]byte, 256)
	n, err := file.ReadAt(buf, 0)
	if err != nil && n == 0 {
		return "", ""
	}
	line := buf[:n]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	rest := strings.TrimSpace(string(line[2:]))
	i := strings.IndexAny(rest, " \t")
	if i < 0 {
		return rest, ""
	}
	return rest[:i], strings.TrimSpace(rest[i:])
}

// elfInterpreter returns the PT_INTERP dynamic-loader path of an ELF file,
// or "" for a static executable or an unparseable header — both of which
// execve treats without a missing-interpreter ENOENT (the second fails
// ENOEXEC and the shell runs the file itself, so the hit still runs).
func elfInterpreter(file *os.File) string {
	parsed, err := elf.NewFile(file)
	if err != nil {
		return ""
	}
	defer parsed.Close()
	for _, prog := range parsed.Progs {
		if prog.Type != elf.PT_INTERP {
			continue
		}
		buf := make([]byte, prog.Filesz)
		if _, err := prog.ReadAt(buf, 0); err != nil {
			return ""
		}
		return string(bytes.TrimRight(buf, "\x00"))
	}
	return ""
}
