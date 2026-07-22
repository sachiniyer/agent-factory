package parity

// Is the derivation honest?
//
// Every other test in this package compares the derivation against the
// inventory. None of them ask the prior question: does the derivation SEE
// anything? A derivation with a hole does not fail loudly — it silently
// under-reports and the suite goes green, which is worse than having no parity
// check at all, because the green would be trusted.
//
// So these tests pin the derivation against gaps we already know are real,
// filed, and field-level. They are fixtures, not aspirations: if the derivation
// cannot rediscover a gap we found by hand, it will not find the next one.
//
//	#1933  the web never sends CreateSessionRequest.backend  (and neither does the TUI)
//	#1948  the CLI never sets PreviewRequest.Tab/TabID/Full
//	#1935  the web's TaskUpdate omits project_path, nested inside a wrapper route
//
// Each is deliberately a DIFFERENT derivation path — web request bodies, the Go
// AST, and nested payloads behind a wrapper — so a hole in any one path fails
// here rather than passing quietly.
//
// If a gap below is ever FIXED, the matching test fails and says so. That is
// correct: the fixture has to be retired deliberately, not silently.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// contains reports whether the derived unreached set includes a field.
func contains(unreached []string, field string) bool {
	for _, f := range unreached {
		if f == field {
			return true
		}
	}
	return false
}

// TestDerivationSeesLazyCobraSurface pins that the tree is walked AFTER cobra
// finishes building it.
//
// cobra adds `completion` (and its per-shell subcommands), `help`, `--help` and
// `--version` lazily inside Execute(). Walking the freshly-constructed tree
// omits all of it — real commands a user can run — so the inventory silently
// disagreed with what af ships, which is the one thing this package claims
// cannot happen. Drop initCobraDefaults and this fixture fails.
func TestDerivationSeesLazyCobraSurface(t *testing.T) {
	derived := deriveCLI(t)

	for _, path := range []string{"af completion bash", "af completion zsh", "af help"} {
		if _, ok := derived[path]; !ok {
			t.Errorf("%q is not derived — cobra's lazily-added commands are invisible to the "+
				"walk, so the inventory omits real CLI surface. Restore initCobraDefaults "+
				"(parity/derive_test.go).", path)
		}
	}

	hasFlag := func(path, flag string) bool {
		for _, f := range derived[path].Flags {
			if f == flag {
				return true
			}
		}
		return false
	}
	if !hasFlag("af", "version") {
		t.Error("`af --version` is not derived — InitDefaultVersionFlag did not run before the walk.")
	}
	for _, path := range []string{"af", "af version", "af sessions create"} {
		if !hasFlag(path, "help") {
			t.Errorf("%q has no derived --help — InitDefaultHelpFlag did not run for it, so "+
				"every command's help flag is missing from the inventory.", path)
		}
	}
}

// TestDerivationSeesWebCreateOptions pins the web CreateSession body parser
// against ground truth. #1968 (the fix for #1933) LANDED: the web now sends
// `backend` — conditionally, and via a `const body = {…}` variable rather than an
// inline literal, which is the shape that made the old parser go blind and read
// the whole body as empty. So this fixture now asserts the reverse of what it did
// before the rebase: `backend` IS reached, and `force_remote`/`in_place` are
// still not.
//
// It reads what the web SENDS, which is not the same as what a user can DO. #1968
// could not prove end-to-end that a working remote session is creatable from the
// browser — provisioning was observed but the program-start timed out, cause
// unresolved — so the inventory records session.create.opt.backend's web cell as
// `partial`, not `yes`. `yes` needs someone to have created a working remote
// session from a browser and said so. This is the package's oldest blind spot,
// "reachable != user-settable" under Known blind spots: the derivation reads call
// sites, not outcomes. #1936 is the same trap mirrored.
func TestDerivationSeesWebCreateOptions(t *testing.T) {
	sent, unanalyzable := webCallBodyChecked(t, "CreateSession")
	if len(unanalyzable) > 0 {
		t.Fatalf("web CreateSession has a body the parser cannot read: %v — it is blind, so "+
			"every field claim below is worthless", unanalyzable)
	}
	if len(sent) == 0 {
		t.Fatal("web CreateSession body parsed as empty — the parser is blind (this is what the " +
			"#1968 `const body = {…}` refactor did to the old inline-literal-only parser)")
	}
	// #1968 landed: backend is now reached. If it stops being reached, either the
	// fix was reverted or the parser regressed on the variable-body shape.
	if !contains(sent, "backend") {
		t.Error("web CreateSession no longer sends `backend`. #1968 shipped it via a " +
			"`const body = {…}; body.backend = …` variable — if resolveWebBodyVar regressed the " +
			"parser is blind again; if the feature was reverted, flip the inventory cell back.")
	}
	// Still not sent — the remaining half of the create-option gap.
	for _, f := range []string{"in_place"} {
		if contains(sent, f) {
			t.Errorf("web CreateSession now sends %q — update session.create.opt.* and "+
				"field_coverage.web_rpcs.CreateSession in the same change.", f)
		}
	}
	// Not blind to the base fields.
	for _, f := range []string{"program", "prompt"} {
		if !contains(sent, f) {
			t.Errorf("web CreateSession body is missing %q, which it demonstrably sends "+
				"(web/src/api.ts createSession) — the body parser is under-reporting.", f)
		}
	}
}

// TestWebPromptParityNamesTheLiveTerminalTransport pins the distinction exposed
// by the #2188 review: the browser CAN send a prompt, but it does so by typing on
// the attached PTY. A dead af("SendPrompt", ...) wrapper is not a production
// surface and must not keep that RPC in the web reachability ledger. The daemon
// route remains public for the CLI, so this asserts both sides instead of
// deleting the capability itself.
func TestWebPromptParityNamesTheLiveTerminalTransport(t *testing.T) {
	inv := loadInventory(t)
	if deriveWebRPCs(t)["SendPrompt"] {
		t.Fatal("web prompt delivery is reaching SendPrompt RPC again; either remove the dead wrapper or deliberately update the terminal-transport parity decision")
	}
	if _, stale := inv.Ledger.WebRPCs["SendPrompt"]; stale {
		t.Fatal("web_rpcs still counts SendPrompt even though the browser reaches prompt delivery through its live terminal")
	}
	if got := inv.Ledger.Routes["POST /v1/SendPrompt"]; got != "session.send-prompt" {
		t.Fatalf("the daemon/CLI SendPrompt route disappeared with the dead web wrapper: route maps to %q", got)
	}
	capability, ok := inv.byID()["session.send-prompt"]
	if !ok || capability.Web.Status != "yes" || !strings.Contains(capability.Web.Pointer, "terminal.ts") {
		t.Fatalf("session.send-prompt must remain a web capability through terminal input, got %+v", capability.Web)
	}
}

// TestDerivationSeesTUIBackendGap pins the other half of #1933 through the Go
// AST: the TUI's sessionStartRequest has no Backend field at all.
func TestDerivationSeesTUIBackendGap(t *testing.T) {
	use := deriveGoRequestUse(t, "tui")
	typeUse := deriveTypeFieldUse(t, "tui")

	u, ok := use["CreateSessionRequest"]
	if !ok {
		t.Fatal("AST found no TUI CreateSessionRequest construction — the walk is blind " +
			"(expected app/session_control.go:101)")
	}
	unreached := unreachedFields(auditedRequests["CreateSessionRequest"], u, typeUse)
	for _, f := range []string{"backend", "in_place"} {
		if !contains(unreached, f) {
			t.Errorf("derivation says the TUI reaches CreateSession.%s, but app/session_control.go:88-95 "+
				"has no such field. Either the gap was fixed (retire the fixture) or the AST "+
				"walk is over-crediting.", f)
		}
	}
	// Not blind: the TUI provably does set these.
	for _, f := range []string{"program", "prompt", "force_remote"} {
		if contains(unreached, f) {
			t.Errorf("derivation says the TUI never sets CreateSession.%s, but it does "+
				"(app/session_control.go:101-109) — the AST walk is under-reporting.", f)
		}
	}
}

// TestDerivationTracksPreviewTabUse is the successor to the #1948 fixture. That
// gap ("`af sessions preview` reaches the Preview RPC but can only ever see tab
// 0") is now CLOSED, and the old test said what to do when that happened: retire
// the fixture. Retiring it outright would delete the meta-check with it, so it is
// repointed rather than removed — and the post-fix state is a strictly better
// fixture, because one request type now exercises BOTH failure directions at
// once:
//
//   - under-reporting: the CLI sets tab/tab_id/tab_name/full, so a walk that
//     missed real usage would report them unreached and manufacture a false gap.
//   - over-reporting: the TUI sets every field EXCEPT tab_name (it holds the live
//     tab and addresses it by the stronger TabID), so a walk that credited
//     construction it never saw would report nothing unreached and hide a real
//     one.
//
// It also still proves the derivation covers an INTERNAL route — Preview is
// absent from the public catalog, so a route-catalog-only check cannot see it.
func TestDerivationTracksPreviewTabUse(t *testing.T) {
	use := deriveGoRequestUse(t, "cli")
	typeUse := deriveTypeFieldUse(t, "cli")

	u, ok := use["PreviewRequest"]
	if !ok {
		t.Fatal("AST found no CLI PreviewRequest construction — the walk is blind " +
			"(expected api/sessions.go, sessionsPreviewCmd)")
	}
	unreached := unreachedFields(auditedRequests["PreviewRequest"], u, typeUse)
	// Not blind: the CLI provably sets all four selectors since #1948.
	for _, f := range []string{"title", "tab", "tab_id", "tab_name", "full"} {
		if contains(unreached, f) {
			t.Errorf("derivation says the CLI never sets PreviewRequest.%s, but sessionsPreviewCmd "+
				"plainly does (#1948) — the AST walk is under-reporting, which manufactures false "+
				"gaps.", f)
		}
	}

	// The TUI is the control, and it is no longer a trivial one: it sets every
	// field but tab_name, so this pins the derivation's ability to spot a genuine
	// non-use rather than just agreeing that everything is covered.
	tuiUse := deriveGoRequestUse(t, "tui")
	tui, ok := tuiUse["PreviewRequest"]
	if !ok {
		t.Fatal("AST found no TUI PreviewRequest construction (expected app/live_stream.go:35)")
	}
	tuiUnreached := unreachedFields(auditedRequests["PreviewRequest"], tui, deriveTypeFieldUse(t, "tui"))
	if !contains(tuiUnreached, "tab_name") {
		t.Error("derivation says the TUI reaches PreviewRequest.tab_name, but app/live_stream.go " +
			"addresses the tab by TabID and never sends a name — the AST walk is over-crediting, " +
			"which HIDES real gaps.")
	}
	for _, f := range []string{"title", "tab", "tab_id", "full"} {
		if contains(tuiUnreached, f) {
			t.Errorf("derivation says the TUI never sets PreviewRequest.%s, but app/live_stream.go:35 "+
				"does — the AST walk is under-reporting.", f)
		}
	}
}

// TestDerivationSeesNestedProjectPathGap pins the wrapper case that motivated
// #1935. UpdateTask is {id, update}: a top-level-only check calls it covered the
// moment the web sends `update`, hiding every option inside task.TaskUpdate —
// exactly how project_path stayed invisible.
//
// #1935 LANDED (like #1968 for the sibling fixture above): the web now edits tasks,
// and its edit call site sends project_path as an inline literal. So this fixture
// asserts the reverse of what it did before — project_path is IN the TS interface
// AND reachable BY VALUE (the derivation real coverage uses) — while still proving
// the recursion and the CLI-side assignment walk see what they must. The CLI has no
// --project-path flag, so the assignment walk must still report TaskUpdate.ProjectPath
// unreached: the surviving CLI half of task.edit.project-path.
func TestDerivationSeesNestedProjectPathGap(t *testing.T) {
	paths := jsonFieldPaths(auditedRequests["UpdateTaskRequest"])
	var nested []string
	for _, p := range paths {
		if strings.HasPrefix(p, "update.") {
			nested = append(nested, p)
		}
	}
	sort.Strings(nested)
	if len(nested) < 8 {
		t.Fatalf("jsonFieldPaths did not recurse into UpdateTaskRequest.Update: got %v. "+
			"Without recursion the wrapper hides every task option.", nested)
	}
	if !contains(nested, "update.project_path") {
		t.Fatal("update.project_path is not in the derived field paths — the recursion is blind " +
			"to the very field that motivated it (task/task.go:566)")
	}

	// #1935 landed: project_path is now in the web's TS TaskUpdate (web/src/types.ts),
	// alongside the options that were always there. If it disappears the fix was
	// reverted — flip task.edit.project-path's web cell back to `no`.
	ts := webTSInterfaceFields(t, "TaskUpdate")
	if !ts["project_path"] {
		t.Error("web/src/types.ts TaskUpdate no longer has project_path — #1935 was reverted; " +
			"flip task.edit's web cell back to `partial` and task.edit.project-path's to `no`.")
	}
	for _, f := range []string{"name", "prompt", "cron_expr", "enabled"} {
		if !ts[f] {
			t.Errorf("TS TaskUpdate is missing %q — the interface parser is under-reporting, "+
				"which would invent gaps that do not exist.", f)
		}
	}

	// The interface says what is POSSIBLE; coverage keys off what the web SENDS. Prove
	// the edit call site reaches project_path (and enabled, via the toggle) BY VALUE —
	// the derivation the real check uses — and that no call site is unreadable (a
	// variable body would silently blind the audit to the reach it derives here).
	reach := webNestedValueReach(t, "TaskUpdate")
	if len(reach.Unanalyzable) > 0 {
		t.Errorf("web TaskUpdate has call sites the value walk cannot read: %v — the audit would "+
			"go blind on the reach it derives from them.", reach.Unanalyzable)
	}
	for _, f := range []string{"project_path", "name", "prompt", "cron_expr", "enabled"} {
		if !reach.Fields[f] {
			t.Errorf("the web does not reach TaskUpdate.%s by value (sites: %v) — #1935's edit "+
				"call site must send it as an inline literal, or the coverage credit is fiction.",
				f, reach.Sites)
		}
	}

	// The CLI reaches the payload field-by-field (`patch.Name = …`), not as a
	// literal, so this also proves the assignment-tracking half of the walk. It still
	// has no --project-path flag, so ProjectPath must stay unreached.
	typeUse := deriveTypeFieldUse(t, "cli")
	fields := typeUse["task.TaskUpdate"]
	if len(fields) == 0 {
		t.Fatal("derivation sees the CLI populating NO task.TaskUpdate fields — the " +
			"var-assignment walk is blind (expected api/tasks.go:296-320 `patch.Name = …`)")
	}
	if fields["ProjectPath"] {
		t.Error("derivation says the CLI sets TaskUpdate.ProjectPath — `af tasks update` has " +
			"no --project-path flag (api/api.go:644-650). Over-crediting.")
	}
	if !fields["Name"] {
		t.Error("derivation says the CLI never sets TaskUpdate.Name, but api/tasks.go does " +
			"(`patch.Name = strPtr(...)`) — under-reporting.")
	}
}

// TestReturnedLiteralsAreBounded locks a parser bug found by reading this
// package's own unreviewed diff: objectLiteralsReturnedBy searched from a
// function's declaration to the END OF THE FILE, so asked for a function that
// returns a string it returned the NEXT function's object literal.
//
// It matters more than a bug in ordinary code. An over-credited payload reach
// makes a real gap read as parity — the analyzer launders its own defect into a
// green check, which is the failure this package exists to prevent.
func TestReturnedLiteralsAreBounded(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "web/src/tasks.ts"))
	if err != nil {
		t.Fatalf("read tasks.ts: %v", err)
	}
	src := string(b)

	// buildTask really does return an object literal — if this is 0 the parser
	// is blind and every assertion below is vacuous.
	if got := objectLiteralsReturnedBy(src, "buildTask"); len(got) != 1 {
		t.Fatalf("buildTask should return exactly 1 object literal, got %d — the parser is blind", len(got))
	}
	// These return strings. Any literal here leaked from a neighbouring
	// function, which is the unbounded-search bug.
	for _, fn := range []string{"genTaskId", "triggerSummary", "lastRunSummary"} {
		if got := objectLiteralsReturnedBy(src, fn); len(got) != 0 {
			t.Errorf("%s() returns a string, but the parser attributed %d object literal(s) to "+
				"it — the search is reaching past the function body into a neighbour, which "+
				"over-credits a payload's reach and turns a real gap into a green check.",
				fn, len(got))
		}
	}
}

// TestBothSurfaceScannersRecurse locks the recursion on BOTH source scanners, so
// a refactor from WalkDir back to a flat ReadDir fails instead of silently
// re-blinding the audit to any module in a subdirectory.
//
// The Go side had exactly that hole when this test was written — goSurfaceFiles
// used a non-recursive ReadDir while webSourceFiles (fixed earlier in the same
// PR) recursed. A planted api/sub file proved invisible. The two scanners must
// stay symmetric: both descend, or the audit is honest on one half and blind on
// the other.
//
// It plants a real file in a subdirectory, asserts the scanner finds it, and
// removes it — so the assertion is about what the CURRENT code walks, not a
// fixture checked into the tree.
func TestBothSurfaceScannersRecurse(t *testing.T) {
	root := repoRoot(t)

	t.Run("go", func(t *testing.T) {
		dir := filepath.Join(root, "api", "paritywalkprobe")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		defer os.RemoveAll(dir)
		planted := filepath.Join(dir, "planted.go")
		if err := os.WriteFile(planted, []byte("package paritywalkprobe\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		found := false
		for _, f := range goSurfaceFiles(t, "cli") {
			if f == planted {
				found = true
			}
		}
		if !found {
			t.Errorf("goSurfaceFiles(cli) did not descend into api/paritywalkprobe/. It is not " +
				"recursing, so a request built in a subdirectory of api/app/apiclient is " +
				"unaudited and the audit reports false parity over it — the exact hole " +
				"webSourceFiles already closed. Restore the WalkDir in goSurfaceFiles.")
		}
	})

	t.Run("web", func(t *testing.T) {
		dir := filepath.Join(root, "web", "src", "paritywalkprobe")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		defer os.RemoveAll(dir)
		planted := filepath.Join(dir, "planted.ts")
		if err := os.WriteFile(planted, []byte("export const x = 1;\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		found := false
		for _, f := range webSourceFiles(t) {
			if f == planted {
				found = true
			}
		}
		if !found {
			t.Errorf("webSourceFiles did not descend into web/src/paritywalkprobe/. It is not " +
				"recursing, so a module moved into a subdirectory of web/src is unaudited and " +
				"the audit reports false parity over it. Restore the WalkDir in webSourceFiles.")
		}
	})
}

// TestDerivationCoversEveryRequestConstructionSite is the backstop the fixtures
// above cannot provide: they prove specific known gaps are visible, not that the
// walk reaches every surface. If a surface's sources move, this catches the walk
// going quiet before a fixture happens to notice.
func TestDerivationCoversEveryRequestConstructionSite(t *testing.T) {
	for _, surface := range []string{"cli", "tui"} {
		files := goSurfaceFiles(t, surface)
		if len(files) == 0 {
			t.Fatalf("%s: goSurfaces resolved to no files — the walk sees nothing", surface)
		}
		use := deriveGoRequestUse(t, surface)
		if len(use) == 0 {
			t.Fatalf("%s: AST found no daemon request literals across %d files", surface, len(files))
		}
		for typeName, u := range use {
			if len(u.Sites) == 0 {
				t.Errorf("%s: %s recorded with no construction site", surface, typeName)
			}
		}
	}
	// Both wrapper packages must be in view: the task requests are built there
	// and nowhere else, so losing them would silently un-audit every task option.
	for surface, wantType := range map[string]string{"cli": "UpdateTaskRequest", "tui": "UpdateTaskRequest"} {
		if _, ok := deriveGoRequestUse(t, surface)[wantType]; !ok {
			t.Errorf("%s: %s is not derived — the wrapper package (daemon/control_client.go or "+
				"apiclient/) has dropped out of goSurfaces, so its fields are unaudited.",
				surface, wantType)
		}
	}
}
