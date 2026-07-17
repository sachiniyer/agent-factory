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

// cliVerb is one invocable cobra command and its own (non-inherited) flags.
type cliVerb struct {
	Path   string   // full invocation, e.g. "af sessions create"
	Flags  []string // sorted long flag names registered on this command
	Hidden bool
}

// deriveCLI walks the real cobra tree. A command with subcommands but no RunE
// is a grouping node (e.g. `af sessions`), not a capability, so it is skipped —
// but its children are still walked.
func deriveCLI(t *testing.T) map[string]cliVerb {
	t.Helper()
	root := commands.NewRootCommand(commands.Options{Version: "0.0.0-parity"})
	out := map[string]cliVerb{}

	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		runnable := c.RunE != nil || c.Run != nil
		if runnable {
			var flags []string
			c.Flags().VisitAll(func(f *pflag.Flag) {
				flags = append(flags, f.Name)
			})
			sort.Strings(flags)
			out[path] = cliVerb{Path: path, Flags: flags, Hidden: c.Hidden}
		}
		for _, sub := range c.Commands() {
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(root, "af")
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

// webAPISource is where the af() chokepoint itself lives; readWebAPI still reads
// it for the request-body parse, which is keyed to that file's call shape.
const webAPISource = "web/src/api.ts"

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

func readWebAPI(t *testing.T) string {
	t.Helper()
	p := filepath.Join(repoRoot(t), webAPISource)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
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
	src := readWebAPI(t)
	seen := map[string]bool{}
	needle := `"` + method + `"`
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
