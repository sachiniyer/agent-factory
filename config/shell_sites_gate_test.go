package config

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The drift gate for #3566.
//
// The load-time warning above covers the operator values af hands to a shell
// TODAY. Its whole point is that the set was derived from the CONSUMERS rather
// than from a plausible-looking list of key names — the survey found that
// remote_hooks, which the issue listed, never sees a shell at all, while
// sandbox.ssh, which it did not, does. A derivation is only worth anything if it
// stays derived, so this walks the tree and requires every shell-invocation site
// to be classified. A new consumer then either inherits the warning or fails
// here; it cannot quietly drift past both.
//
// Detection is deliberately WIDER than `exec.Command(…, "sh", "-c", …)`: it is
// every `-c` string literal passed as an argument. The narrow reading has a hole
// exactly where this issue lives — sessionenv/exec_unix.go runs the pane's
// program as []string{shell, "-c", command}, naming the shell through a
// variable, and that IS the site that turns program_overrides into a dash
// invocation. systemdunit's scope constructors and docker's appended argv would
// escape it too. Widening costs three `not-a-shell` entries (tmux's start
// directory, stat's format, ionice's scheduling class) and buys a detector that
// cannot be named around: a new consumer either classifies its site or goes red.
//
// Two limits, stated rather than papered over:
//
//   - Test files are not scanned. A test's shell string is authored by the test,
//     so there is no operator value to miss, and the alternative is a registry of
//     several hundred harness scripts nobody would read.
//   - tmux runs a pane command through /bin/sh itself, with no `-c` anywhere in
//     af's source. No AST scan can see that, which is the reason
//     program_overrides is warned about on its own merits rather than because
//     some site here happens to name it.

// shellSiteClass says what kind of string reaches the `-c` at a site.
type shellSiteClass string

const (
	// afAuthored: af composes the whole string. An operator value may be
	// EMBEDDED (a program name inside a docker script), but it arrived through a
	// path already covered elsewhere in this table.
	afAuthored shellSiteClass = "af-authored"
	// operatorConfig: a config value reaches the shell verbatim. rule.key must
	// name the config key, and that key must be one warnExecSeparator inspects.
	operatorConfig shellSiteClass = "operator-config"
	// operatorOther: operator-authored, but not from a config file — so the
	// config-load warning cannot reach it. rule.note must say where it comes
	// from and why it is out of the warning's reach.
	operatorOther shellSiteClass = "operator-not-config"
	// notAShell: the `-c` is some other program's flag.
	notAShell shellSiteClass = "not-a-shell"
)

// classifiedSite is one `-c` site's verdict. A function may hold several with
// DIFFERENT verdicts — prepareAccountUser asks `stat -c` a question and then
// runs a script through `sh -c` — so the classification is per site, in source
// order, not per function.
type classifiedSite struct {
	class shellSiteClass
	// key is the config key for an operatorConfig site. It must be one
	// warnExecSeparator inspects, which TestShellSites_OperatorConfigSitesNameAWarnedKey
	// checks against warnedShellValueKeys.
	key string
	// note explains the verdict for every other class.
	note string
}

// shellSiteRegistry classifies every `-c` site in the non-test tree, keyed by
// "<repo-relative file>:<enclosing function>" and listing that function's sites
// in source order. The list length is what stops a NEW site added inside an
// already-classified function from inheriting its neighbour's verdict.
//
// Line numbers are deliberately NOT part of the key. The survey this gate came
// from had already gone stale on line numbers by the time it was implemented.
var shellSiteRegistry = map[string][]classifiedSite{
	// ---- operator config values: covered by warnExecSeparator ----
	"internal/sessionenv/exec_unix.go:execInvocationMode": {{
		class: operatorConfig, key: "program_overrides",
		note: "the pane's program, run as `/bin/sh -c <command>` immediately before exec. This is the site " +
			"#3566 is about: an unscoped session never reaches the account boundary, so an `exec --` prefix " +
			"here dies with exit 127 and no explanation.",
	}},
	"session/git/hooks.go:runPostWorktreeHooks": {
		{class: operatorConfig, key: "post_worktree_commands", note: "the plain child"},
		{class: operatorConfig, key: "post_worktree_commands", note: "the same command inside a transient systemd scope"},
	},
	"daemon/archive_hook.go:runOnArchiveHook": {{
		class: operatorConfig, key: "on_archive_command",
		note: "the resolved archive hook, in its own transient scope",
	}},
	"session/backend_sandbox.go:(*sandboxProvisioner).buildRunCommand": {{
		class: operatorConfig, key: "sandbox.ssh",
		note: "the operator's ssh command as a PREFIX: `sh -c '<sandbox.ssh> \"$@\"' af-sandbox <script>`",
	}},
	"session/backend_sandbox.go:(*sandboxProvisioner).startTunnel": {{
		class: operatorConfig, key: "sandbox.ssh",
		note: "the same prefix, with -N -L appended for the port-forward child",
	}},

	// ---- operator-authored, but not from a config file ----
	"daemon/watcher.go:(*taskWatcher).runOnce": {{
		class: operatorOther,
		note: "a watch task's command, from the task store rather than a config file, so no config load ever " +
			"sees it. Out of #3566's scope on purpose — the fix is a warning where the value is ADMITTED, " +
			"which for a task is task creation, not config load.",
	}},

	// ---- af-authored scripts ----
	"config/claude_probe.go:probeClaudeCommand": {
		{class: afAuthored, note: "zsh probe: source ~/.zshrc, then `which claude`"},
		{class: afAuthored, note: "bash probe: -i so aliases are defined, then `type claude`"},
		{class: afAuthored, note: "fallback probe: `which claude`"},
	},
	"daemon/vscode_start_gate.go:newGatedVSCodeCommand": {{
		class: afAuthored,
		note: "vscodeStartGateScript, a const in the same file; the binary and its args arrive as \"$@\" " +
			"positional parameters, never spliced into the script text",
	}},
	"session/backend_docker.go:(*dockerProvisioner).startAgentServer": {{
		class: afAuthored,
		note: "af's own `af agent-server …` line. The operator's program is embedded, but shell-QUOTED as a " +
			"--program argument; it reaches a shell only later, in the container, through the pane path above.",
	}},
	"session/backend_docker.go:(*dockerProvisioner).execSh": {{
		class: afAuthored, note: "af's container setup scripts (chmod, mkdir), composed with shellQuote",
	}},
	"session/backend_docker_account.go:(*dockerProvisioner).execSessionSh": {{
		class: afAuthored, note: "the same, run as the session's container user",
	}},
	"session/backend_docker_account.go:(*dockerProvisioner).prepareAccountUser": {
		{class: notAShell, note: "`stat -c %u:%g` — stat's format flag"},
		{class: afAuthored, note: "af's mkdir/chown script for the account-owned home"},
	},

	// ---- not a shell at all ----
	"session/tmux/start.go:(*TmuxSession).Start": {{
		class: notAShell, note: "`tmux new-session -c <dir>` — tmux's start-directory flag",
	}},
	"internal/sessionenv/account_environment_builtins.go:unwrapIonice": {{
		class: notAShell, note: "strings.HasPrefix(option, \"-c\") — ionice's scheduling-class flag, matched as text",
	}},
}

func TestShellSites_EveryShellSiteIsClassified(t *testing.T) {
	root := repoRootForShellSites(t)
	sites, err := scanShellSites(root)
	require.NoError(t, err)
	require.NotEmpty(t, sites, "the scanner found nothing; it has stopped looking rather than found a clean tree")

	unknown, mismatched, stale := classifyShellSites(sites, shellSiteRegistry)
	assert.Empty(t, unknown, "unclassified shell-invocation site(s): add each to shellSiteRegistry, "+
		"and if the site hands an OPERATOR value to the shell, wire that value into warnExecSeparator first")
	assert.Empty(t, mismatched, "a classified function's site count changed: re-read the function and update its rule")
	assert.Empty(t, stale, "shellSiteRegistry entries no longer match any site; delete them")
}

func TestShellSites_OperatorConfigSitesNameAWarnedKey(t *testing.T) {
	warned := map[string]bool{}
	for _, key := range warnedShellValueKeys {
		warned[key] = true
	}
	claimed := map[string]bool{}
	for site, rules := range shellSiteRegistry {
		for i, rule := range rules {
			switch rule.class {
			case operatorConfig:
				require.NotEmptyf(t, rule.key, "%s site %d carries a config value but names no key", site, i)
				assert.Truef(t, warned[rule.key],
					"%s hands %s to a shell, but %s is not in warnedShellValueKeys — the site would be vouched "+
						"for by a warning that does not inspect it", site, rule.key, rule.key)
				claimed[rule.key] = true
			default:
				assert.NotEmptyf(t, rule.note, "%s site %d must say why it is %s", site, i, rule.class)
			}
		}
	}
	for _, key := range warnedShellValueKeys {
		assert.Truef(t, claimed[key],
			"%s is warned about but no shell site claims it; either the consumer went away and the key should "+
				"go with it, or the gate has lost sight of the consumer", key)
	}
}

// TestShellSites_GateFailsOnAnUnclassifiedSite is the gate's own red. A gate
// that only ever runs against a tree it already passes on proves nothing about
// what it would do when a consumer is added.
func TestShellSites_GateFailsOnAnUnclassifiedSite(t *testing.T) {
	tree := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tree, "newconsumer"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "newconsumer", "run.go"), []byte(`package newconsumer

import "os/exec"

func RunOperatorCommand(command string) error {
	return exec.Command("sh", "-c", command).Run()
}
`), 0o644))

	sites, err := scanShellSites(tree)
	require.NoError(t, err)
	require.Len(t, sites, 1, "the scanner must see the new consumer")
	assert.Equal(t, "newconsumer/run.go", sites[0].File)
	assert.Equal(t, "RunOperatorCommand", sites[0].Func)

	unknown, _, _ := classifyShellSites(sites, shellSiteRegistry)
	require.Len(t, unknown, 1, "an unregistered consumer must be reported, not inherited")
	assert.Contains(t, unknown[0], "newconsumer/run.go:RunOperatorCommand")
}

// TestShellSites_GateFailsOnASecondSiteInAClassifiedFunction covers the way a
// function-keyed registry could be fooled: adding a shell call BESIDE one that
// is already classified. The count is what closes it.
func TestShellSites_GateFailsOnASecondSiteInAClassifiedFunction(t *testing.T) {
	tree := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tree, "run.go"), []byte(`package run

import "os/exec"

func Run(afScript, operatorValue string) {
	_ = exec.Command("sh", "-c", afScript)
	_ = exec.Command("sh", "-c", operatorValue)
}
`), 0o644))

	sites, err := scanShellSites(tree)
	require.NoError(t, err)
	require.Len(t, sites, 2)

	registry := map[string][]classifiedSite{
		"run.go:Run":  {{class: afAuthored, note: "af's own script"}},
		"gone.go:Old": {{class: afAuthored, note: "a consumer that has since been deleted"}},
	}
	unknown, mismatched, stale := classifyShellSites(sites, registry)
	assert.Empty(t, unknown)
	require.Len(t, mismatched, 1, "a second site under an existing key must not inherit its classification")
	assert.Contains(t, mismatched[0], "run.go:Run")
	assert.Contains(t, mismatched[0], "2")
	// The other direction: a rule for a site that no longer exists must be
	// reported too, so the registry cannot rot into a list of claims about code
	// that is gone.
	require.Equal(t, []string{"gone.go:Old"}, stale)
}

// shellSite is one place a `-c` string literal is passed as an argument.
type shellSite struct {
	File string // repo-relative, slash-separated
	Func string // enclosing function or method
	Line int
}

func (s shellSite) key() string { return s.File + ":" + s.Func }

// scanShellSites parses every non-test Go file under root and returns the sites
// in file/line order.
func scanShellSites(root string) ([]shellSite, error) {
	var sites []shellSite
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// node_modules and vendor hold third-party code af does not run;
			// testdata deliberately holds files that need not even parse. Nothing
			// else is skipped — web/embed.go is Go and is scanned like any other.
			case ".git", "vendor", "node_modules", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", rel, parseErr)
		}
		sites = append(sites, shellSitesInFile(fset, file, filepath.ToSlash(rel))...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})
	return sites, nil
}

// shellSitesInFile reports every `-c` string literal that appears in a call's
// argument list or in a slice/array literal — the two ways an argv is written.
// A `-c` compared in a switch or an if is not an invocation and is not reported.
func shellSitesInFile(fset *token.FileSet, file *ast.File, rel string) []shellSite {
	var sites []shellSite
	collect := func(node ast.Node, funcName string) {
		ast.Inspect(node, func(n ast.Node) bool {
			var exprs []ast.Expr
			switch typed := n.(type) {
			case *ast.CallExpr:
				exprs = typed.Args
			case *ast.CompositeLit:
				exprs = typed.Elts
			default:
				return true
			}
			for _, expr := range exprs {
				if isDashC(expr) {
					sites = append(sites, shellSite{File: rel, Func: funcName, Line: fset.Position(expr.Pos()).Line})
				}
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			collect(fn, funcDeclName(fn))
			continue
		}
		// A package-level var may still hold an argv literal.
		collect(decl, "<file scope>")
	}
	return sites
}

func funcDeclName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return "(" + exprString(fn.Recv.List[0].Type) + ")." + fn.Name.Name
}

func exprString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + exprString(e.X)
	case *ast.IndexExpr:
		return exprString(e.X)
	case *ast.IndexListExpr:
		return exprString(e.X)
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	}
	return "?"
}

func isDashC(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(lit.Value)
	return err == nil && value == "-c"
}

// classifyShellSites compares scanned sites against a registry and returns the
// three ways they can disagree: sites with no rule, rules whose site count no
// longer matches, and rules that match nothing at all.
func classifyShellSites(sites []shellSite, registry map[string][]classifiedSite) (unknown, mismatched, stale []string) {
	lines := map[string][]int{}
	var order []string
	for _, site := range sites {
		if _, seen := lines[site.key()]; !seen {
			order = append(order, site.key())
		}
		lines[site.key()] = append(lines[site.key()], site.Line)
	}
	sort.Strings(order)
	for _, key := range order {
		rules, ok := registry[key]
		if !ok {
			unknown = append(unknown, fmt.Sprintf("%s (lines %v)", key, lines[key]))
			continue
		}
		if len(rules) != len(lines[key]) {
			mismatched = append(mismatched, fmt.Sprintf("%s: registry classifies %d site(s), tree has %d (lines %v)",
				key, len(rules), len(lines[key]), lines[key]))
		}
	}
	registryKeys := make([]string, 0, len(registry))
	for key := range registry {
		registryKeys = append(registryKeys, key)
	}
	sort.Strings(registryKeys)
	for _, key := range registryKeys {
		if len(lines[key]) == 0 {
			stale = append(stale, key)
		}
	}
	return unknown, mismatched, stale
}

// repoRootForShellSites walks up to the repository root. It SKIPS rather than
// fails when the tree is not there, matching repoRootForDocs: the package must
// stay testable from a module cache, where there is no tree to walk.
func repoRootForShellSites(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if info, err := os.Stat(filepath.Join(dir, "session")); err == nil && info.IsDir() {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("the repository tree is not present in this checkout; the shell-site drift gate needs it")
		}
		dir = parent
	}
}
