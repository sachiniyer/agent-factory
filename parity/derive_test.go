package parity

// Surface derivation: every function here reads a REAL surface out of the
// running code (or, for the web, out of its single RPC chokepoint) so the
// inventory can never quietly disagree with what af actually ships.
//
// Nothing here dials a socket, spawns a daemon, or touches AGENT_FACTORY_HOME:
// daemon.HTTPRoutes, keys.EffectiveBindings and commands.NewRootCommand are all
// pure table reads, which is what lets this package run on a shared dev box.

import (
	"fmt"
	"io/fs"
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

// webSourceFiles lists the web client's non-test TypeScript sources, RECURSIVELY.
//
// Tests are excluded: a mock or fixture naming an RPC is not the client reaching
// it. Subdirectories are NOT excluded — a flat read would silently skip any
// module moved into one, and the audit would keep reporting parity over code it
// never opened. web/src happens to be flat today (web/src/icons holds only image
// assets), so this changes nothing right now and prevents everything later.
func webSourceFiles(t *testing.T) []string {
	t.Helper()
	root := filepath.Join(repoRoot(t), webSrcDir)
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".test.ts") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// webCallBodyChecked returns the JSON keys the web can set on one RPC, unioned
// across EVERY call site for that method, plus a fail-closed report of call sites
// whose body it could not read. The union matters: CreateTab has two sites (a
// shell tab and a VS Code tab) and only the second sets `kind`, so reading just
// the first would wrongly report `kind` as unreachable.
//
// It handles TWO shapes, because #1968 introduced the second and the old parser
// went blind on it — reading `af("CreateSession", body, token)` as "sends
// nothing", which is under-coverage, the exact failure this package exists to
// prevent:
//
//	af("Method", { a: 1, b: 2 }, token)   // inline literal
//	const body = { a: 1 }; body.b = x;    // a body built as a variable, then
//	af("Method", body, token)             //   the variable passed
//
// Anything at the body position that is neither an inline literal nor a
// resolvable local variable goes into unanalyzable, never silently dropped — a
// non-empty list means the coverage for this RPC is UNVERIFIED, not complete.
func webCallBodyChecked(t *testing.T, method string) (fields []string, unanalyzable []string) {
	t.Helper()
	seen := map[string]bool{}
	callRe := regexp.MustCompile(`\baf(?:<[^>]*>)?\(\s*"` + regexp.QuoteMeta(method) + `"\s*,`)
	identRe := regexp.MustCompile(`^[A-Za-z_]\w*$`)

	for _, path := range webSourceFiles(t) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := string(b)
		for _, loc := range callRe.FindAllStringIndex(src, -1) {
			callPos, argStart := loc[0], loc[1]
			arg := strings.TrimSpace(readOneArg(src[argStart:]))
			switch {
			case strings.HasPrefix(arg, "{"):
				for _, f := range objectKeys(balancedFrom(arg)) {
					seen[f] = true
				}
			case identRe.MatchString(arg):
				keys, ok := resolveWebBodyVar(src, callPos, arg)
				if !ok {
					unanalyzable = append(unanalyzable,
						fmt.Sprintf("%s(%s) in %s: body variable not resolvable to a literal",
							method, arg, relSite(t, path)))
					continue
				}
				for f := range keys {
					seen[f] = true
				}
			default:
				unanalyzable = append(unanalyzable,
					fmt.Sprintf("%s(%s…) in %s: body is neither an inline literal nor a plain variable",
						method, truncate(arg, 24), relSite(t, path)))
			}
		}
	}
	var out []string
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out, unanalyzable
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// readOneArg returns one argument expression from the start of an argument list
// (positioned just after a comma), stopping at the next top-level comma or the
// call's closing paren. Quote- and bracket-aware so a comma inside a nested
// object or string does not split the argument.
func readOneArg(s string) string {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
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
			if depth == 0 {
				return s[:i]
			}
			depth--
		case ',':
			if depth == 0 {
				return s[:i]
			}
		}
	}
	return s
}

// resolveWebBodyVar resolves the keys a body variable carries at a call site: the
// keys of its nearest preceding `const|let|var id … = { … }` declaration, plus
// any `id.key = …` assignments between that declaration and the call.
//
// "Nearest preceding" is correct because a well-formed function declares the body
// before passing it, so a same-named local in another function never wins. If the
// variable has no literal initialiser the parser cannot read it, and the caller
// reports it unanalyzable rather than crediting an empty body.
func resolveWebBodyVar(src string, callPos int, id string) (map[string]bool, bool) {
	declRe := regexp.MustCompile(`(?:const|let|var)\s+` + regexp.QuoteMeta(id) + `\b[^\n=]*=\s*\{`)
	locs := declRe.FindAllStringIndex(src[:callPos], -1)
	if len(locs) == 0 {
		return nil, false
	}
	last := locs[len(locs)-1]
	lit := balancedFrom(src[last[1]-1:]) // from the '{'
	if lit == "" {
		return nil, false
	}
	keys := map[string]bool{}
	for _, k := range objectKeys(lit) {
		keys[k] = true
	}
	// Conditional additions: `id.field = …` between the declaration and the call.
	region := src[last[0]:callPos]
	asgn := regexp.MustCompile(`\b` + regexp.QuoteMeta(id) + `\.(\w+)\s*=[^=]`)
	for _, m := range asgn.FindAllStringSubmatch(region, -1) {
		keys[m[1]] = true
	}
	return keys, true
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

// --- Web nested payloads: what is SENT, not what is TYPED ---------------------

// A wrapper RPC hands its payload through whole (`{ id, update }`), so the
// call-body parse only ever sees the wrapper key. The obvious next move — read
// the payload's TypeScript interface — is WRONG in the dangerous direction: the
// interface says what is POSSIBLE, and a client that never sends a field still
// passes. `updateTask` is exactly that trap: TaskUpdate declares seven options
// and the single call site (web/src/index.ts:862) sends `{ enabled }` alone, so
// typing-based coverage credits the web with six options it cannot reach.
//
// So the reach is derived from VALUES: object literals the web actually passes
// at a T-typed parameter, or returns from a T-returning builder. Anything else
// at those positions is reported unanalyzable rather than assumed complete.

var tsParamRe = regexp.MustCompile(`function\s+(\w+)\s*\(([^)]*)\)`)
var tsReturnsRe = regexp.MustCompile(`function\s+(\w+)\s*\([^)]*\)\s*:\s*(\w+)\s*\{`)

// webValueReach is one TS payload type's derived reach.
type webValueReach struct {
	Fields       map[string]bool
	Sites        []string
	Unanalyzable []string
}

// webNestedValueReach returns the keys the web can actually put in a payload of
// the given TypeScript type.
func webNestedValueReach(t *testing.T, tsType string) webValueReach {
	t.Helper()
	out := webValueReach{Fields: map[string]bool{}}

	type paramPos struct {
		fn  string
		idx int
	}
	var takers []paramPos
	builders := map[string]bool{}

	files := webSourceFiles(t)
	srcs := map[string]string{}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		srcs[p] = string(b)
	}

	// Who produces this payload, and who SENDS it to the daemon?
	//
	// Only api.ts's wrappers count as senders: they are the layer that reaches
	// af(). A reader that merely takes a TaskData (tasks.ts's triggerSummary,
	// canTrigger) is not constructing a payload, and counting it would drown the
	// real findings in noise about functions that only look at tasks.
	for path, src := range srcs {
		for _, m := range tsReturnsRe.FindAllStringSubmatch(src, -1) {
			if m[2] == tsType {
				builders[m[1]] = true
			}
		}
		if !strings.HasSuffix(path, webAPIFile) {
			continue
		}
		for _, m := range tsParamRe.FindAllStringSubmatch(src, -1) {
			for i, param := range strings.Split(m[2], ",") {
				if _, typ, ok := strings.Cut(param, ":"); ok && strings.TrimSpace(typ) == tsType {
					takers = append(takers, paramPos{fn: m[1], idx: i})
				}
			}
		}
	}

	// A builder's returned literal is the payload it produces.
	for _, src := range srcs {
		for fn := range builders {
			for _, lit := range objectLiteralsReturnedBy(src, fn) {
				for _, k := range objectKeys(lit) {
					out.Fields[k] = true
				}
				out.Sites = append(out.Sites, "builder "+fn)
			}
		}
	}

	// And every literal handed to a T-typed parameter.
	for path, src := range srcs {
		for _, tk := range takers {
			for _, argExpr := range callArgsAt(src, tk.fn, tk.idx) {
				arg := strings.TrimSpace(argExpr)
				switch {
				case strings.HasPrefix(arg, "{"):
					for _, k := range objectKeys(arg) {
						out.Fields[k] = true
					}
					out.Sites = append(out.Sites, tk.fn+"() in "+relSite(t, path))
				case isBuilderCall(arg, builders):
					// Covered by the builder's own literal above.
				default:
					out.Unanalyzable = append(out.Unanalyzable,
						tk.fn+"("+arg+") in "+relSite(t, path))
				}
			}
		}
	}
	return out
}

func isBuilderCall(arg string, builders map[string]bool) bool {
	name, _, ok := strings.Cut(arg, "(")
	return ok && builders[strings.TrimSpace(name)]
}

// objectLiteralsReturnedBy returns the object literals a named function returns.
//
// The search is bounded to the function's OWN body. An unbounded scan from the
// declaration reads on into the rest of the file and steals the next function's
// literal: asked for genTaskId (which returns a string), it happily returned
// buildTask's TaskData literal. That over-credits a payload's reach, which is the
// under-reporting failure this package exists to prevent — a parser bug in the
// tool is worth more than a bug in the code it audits, because it silently
// launders itself into a green check.
func objectLiteralsReturnedBy(src, fn string) []string {
	re := regexp.MustCompile(`function\s+` + regexp.QuoteMeta(fn) + `\s*\(`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return nil
	}
	// Step over the parameter list (which may itself contain braces, e.g. a
	// destructured argument) to reach the body brace.
	parenAt := loc[1] - 1
	params := balancedFrom(src[parenAt:])
	if params == "" {
		return nil
	}
	after := src[parenAt+len(params):]
	bi := strings.Index(after, "{")
	if bi < 0 {
		return nil
	}
	body := balancedFrom(after[bi:])
	if body == "" {
		return nil
	}

	var out []string
	for off := 0; ; {
		i := strings.Index(body[off:], "return {")
		if i < 0 {
			break
		}
		at := off + i + len("return ")
		off = at
		if lit := balancedFrom(body[at:]); lit != "" {
			out = append(out, lit)
			off = at + len(lit)
		}
	}
	return out
}

// callArgsAt returns the argument expression at index idx for every CALL to fn.
//
// Three things are deliberately NOT calls, and treating them as such reports
// noise where the fail-closed findings should be:
//   - `function fn(...)` — the declaration; its parameter list is a signature.
//   - `x.fn(...)` — a method on something else that happens to share the name.
//   - `fn()` with no arguments — e.g. ui.ts's zero-arg actions member.
func callArgsAt(src, fn string, idx int) []string {
	var out []string
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(fn) + `\s*\(`)
	for off := 0; off < len(src); {
		loc := re.FindStringIndex(src[off:])
		if loc == nil {
			break
		}
		nameStart, parenAt := off+loc[0], off+loc[1]-1
		off = parenAt + 1

		before := strings.TrimRight(src[:nameStart], " \t\n")
		if strings.HasSuffix(before, ".") || strings.HasSuffix(before, "function") {
			continue
		}
		args := splitTopLevel(balancedFrom(src[parenAt:]))
		if idx >= len(args) || strings.TrimSpace(args[idx]) == "" {
			continue
		}
		out = append(out, args[idx])
	}
	return out
}

// balancedFrom returns the balanced bracketed span starting at s[0].
func balancedFrom(s string) string {
	if s == "" {
		return ""
	}
	open := s[0]
	var close byte
	switch open {
	case '{':
		close = '}'
	case '(':
		close = ')'
	default:
		return ""
	}
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
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
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return ""
}

// splitTopLevel splits a bracketed arg list on depth-1 commas.
func splitTopLevel(span string) []string {
	if len(span) < 2 {
		return nil
	}
	inner := span[1 : len(span)-1]
	var out []string
	depth, start := 0, 0
	var quote byte
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
				out = append(out, inner[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, inner[start:])
	return out
}

// webAPIFile is the module holding the af() chokepoint wrappers — the only layer
// that sends a payload to the daemon.
const webAPIFile = "api.ts"
