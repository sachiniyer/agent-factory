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

// TestDerivationSeesWebBackendGap pins #1933 through the WEB body parser: the
// daemon accepts nine CreateSession fields and the web sends five.
func TestDerivationSeesWebBackendGap(t *testing.T) {
	sent := webCallBody(t, "CreateSession")
	if len(sent) == 0 {
		t.Fatal("web CreateSession body parsed as empty — the parser is blind")
	}
	for _, f := range []string{"backend", "force_remote", "in_place"} {
		if contains(sent, f) {
			t.Errorf("the web now sends CreateSession.%s. If #1933 was fixed, update the "+
				"inventory verdict and retire this fixture; if not, the parser is wrong.", f)
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
