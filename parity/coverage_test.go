package parity

// The audit's denominator.
//
// Every other test here asks "do the surfaces agree?". This one asks the
// question that has to be answered first: WHAT DID THE AUDIT ACTUALLY LOOK AT?
//
// An audit that under-covers does not merely miss gaps. It asserts parity over
// surfaces it never opened, and it is believed, because it is a green check —
// converting "we have a gap" into "we have a gap and a test says we do not".
// Every hole found in this package so far had that shape: a walk that ran before
// cobra finished building the tree, a web scan that skipped subdirectories, an
// enum check that read one of two selectors, a request shape the analyzer
// dropped rather than reported.
//
// So the audit states its denominator out loud (`go test ./parity/ -v -run
// TestAuditCoverageReport`) and, more importantly, FAILS when it cannot see
// something rather than shrugging. A file it could not parse, a construct it
// could not derive, a directory it did not enter is a finding, not a pass.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// coverage is what one audit run looked at, and what it could not.
type coverage struct {
	Scanned map[string]int // what was read, by kind
	Skipped []string       // anything NOT covered, with the reason — must be empty
}

func (c *coverage) count(kind string, n int) {
	if c.Scanned == nil {
		c.Scanned = map[string]int{}
	}
	c.Scanned[kind] += n
}

func (c *coverage) skip(what, why string) {
	c.Skipped = append(c.Skipped, what+" — "+why)
}

// TestAuditCoverageReport emits the denominator and fails on any blind spot.
//
// Run `go test ./parity/ -v -run TestAuditCoverageReport` to read it.
func TestAuditCoverageReport(t *testing.T) {
	inv := loadInventory(t)
	c := &coverage{}

	// --- CLI ---------------------------------------------------------------
	cli := deriveCLI(t)
	verbs, flags := 0, 0
	for _, v := range cli {
		if v.Runnable {
			verbs++
		}
		flags += len(v.Flags)
	}
	c.count("cli.commands", len(cli))
	c.count("cli.verbs", verbs)
	c.count("cli.flags", flags)
	if _, ok := cli["af completion bash"]; !ok {
		c.skip("cli.lazy-commands", "cobra's lazily-added commands are missing: the tree was "+
			"walked before InitDefaultCompletionCmd/InitDefaultHelpCmd ran, so real user-facing "+
			"surface is invisible")
	}

	// --- Daemon route catalog ----------------------------------------------
	routes := deriveRoutes(t)
	c.count("daemon.public-routes", len(routes))
	// The public catalog is not the whole mux. Internal routes (Preview) are served
	// but excluded from HTTPRoutes(), so they are covered at the FIELD level via
	// auditedRequests instead — state that rather than letting the route count imply
	// total coverage.
	//
	// ResumeFromLimit left this list in #1934: it is now a PUBLIC route, so the
	// route count covers it directly and naming it here would claim a blind spot
	// that no longer exists.
	c.count("daemon.audited-request-types", len(auditedRequests))
	for _, internal := range []string{"PreviewRequest"} {
		if _, ok := auditedRequests[internal]; !ok {
			c.skip("daemon.internal-route "+internal, "served but absent from HTTPRoutes() AND "+
				"not in auditedRequests, so neither the verb nor the field level sees it")
		}
	}

	// --- TUI ---------------------------------------------------------------
	c.count("tui.bindings", len(deriveTUI(t)))

	// --- Web ---------------------------------------------------------------
	webFiles := webSourceFiles(t)
	c.count("web.source-files", len(webFiles))
	c.count("web.rpcs", len(deriveWebRPCs(t)))
	// Hardcoded copies of the agent enum, which #1970 removed by serving it. This is
	// now expected to be ZERO, so unlike the other counts it carries no floor — a
	// floor here would demand the bug exist. The anti-vacuous guarantee moved into
	// TestAgentEnumDetectorIsNotVacuous, which proves every run that the detector can
	// still SEE a copy; without that, a zero here would be indistinguishable from a
	// blind check.
	webEnumCopies := 0
	for _, path := range webFiles {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		webEnumCopies += len(hardcodedAgentEnums(string(b)))
	}
	c.count("web.hardcoded-enum-sites", webEnumCopies)

	// --- Go surfaces: request construction ---------------------------------
	for _, surface := range []string{"cli", "tui"} {
		files := goSurfaceFiles(t, surface)
		c.count("go."+surface+".files", len(files))
		sites, unanalyzable := 0, 0
		for typeName, u := range deriveGoRequestUse(t, surface) {
			sites += len(u.Sites)
			unanalyzable += len(u.Unanalyzable)
			for _, site := range u.Unanalyzable {
				c.skip("go."+surface+"."+typeName,
					"built at "+site+" in a shape the analyzer cannot read, so its field "+
						"coverage is unverified")
			}
		}
		c.count("go."+surface+".request-sites", sites)
	}

	// --- Argument shapes ---------------------------------------------------
	groups := deriveArgShapes(t, inv.ArgumentShapes.Synonyms)
	concepts := 0
	for _, g := range groups {
		concepts += len(g)
	}
	c.count("cli.noun-groups", len(groups))
	c.count("cli.arg-concepts", concepts)

	// --- Inventory ---------------------------------------------------------
	c.count("inventory.capabilities", len(inv.Capabilities))
	byVerdict := map[string]int{}
	for _, cap := range inv.Capabilities {
		byVerdict[cap.Verdict]++
	}

	// --- Report ------------------------------------------------------------
	var kinds []string
	for k := range c.Scanned {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	var b strings.Builder
	b.WriteString("\n=== surface-parity audit coverage ===\n")
	for _, k := range kinds {
		b.WriteString(fmt.Sprintf("  %-32s %d\n", k, c.Scanned[k]))
	}
	b.WriteString("  verdicts:")
	for _, v := range []string{"parity", "deliberate", "real-gap", "unclear"} {
		b.WriteString(fmt.Sprintf("  %s=%d", v, byVerdict[v]))
	}
	b.WriteString("\n")
	if len(c.Skipped) == 0 {
		b.WriteString("  SKIPPED: none — every surface above was read\n")
	}
	t.Log(b.String())

	// Fail closed. Anything the audit could not see is a finding: a green run
	// must mean "looked and found parity", never "did not look".
	for _, s := range c.Skipped {
		t.Errorf("audit blind spot: %s\n\nThis is under-coverage, which is worse than a missing "+
			"check: the suite would go green while asserting parity over something it never "+
			"read. Fix the derivation — do not silence this.", s)
	}

	// A denominator that collapses is itself a blind spot: these floors are the
	// difference between "scanned everything and found parity" and "scanned
	// nothing and found nothing".
	floors := map[string]int{
		"cli.verbs": 40, "cli.flags": 100, "daemon.public-routes": 15,
		"tui.bindings": 40, "web.source-files": 15, "web.rpcs": 10,
		"go.cli.request-sites": 8, "go.tui.request-sites": 8,
		"cli.noun-groups": 5, "inventory.capabilities": 50,
	}
	for kind, floor := range floors {
		if c.Scanned[kind] < floor {
			t.Errorf("coverage collapsed: %s = %d, expected >= %d. The derivation for that "+
				"surface has gone (partly) blind — a shrinking denominator makes every parity "+
				"claim above it meaningless.", kind, c.Scanned[kind], floor)
		}
	}
}
