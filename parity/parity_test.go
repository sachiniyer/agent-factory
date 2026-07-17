package parity

// The drift check. Each test compares a surface DERIVED FROM CODE against
// inventory.json and fails when the two disagree, so a capability landing on one
// surface cannot silently skip the question "what about the other two?".
//
// These tests are pure table reads — no daemon, no tmux, no AGENT_FACTORY_HOME.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type cell struct {
	Status  string `json:"status"`
	Pointer string `json:"pointer"`
}

type capability struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	TUI     cell   `json:"tui"`
	Web     cell   `json:"web"`
	CLI     cell   `json:"cli"`
	Verdict string `json:"verdict"`
	Issue   string `json:"issue"`
	Notes   string `json:"notes"`
}

type ledger struct {
	CLIVerbs map[string]string `json:"cli_verbs"`
	// CLIFlags is keyed "af sessions create --backend". A flag IS a capability:
	// the option dimension of a verb. Without this, a new flag on an existing
	// command ships with no parity decision — the same hole that let #1948
	// through at the field level.
	CLIFlags    map[string]string `json:"cli_flags"`
	Routes      map[string]string `json:"routes"`
	TUIBindings map[string]string `json:"tui_bindings"`
	WebRPCs     map[string]string `json:"web_rpcs"`
}

type inventory struct {
	Capabilities  []capability  `json:"capabilities"`
	Ledger        ledger        `json:"ledger"`
	FieldCoverage fieldCoverage `json:"field_coverage"`
}

func loadInventory(t *testing.T) inventory {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("inventory.json"))
	if err != nil {
		t.Fatalf("read inventory.json: %v", err)
	}
	var inv inventory
	if err := json.Unmarshal(b, &inv); err != nil {
		t.Fatalf("parse inventory.json: %v", err)
	}
	return inv
}

// surfaceStatus returns a capability's status cell for one surface.
func surfaceStatus(c capability, surface string) string {
	switch surface {
	case "tui":
		return c.TUI.Status
	case "web":
		return c.Web.Status
	case "cli":
		return c.CLI.Status
	}
	return ""
}

func (inv inventory) byID() map[string]capability {
	out := map[string]capability{}
	for _, c := range inv.Capabilities {
		out[c.ID] = c
	}
	return out
}

// fixHint is appended to every drift failure: the whole point of this package is
// that the fix is "make a parity decision", not "silence the test".
const fixHint = "\n\nThis is the surface-parity drift check (see docs/surface-parity.md). " +
	"Add the item to parity/inventory.json: map it in the ledger and give its capability " +
	"a tui/web/cli status with a code pointer and a verdict. If the other surfaces " +
	"deliberately will not have it, say so in notes — that records the decision so it is " +
	"never re-reported as a gap."

func diff(derived, known []string) (missing []string) {
	k := map[string]bool{}
	for _, s := range known {
		k[s] = true
	}
	for _, d := range derived {
		if !k[d] {
			missing = append(missing, d)
		}
	}
	sort.Strings(missing)
	return missing
}

func keysOf[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestCLIVerbsAreInventoried fails when a new cobra command appears with no
// inventory entry.
func TestCLIVerbsAreInventoried(t *testing.T) {
	inv := loadInventory(t)
	// Runnable commands only: `af sessions` is a grouping node, not a verb. Its
	// persistent flags are still audited by TestCLIFlagsAreInventoried.
	derived := cliVerbs(t)

	if missing := diff(keysOf(derived), keysOf(inv.Ledger.CLIVerbs)); len(missing) > 0 {
		t.Errorf("CLI verbs with no inventory entry: %v%s", missing, fixHint)
	}
	// A verb removed from the tree but left in the inventory is drift too: the
	// table would keep advertising a capability af no longer has.
	if stale := diff(keysOf(inv.Ledger.CLIVerbs), keysOf(derived)); len(stale) > 0 {
		t.Errorf("inventory lists CLI verbs that no longer exist: %v", stale)
	}
}

// TestCLIFlagsAreInventoried fails when a cobra command grows a flag with no
// inventory entry. A flag is a capability — it is the option dimension of a
// verb, and the verb-level check above is blind to it: `af sessions create`
// existing says nothing about whether it can pass --backend.
func TestCLIFlagsAreInventoried(t *testing.T) {
	inv := loadInventory(t)

	var derived []string
	for path, v := range deriveCLI(t) {
		for _, f := range v.Flags {
			derived = append(derived, path+" --"+f)
		}
	}
	sort.Strings(derived)

	if missing := diff(derived, keysOf(inv.Ledger.CLIFlags)); len(missing) > 0 {
		t.Errorf("CLI flags with no inventory entry: %v%s", missing, fixHint)
	}
	if stale := diff(keysOf(inv.Ledger.CLIFlags), derived); len(stale) > 0 {
		t.Errorf("inventory lists CLI flags that no longer exist: %v", stale)
	}
}

// TestRoutesAreInventoried fails when the daemon's public catalog grows a route
// with no inventory entry. The catalog is the shared substrate all three
// surfaces sit on, so a new route is the earliest signal of a coming divergence.
func TestRoutesAreInventoried(t *testing.T) {
	inv := loadInventory(t)
	derived := deriveRoutes(t)

	if missing := diff(keysOf(derived), keysOf(inv.Ledger.Routes)); len(missing) > 0 {
		t.Errorf("daemon routes with no inventory entry: %v%s", missing, fixHint)
	}
	if stale := diff(keysOf(inv.Ledger.Routes), keysOf(derived)); len(stale) > 0 {
		t.Errorf("inventory lists routes that no longer exist: %v", stale)
	}
}

// TestTUIBindingsAreInventoried fails when a keybinding is added or its identity
// changes. A binding is a capability.
func TestTUIBindingsAreInventoried(t *testing.T) {
	inv := loadInventory(t)
	derived := deriveTUI(t)

	if missing := diff(keysOf(derived), keysOf(inv.Ledger.TUIBindings)); len(missing) > 0 {
		t.Errorf("TUI bindings with no inventory entry: %v%s", missing, fixHint)
	}
	if stale := diff(keysOf(inv.Ledger.TUIBindings), keysOf(derived)); len(stale) > 0 {
		t.Errorf("inventory lists TUI bindings that no longer exist: %v", stale)
	}
}

// TestWebRPCsAreInventoried fails when the web client starts (or stops) calling
// a daemon RPC. This is the half that catches the web catching UP: wire a
// restore button and this test tells you to update session.restore's verdict.
func TestWebRPCsAreInventoried(t *testing.T) {
	inv := loadInventory(t)
	derived := deriveWebRPCs(t)

	if missing := diff(keysOf(derived), keysOf(inv.Ledger.WebRPCs)); len(missing) > 0 {
		t.Errorf("web RPC calls with no inventory entry: %v%s", missing, fixHint)
	}
	if stale := diff(keysOf(inv.Ledger.WebRPCs), keysOf(derived)); len(stale) > 0 {
		t.Errorf("inventory lists web RPCs the client no longer calls: %v", stale)
	}
}

// TestLedgerPointsAtRealCapabilities keeps the ledger honest: every mapping must
// name a capability that exists (or "-" for a non-capability).
func TestLedgerPointsAtRealCapabilities(t *testing.T) {
	inv := loadInventory(t)
	caps := inv.byID()

	check := func(surface string, m map[string]string) {
		for item, capID := range m {
			if capID == "-" {
				continue
			}
			if _, ok := caps[capID]; !ok {
				t.Errorf("%s ledger maps %q to unknown capability %q", surface, item, capID)
			}
		}
	}
	check("cli_verbs", inv.Ledger.CLIVerbs)
	check("cli_flags", inv.Ledger.CLIFlags)
	check("routes", inv.Ledger.Routes)
	check("tui_bindings", inv.Ledger.TUIBindings)
	check("web_rpcs", inv.Ledger.WebRPCs)
}

// TestLedgerAgreesWithSurfaceStatus closes the gap between "it is mapped" and
// "the table says it exists". Mapping an item into the ledger is otherwise
// enough to make every drift test pass while the capability row still claims
// that surface does not have it — so wiring a web restore button and adding the
// ledger entry would leave session.restore reading "web: no, real-gap" forever.
//
// The ledger is derived-truth ("this surface reaches this capability"), so it is
// the side that wins: a mapping proves the surface HAS it.
func TestLedgerAgreesWithSurfaceStatus(t *testing.T) {
	inv := loadInventory(t)
	caps := inv.byID()

	check := func(surface string, m map[string]string, status func(capability) string) {
		for item, capID := range m {
			if capID == "-" {
				continue
			}
			c, ok := caps[capID]
			if !ok {
				continue // TestLedgerPointsAtRealCapabilities reports this
			}
			if s := status(c); s == "no" || s == "n/a" {
				t.Errorf("%s reaches %q (mapped to capability %q), but that capability's "+
					"%s status is %q. Either the mapping is wrong or the surface gained the "+
					"capability and the row is stale — if it was just implemented, update the "+
					"status, the pointer, and the verdict.", surface, item, capID, surface, s)
			}
		}
	}
	check("cli", inv.Ledger.CLIVerbs, func(c capability) string { return c.CLI.Status })
	// Flags carry option capabilities, so they need the same agreement: mapping
	// `af sessions create --autoyes` while session.create.opt.autoyes still says
	// cli:no would otherwise pass with a stale row.
	check("cli", inv.Ledger.CLIFlags, func(c capability) string { return c.CLI.Status })
	check("tui", inv.Ledger.TUIBindings, func(c capability) string { return c.TUI.Status })
	check("web", inv.Ledger.WebRPCs, func(c capability) string { return c.Web.Status })
}

// TestVerdictAgreesWithStatuses stops a stale verdict outliving the statuses it
// was derived from: a row cannot claim "parity" while an applicable surface is
// still missing it, and cannot claim "real-gap" once every surface has it.
func TestVerdictAgreesWithStatuses(t *testing.T) {
	inv := loadInventory(t)

	for _, c := range inv.Capabilities {
		statuses := map[string]string{"tui": c.TUI.Status, "web": c.Web.Status, "cli": c.CLI.Status}

		var missing []string
		for name, s := range statuses {
			if s == "no" || s == "partial" {
				missing = append(missing, name+"="+s)
			}
		}
		sort.Strings(missing)

		switch c.Verdict {
		case "parity":
			if len(missing) > 0 {
				t.Errorf("%s: verdict 'parity' but %v. An applicable surface is missing it — "+
					"the verdict should be real-gap, deliberate, or unclear.", c.ID, missing)
			}
		case "real-gap", "unclear":
			if len(missing) == 0 {
				t.Errorf("%s: verdict %q but every surface reports yes/n-a. If the gap was "+
					"closed, flip the verdict to 'parity' and close %s.", c.ID, c.Verdict, c.Issue)
			}
		}
	}
}

// TestCapabilitiesAreWellFormed pins the quality bar: a row with a gap but no
// pointer is the noise this package exists to prevent.
func TestCapabilitiesAreWellFormed(t *testing.T) {
	inv := loadInventory(t)
	validStatus := map[string]bool{"yes": true, "no": true, "partial": true, "n/a": true}
	validVerdict := map[string]bool{"parity": true, "real-gap": true, "deliberate": true, "unclear": true}

	seen := map[string]bool{}
	for _, c := range inv.Capabilities {
		if seen[c.ID] {
			t.Errorf("duplicate capability id %q", c.ID)
		}
		seen[c.ID] = true

		if c.Title == "" {
			t.Errorf("%s: missing title", c.ID)
		}
		if !validVerdict[c.Verdict] {
			t.Errorf("%s: invalid verdict %q", c.ID, c.Verdict)
		}
		for name, cl := range map[string]cell{"tui": c.TUI, "web": c.Web, "cli": c.CLI} {
			if !validStatus[cl.Status] {
				t.Errorf("%s.%s: invalid status %q", c.ID, name, cl.Status)
			}
			// A surface that HAS a capability must say where. Absence needs no
			// pointer (you cannot cite code that does not exist), but presence
			// without one makes the row unverifiable.
			if (cl.Status == "yes" || cl.Status == "partial") && cl.Pointer == "" {
				t.Errorf("%s.%s: status %q needs a code pointer", c.ID, name, cl.Status)
			}
		}
		// Deliberate omissions MUST carry a reason or they get re-reported as
		// gaps by the next person to read the table.
		if c.Verdict == "deliberate" && c.Notes == "" {
			t.Errorf("%s: verdict 'deliberate' requires notes explaining why", c.ID)
		}
		if c.Verdict == "real-gap" && c.Issue == "" {
			t.Errorf("%s: verdict 'real-gap' requires an issue reference (use TBD while filing)", c.ID)
		}
	}
}
