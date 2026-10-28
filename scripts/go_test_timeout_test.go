package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ── the go test timeout lint ─────────────────────────────────────────────────
//
// `go test` without -timeout gets Go's default of 10m PER PACKAGE, and the
// daemon package sits close enough to that wall that a slow runner crosses it:
// run 35205849014 panicked at 600.014s on a post-merge Build cell whose three
// siblings finished the same package in ~400s. A package timeout names no slow
// test and marks everything after it as not run, so it reads as breakage.
//
// #4463 gave one release lane an explicit budget and left three siblings on the
// default. This lint is derived from the files themselves — every workflow,
// every script under scripts/, the Makefile — so a new lane cannot reintroduce
// the default without failing here. It flags a `go test` that can run many
// packages: one naming a `...` pattern, or one forwarding its caller's
// arguments (which is how scripts/container/run-tests.sh runs `./...`).
// A single named package keeps the default; its budget is a per-package
// question the default answers well enough.

// goTestInvocation is one `go test` command: where it starts and the words
// that follow `test` up to the end of that shell command.
type goTestInvocation struct {
	file string
	line int
	args []string
}

// passThroughArgs are the shell spellings that forward a caller's arguments —
// and so its package patterns — into `go test`.
var passThroughArgs = []string{`"$@"`, `$@`, `${@}`, `"${@}"`, `$*`, `"$*"`}

// makeVarRef matches a Makefile variable reference in a recipe. Its value is
// not visible here, so like "$@" it may carry any package pattern.
var makeVarRef = regexp.MustCompile(`\$\(([A-Za-z_][A-Za-z0-9_]*)\)`)

const makeVarToken = "MAKEVAR:"

func (g goTestInvocation) runsManyPackages() bool {
	for _, arg := range g.args {
		if strings.HasPrefix(arg, makeVarToken) {
			return true
		}
		for _, pass := range passThroughArgs {
			if arg == pass {
				return true
			}
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if strings.Contains(strings.Trim(arg, `"'`), "...") {
			return true
		}
	}
	return false
}

func (g goTestInvocation) hasTimeout() bool {
	for _, arg := range g.args {
		name := strings.TrimLeft(strings.Trim(arg, `"'`), "-")
		if name == "timeout" || strings.HasPrefix(name, "timeout=") ||
			name == "test.timeout" || strings.HasPrefix(name, "test.timeout=") {
			return true
		}
	}
	return false
}

// logicalLines joins backslash continuations and drops comment lines, keeping
// the number of the line each logical line starts on. A `#` line is inert in
// YAML, in a `run:` block, in a shell script and in a Makefile recipe; the
// call sites explain their budgets in prose that mentions `go test ./...`.
func logicalLines(content string) (lines []string, starts []int) {
	var cur strings.Builder
	start := 0
	for i, raw := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(raw)
		if cur.Len() == 0 {
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			start = i + 1
		}
		if strings.HasSuffix(trimmed, `\`) {
			cur.WriteString(strings.TrimSuffix(trimmed, `\`))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(trimmed)
		lines = append(lines, cur.String())
		starts = append(starts, start)
		cur.Reset()
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
		starts = append(starts, start)
	}
	return lines, starts
}

// shellSeparators end one command and begin the next.
var shellSeparators = map[string]bool{";": true, "&&": true, "||": true, "|": true, "&": true, "then": true, "do": true, "else": true}

// goTestInvocations finds every `go test` command in content. Tokens are split
// on whitespace with the separators spaced out first, so `a && go test ./...`
// and `x=$(go test ./... )` both expose the command; a trailing ` #` comment
// ends the line.
func goTestInvocations(file, content string) []goTestInvocation {
	lines, starts := logicalLines(content)
	var found []goTestInvocation
	for idx, line := range lines {
		if cut := strings.Index(line, " #"); cut >= 0 {
			line = line[:cut]
		}
		// Protect $(VAR) before "$(" is treated as command substitution.
		line = makeVarRef.ReplaceAllString(line, makeVarToken+"$1")
		for _, sep := range []string{"&&", "||", ";", "$(", "`", "(", ")"} {
			line = strings.ReplaceAll(line, sep, " "+strings.Trim(sep, "$(")+" ")
		}
		tokens := strings.Fields(line)
		for i := 0; i+1 < len(tokens); i++ {
			if strings.Trim(tokens[i], `"'`) != "go" || strings.Trim(tokens[i+1], `"'`) != "test" {
				continue
			}
			inv := goTestInvocation{file: file, line: starts[idx]}
			for _, tok := range tokens[i+2:] {
				if shellSeparators[tok] || tok == ")" || tok == "`" {
					break
				}
				inv.args = append(inv.args, tok)
			}
			found = append(found, inv)
		}
	}
	return found
}

// lintGoTestTimeouts reports every many-package `go test` with no -timeout.
func lintGoTestTimeouts(files map[string]string) []string {
	var findings []string
	for name, content := range files {
		for _, inv := range goTestInvocations(name, content) {
			if inv.runsManyPackages() && !inv.hasTimeout() {
				findings = append(findings, inv.file+":"+strconv.Itoa(inv.line)+
					": `go test "+strings.Join(inv.args, " ")+"` runs many packages with no -timeout;"+
					" Go's default is 10m per package, which the daemon package has already crossed on a"+
					" slow runner — give it an explicit, measured budget")
			}
		}
	}
	return findings
}

func TestGoTestTimeoutLintDetector(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          int
	}{
		{"bare whole tree", "run: go test -v ./...", 1},
		{"explicit equals form", "run: go test -v -timeout=20m ./...", 0},
		{"explicit spaced form", "go test -v -race -timeout 20m ./...", 0},
		{"test-binary spelling", "go test ./... -test.timeout=5m", 0},
		{"subtree pattern", "go test -race ./daemon/...", 1},
		{"single package keeps the default", "run: go test ./internal/designtokens -run X -count=1", 0},
		{"list probe of one package", `matched="$(go test ./integration -list "$pin" | grep -c x)"`, 0},
		{"after a separator", "gofmt -l . && go test -short -v ./...", 1},
		{"inside command substitution", "out=$(go test ./... 2>&1)", 1},
		{"continued across lines", "go test -count=1 \\\n  -v \\\n  ./...", 1},
		{"continued with a timeout", "go test -count=1 \\\n  -timeout=30m \\\n  ./...", 0},
		{"forwards its caller's patterns", `exec go test -count=1 -buildvcs=false "$@"`, 1},
		{"forwards with a default budget", `exec go test -count=1 -timeout=30m "$@"`, 0},
		{"make variable pass-through", "\tgo test $(GOTESTARGS)", 1},
		{"comment line", "# a lane that runs `go test ./...` needs zsh", 0},
		{"trailing comment", "go test ./session/tmux # not ./...", 0},
		{"go vet is not go test", "go vet ./...", 0},
		{"deadcode -test is not go test", `out=$("$gopath/bin/deadcode" -test ./... 2>&1)`, 0},
		{"two invocations on one line", "go test ./... ; go test -timeout=1m ./...", 1},
		{"package list from go list", "go test $(go list ./... | grep -vE '/(daemon|app)')", 1},
		{"package list with a budget", "go test -timeout=10m $(go list ./... | grep -v x)", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lintGoTestTimeouts(map[string]string{"f": tc.content})
			if len(got) != tc.want {
				t.Fatalf("findings = %d, want %d:\n  %s", len(got), tc.want, strings.Join(got, "\n  "))
			}
		})
	}
}

// TestGoTestInvocationsCarryATimeout is the lint itself, over the real files.
func TestGoTestInvocationsCarryATimeout(t *testing.T) {
	root := repoRoot(t)
	files := map[string]string{}
	add := func(path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		files[rel] = string(data)
	}
	walk := func(dir string, keep func(string) bool) {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && keep(path) {
				add(path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	walk(".github", func(p string) bool {
		return strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".sh")
	})
	walk("scripts", func(p string) bool { return strings.HasSuffix(p, ".sh") })
	add(filepath.Join(root, "Makefile"))

	// Anti-vacuity: a lint that read the wrong files, or whose detector stopped
	// recognising the commands, would also report nothing. Every file below runs
	// a many-package `go test` today; if one stops, update this list with it.
	manyPackage := map[string]int{}
	for name, content := range files {
		for _, inv := range goTestInvocations(name, content) {
			if inv.runsManyPackages() {
				manyPackage[name]++
			}
		}
	}
	for _, must := range []string{
		filepath.Join(".github", "workflows", "build.yml"),
		filepath.Join(".github", "workflows", "pr.yml"),
		filepath.Join(".github", "workflows", "auto-release.yml"),
		filepath.Join(".github", "workflows", "stable-release.yml"),
		filepath.Join("scripts", "container", "run-tests.sh"),
	} {
		if manyPackage[must] == 0 {
			t.Errorf("found no many-package `go test` in %s — the lint is not seeing what it guards", must)
		}
	}

	if findings := lintGoTestTimeouts(files); len(findings) > 0 {
		t.Fatalf("many-package `go test` invocations rely on Go's 10m default:\n  %s",
			strings.Join(findings, "\n  "))
	}
}
