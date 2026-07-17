package parity

// Surface derivation: every function here reads a REAL surface out of the
// running code (or, for the web, out of its single RPC chokepoint) so the
// inventory can never quietly disagree with what af actually ships.
//
// Nothing here dials a socket, spawns a daemon, or touches AGENT_FACTORY_HOME:
// daemon.HTTPRoutes, keys.EffectiveBindings and commands.NewRootCommand are all
// pure table reads, which is what lets this package run on a shared dev box.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/commands"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// repoRoot resolves the repository root from this package's directory so the
// web-source parse works regardless of the test's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(wd)
}

// --- CLI -------------------------------------------------------------------

// cliVerb is one cobra command and the flags DECLARED on it (its own locals plus
// any persistent flags it declares for its children — not flags inherited from a
// parent, which belong to that parent's entry).
type cliVerb struct {
	Path   string   // full invocation, e.g. "af sessions create"
	Flags  []string // sorted long flag names declared at this command
	Hidden bool
	// Use is cobra's usage line ("send-prompt <title> <prompt>"), which is where
	// the POSITIONAL arguments are declared — the other half of a verb's
	// argument shape, invisible in the flag set.
	Use string
	// Runnable distinguishes an invocable verb from a grouping node like
	// `af sessions`, which is not a capability itself but can declare flags.
	Runnable bool
}

// deriveCLI walks the real cobra tree. A command with subcommands but no RunE is
// a grouping node (e.g. `af sessions`), not an invocable verb — but it can still
// DECLARE capabilities as persistent flags, so its flags are recorded even though
// it is not a verb.
//
// Flags are attributed to the command that DECLARES them, via LocalFlags():
// cobra's Flags() omits a parent's persistent flags, so walking only runnable
// leaves would lose `--repo`/`--json` (declared on `af sessions`, api/api.go:572-581)
// entirely — they show under `af sessions create --help` as Global Flags but
// belong to the group. Attributing at the declaration site also keeps one flag as
// one ledger entry instead of repeating it under every child.
func deriveCLI(t *testing.T) map[string]cliVerb {
	t.Helper()
	root := initCobraDefaults(commands.NewRootCommand(commands.Options{Version: "0.0.0-parity"}))
	out := map[string]cliVerb{}

	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		var flags []string
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			flags = append(flags, f.Name)
		})
		sort.Strings(flags)
		out[path] = cliVerb{
			Path:     path,
			Flags:    flags,
			Hidden:   c.Hidden,
			Use:      c.Use,
			Runnable: c.RunE != nil || c.Run != nil,
		}
		for _, sub := range c.Commands() {
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(root, "af")
	return out
}

// initCobraDefaults finishes building the command tree the way running the
// binary would, BEFORE anything walks it.
//
// Cobra adds `completion` (and its per-shell subcommands), `help`, `--help` on
// every command, and `--version` on root LAZILY — inside Execute(), not when the
// tree is constructed. A walk of the freshly-built tree therefore misses real,
// user-visible surface: `af completion bash` and `af help` are commands a user
// can run, and `af version --help` is a flag they can pass.
//
// That omission is not a cosmetic gap. This package's whole claim is that the
// inventory cannot quietly disagree with what af ships, and for those commands
// it silently did — the derivation reported green while the surface existed.
// Deriving after init is what makes the claim true rather than nearly true.
func initCobraDefaults(root *cobra.Command) *cobra.Command {
	// Order matters: these add SUBCOMMANDS, so they run before the per-command
	// flag init below walks the tree.
	root.InitDefaultCompletionCmd()
	root.InitDefaultHelpCmd()

	var initFlags func(c *cobra.Command)
	initFlags = func(c *cobra.Command) {
		c.InitDefaultHelpFlag()
		// Only adds --version where Version is set (root), matching Execute().
		c.InitDefaultVersionFlag()
		for _, sub := range c.Commands() {
			initFlags(sub)
		}
	}
	initFlags(root)
	return root
}

// cliVerbs returns just the invocable commands — the verb-level view.
func cliVerbs(t *testing.T) map[string]cliVerb {
	t.Helper()
	out := map[string]cliVerb{}
	for path, v := range deriveCLI(t) {
		if v.Runnable {
			out[path] = v
		}
	}
	return out
}

// --- Daemon route catalog --------------------------------------------------

// route is one HTTP/JSON route of the daemon's PUBLIC catalog, with the request
// body fields reflected off the real wire struct.
type route struct {
	ID     string // "POST /v1/CreateSession"
	Fields []string
}

func deriveRoutes(t *testing.T) map[string]route {
	t.Helper()
	out := map[string]route{}
	for _, r := range daemon.HTTPRoutes() {
		id := r.Method + " " + r.Path
		out[id] = route{ID: id, Fields: append([]string(nil), r.RequestFields...)}
	}
	return out
}

// --- TUI -------------------------------------------------------------------

// binding is one row of the TUI's canonical key table.
type binding struct {
	ID   string // the [keys] config name, or "fixed:<desc>" for fixed bindings
	Keys []string
	Desc string
}

// deriveTUI reads the real binding table. Rebindable actions are identified by
// their stable [keys] config name; fixed bindings (which config cannot touch)
// have no such name, so they are identified by description — a desc change
// there is itself a capability change worth re-reviewing.
func deriveTUI(t *testing.T) map[string]binding {
	t.Helper()
	infos, err := keys.EffectiveBindings(nil)
	if err != nil {
		t.Fatalf("EffectiveBindings: %v", err)
	}
	out := map[string]binding{}
	for _, b := range infos {
		id := b.Action
		if id == "" {
			id = "fixed:" + b.Desc
		}
		out[id] = binding{ID: id, Keys: b.Keys, Desc: b.Desc}
	}
	return out
}

// --- Web -------------------------------------------------------------------

// Every web control-plane call funnels through the one af<T>(method, body,
// token) helper in web/src/api.ts, which POSTs /v1/<method>. That chokepoint is
// what makes a static read of the call sites reliable: there is no other way
// for the SPA to reach the daemon's JSON API.
//
// Matches both `af<T>("Name", {...})` and the un-generic `af("Name", {...})`,
// with the literal on the same line or the next.
var webCallRe = regexp.MustCompile(`(?s)\baf(?:<[^>]*>)?\(\s*"([A-Za-z0-9_]+)"\s*,`)

// webSrcDir is scanned WHOLE rather than just api.ts: af<T>() is exported
// (web/src/api.ts:130), so any module can import it and reach a new daemon RPC
// directly. Parsing only api.ts would leave that call invisible while the
// existing call count kept minWebCalls satisfied — a silent blind spot rather
// than a loud failure.
const webSrcDir = "web/src"

// minWebCalls guards against a vacuous pass: if api.ts is restructured such
// that the regex stops matching, the parity test must fail loudly instead of
// concluding "the web calls nothing".
const minWebCalls = 10

// deriveWebRPCs returns the set of daemon RPC method names the web client can
// reach, read from its call sites across the whole of web/src — not just
// api.ts, since af() is exported and any module may call it.
func deriveWebRPCs(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, path := range webSourceFiles(t) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range webCallRe.FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
	}
	if len(out) < minWebCalls {
		t.Fatalf("web RPC parse found only %d call sites under %s (expected >= %d): the af() "+
			"chokepoint or its call shape changed, so this parser is now blind. Fix "+
			"webCallRe in derive_test.go rather than lowering minWebCalls.",
			len(out), webSrcDir, minWebCalls)
	}
	return out
}

// webSourceFiles lists the web client's non-test TypeScript sources. Tests are
// excluded: a mock or fixture naming an RPC is not the client reaching it.
func webSourceFiles(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), webSrcDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".test.ts") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out
}

// webCallBody returns the JSON keys the web can set on one RPC, unioned across
// EVERY call site for that method. The union matters: CreateTab has two sites
// (a shell tab and a VS Code tab, web/src/api.ts:339 and :355) and only the
// second sets `kind`, so reading just the first call site would wrongly report
// `kind` as unreachable from the web.
//
// Used to audit the OPTION dimension — which fields of a request a surface can
// actually populate — not just verb reachability.
func webCallBody(t *testing.T, method string) []string {
	t.Helper()
	seen := map[string]bool{}
	needle := `"` + method + `"`
	// Scan every web source, not just api.ts: af<T>() is exported, so a module
	// that calls it directly and sets a field (say CreateSession with `backend`)
	// would otherwise be invisible here — and the inventory would keep reporting
	// a gap that had actually been fixed.
	for _, path := range webSourceFiles(t) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := string(b)
		for off := 0; ; {
			i := strings.Index(src[off:], needle)
			if i < 0 {
				break
			}
			at := off + i
			off = at + len(needle)
			for _, f := range objectLiteralAfter(src[at:]) {
				seen[f] = true
			}
		}
	}
	var out []string
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// objectLiteralAfter returns the top-level keys of the first object literal that
// follows s's start.
func objectLiteralAfter(rest string) []string {
	open := strings.Index(rest, "{")
	if open < 0 {
		return nil
	}
	depth, end := 0, -1
	for i := open; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
				break
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil
	}
	return objectKeys(rest[open : end+1])
}

// leadingIdentRe pulls the property name off one object-literal entry, covering
// both `key: value` and the ES6 shorthand `key` (which the web uses for
// id/title/task) — a colon-only parse silently misses the shorthand and would
// under-report what a surface actually sends.
var leadingIdentRe = regexp.MustCompile(`^\s*([A-Za-z_]\w*)\s*(:|$)`)

// objectKeys returns the top-level property names of a JS/TS object literal.
// Only depth-1 commas separate entries, so nested objects/arrays and any commas
// inside strings must not split a segment.
func objectKeys(lit string) []string {
	inner := strings.TrimSpace(lit)
	inner = strings.TrimPrefix(inner, "{")
	inner = strings.TrimSuffix(inner, "}")

	var fields []string
	depth := 0
	var quote byte
	start := 0
	flush := func(seg string) {
		if m := leadingIdentRe.FindStringSubmatch(seg); m != nil {
			fields = append(fields, m[1])
		}
	}
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
		case ',':
			if depth == 0 {
				flush(inner[start:i])
				start = i + 1
			}
		}
	}
	flush(inner[start:])
	sort.Strings(fields)
	return fields
}

// --- Web TypeScript payload types --------------------------------------------

// webTSTypes maps a Go nested payload type to the TypeScript interface that is
// the web's contract for it. For a wrapper RPC the web sends the payload whole
// (`{ task }`, `{ id, update }`), so the object-literal parse sees only the
// wrapper key and every option inside would look covered. The TS interface IS
// the web's reachable set for those payloads — and it is derived, not asserted,
// which is what catches TaskUpdate omitting project_path (#1935) mechanically
// rather than by someone noticing.
var webTSTypes = map[string]string{
	"task.Task":       "TaskData",
	"task.TaskUpdate": "TaskUpdate",
}

var tsInterfaceFieldRe = regexp.MustCompile(`(?m)^\s*([A-Za-z_]\w*)\??\s*:`)

// webTSInterfaceFields returns the property names of a TypeScript interface in
// web/src/types.ts.
func webTSInterfaceFields(t *testing.T, name string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "web/src/types.ts"))
	if err != nil {
		t.Fatalf("read types.ts: %v", err)
	}
	src := string(b)
	head := "export interface " + name + " {"
	i := strings.Index(src, head)
	if i < 0 {
		t.Fatalf("web/src/types.ts has no `%s` — webTSTypes in derive_test.go is stale.", head)
	}
	rest := src[i+len(head):]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("unterminated interface %s in web/src/types.ts", name)
	}
	out := map[string]bool{}
	for _, m := range tsInterfaceFieldRe.FindAllStringSubmatch(rest[:end], -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatalf("parsed zero fields from interface %s — the parser is blind", name)
	}
	return out
}
