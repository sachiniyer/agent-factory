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

// TestDerivationSeesWebBackendGap pins #1933 through the WEB body parser: the
// daemon accepts nine CreateSession fields and the web sends five.
//
// COORDINATION: a fix for #1933 is in flight (#1968). When the web starts sending
// `backend`, THIS TEST FAILS BY DESIGN — it is the fixture doing its job, not a
// broken test. Retiring it is three edits, and they belong to the PR that
// lands the fix:
//  1. drop "backend" (and any other now-sent field) from the list below;
//  2. in parity/inventory.json, update session.create.opt.backend's web cell —
//     but see the WARNING below on what it may honestly claim;
//  3. drop the matching entry from field_coverage.web_rpcs.CreateSession.
//
// TestWebFieldCoverage will refuse to pass until (2) and (3) agree, so the
// inventory cannot be left claiming a gap that was fixed.
//
// WARNING — do not record the stronger claim. This test only proves what the web
// SENDS. That is not the same as what a user can DO, and #1968's author could not
// prove end-to-end that a working remote session is creatable from the browser:
// provisioning was observed (launch_cmd ran, a real agent-server came up) but the
// session then timed out waiting for its program, cause unresolved. So on landing,
// `partial` with that caveat in notes is the honest cell; `yes` requires someone
// to have created a working remote session from the browser and said so.
//
// This distinction is this package's oldest blind spot, stated under Known blind
// spots as "reachable != user-settable" — the derivation reads call sites, not
// outcomes. #1936 is the same trap from the other side: app/session_control.go:106
// DOES set Prompt, so the field reads as covered while the TUI's flow never
// populates it. A field-level pass is evidence about the wire, never about the
// user's experience.
func TestDerivationSeesWebBackendGap(t *testing.T) {
	sent := webCallBody(t, "CreateSession")
	if len(sent) == 0 {
		t.Fatal("web CreateSession body parsed as empty — the parser is blind")
	}
	for _, f := range []string{"backend", "force_remote", "in_place"} {
		if contains(sent, f) {
			t.Errorf("the web now sends CreateSession.%s — see the COORDINATION note above this "+
				"test: if #1933 was fixed, retire the fixture and flip the inventory cell in the "+
				"same PR; if it was not, the body parser is wrong.", f)
		}
	}
	// And prove the parser is not simply blind to every field.
	for _, f := range []string{"program", "prompt", "auto_yes"} {
		if !contains(sent, f) {
			t.Errorf("web CreateSession body is missing %q, which it demonstrably sends "+
				"(web/src/api.ts:251-264) — the body parser is under-reporting.", f)
		}
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

// TestDerivationSeesPreviewTabGap pins #1948: `af sessions preview` reaches the
// Preview RPC but can only ever see tab 0. This one also proves the derivation
// covers an INTERNAL route — Preview is absent from the public catalog, so a
// route-catalog-only check cannot see it.
func TestDerivationSeesPreviewTabGap(t *testing.T) {
	use := deriveGoRequestUse(t, "cli")
	typeUse := deriveTypeFieldUse(t, "cli")

	u, ok := use["PreviewRequest"]
	if !ok {
		t.Fatal("AST found no CLI PreviewRequest construction — the walk is blind " +
			"(expected api/sessions.go:674)")
	}
	unreached := unreachedFields(auditedRequests["PreviewRequest"], u, typeUse)
	for _, f := range []string{"tab", "tab_id", "full"} {
		if !contains(unreached, f) {
			t.Errorf("derivation says the CLI reaches PreviewRequest.%s, but api/sessions.go:674 "+
				"sends only {Title, RepoID}. Either #1948 was fixed (retire the fixture) or "+
				"the AST walk is over-crediting.", f)
		}
	}
	if contains(unreached, "title") {
		t.Error("derivation says the CLI never sets PreviewRequest.title — it plainly does; " +
			"the AST walk is under-reporting.")
	}

	// The TUI is the control: it drives the SAME RPC with every field, so a
	// derivation that reported the gap for both surfaces would be broken in a way
	// the CLI-only assertions above could not distinguish.
	tuiUse := deriveGoRequestUse(t, "tui")
	tui, ok := tuiUse["PreviewRequest"]
	if !ok {
		t.Fatal("AST found no TUI PreviewRequest construction (expected app/live_stream.go:35)")
	}
	tuiUnreached := unreachedFields(auditedRequests["PreviewRequest"], tui, deriveTypeFieldUse(t, "tui"))
	if len(tuiUnreached) != 0 {
		t.Errorf("the TUI sets every PreviewRequest field (app/live_stream.go:35), but derivation "+
			"reports %v unreached — it is over-reporting, which would manufacture false gaps.",
			tuiUnreached)
	}
}

// TestDerivationSeesNestedProjectPathGap pins the wrapper case behind #1935.
// UpdateTask is {id, update}: a top-level-only check calls it covered the moment
// the web sends `update`, hiding every option inside task.TaskUpdate. This is the
// exact shape that hid project_path.
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
			"to the very field that motivated it (task/task.go:428)")
	}

	// The web's contract for the payload is its TS interface; project_path is
	// absent from it (web/src/types.ts:174-182) while the other options are not.
	ts := webTSInterfaceFields(t, "TaskUpdate")
	if ts["project_path"] {
		t.Error("web/src/types.ts TaskUpdate now has project_path. If the gap was fixed, " +
			"update task.edit.project-path and retire this fixture.")
	}
	for _, f := range []string{"name", "prompt", "cron_expr", "enabled"} {
		if !ts[f] {
			t.Errorf("TS TaskUpdate is missing %q — the interface parser is under-reporting, "+
				"which would invent gaps that do not exist.", f)
		}
	}

	// The CLI reaches the payload field-by-field (`patch.Name = …`), not as a
	// literal, so this also proves the assignment-tracking half of the walk.
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
