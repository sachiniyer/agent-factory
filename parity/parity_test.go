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
	"strings"
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
	CLIVerbs    map[string]string `json:"cli_verbs"`
	Routes      map[string]string `json:"routes"`
	TUIBindings map[string]string `json:"tui_bindings"`
	WebRPCs     map[string]string `json:"web_rpcs"`
}

type createOptions struct {
	Route       string            `json:"route"`
	WebSends    []string          `json:"web_sends"`
	UnsentByWeb map[string]string `json:"unsent_by_web"`
}

type inventory struct {
	Capabilities  []capability  `json:"capabilities"`
	Ledger        ledger        `json:"ledger"`
	CreateOptions createOptions `json:"create_options"`
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
	derived := deriveCLI(t)

	if missing := diff(keysOf(derived), keysOf(inv.Ledger.CLIVerbs)); len(missing) > 0 {
		t.Errorf("CLI verbs with no inventory entry: %v%s", missing, fixHint)
	}
	// A verb removed from the tree but left in the inventory is drift too: the
	// table would keep advertising a capability af no longer has.
	if stale := diff(keysOf(inv.Ledger.CLIVerbs), keysOf(derived)); len(stale) > 0 {
		t.Errorf("inventory lists CLI verbs that no longer exist: %v", stale)
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
	check("routes", inv.Ledger.Routes)
	check("tui_bindings", inv.Ledger.TUIBindings)
	check("web_rpcs", inv.Ledger.WebRPCs)
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

// TestCreateOptionParity is the targeted check for the option dimension — the
// axis the owner's remote-instance report lives on. "Create a session" being
// present on all three surfaces hides the fact that they accept different
// options, so the fields are pinned explicitly.
func TestCreateOptionParity(t *testing.T) {
	inv := loadInventory(t)
	routes := deriveRoutes(t)

	r, ok := routes[inv.CreateOptions.Route]
	if !ok {
		t.Fatalf("inventory's create route %q is not in the daemon catalog", inv.CreateOptions.Route)
	}

	// What the web actually sends today, straight from its call site.
	got := webCallBody(t, "CreateSession")
	want := append([]string(nil), inv.CreateOptions.WebSends...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("web's CreateSession body changed.\n  sends now: %v\n  inventory: %v\n\n"+
			"If the web gained a create option, update create_options.web_sends AND the "+
			"matching session.create.opt.* verdict — that is the whole point of this row.",
			got, want)
	}

	// Every field the route accepts is either sent by the web or declared as a
	// known gap. A NEW field on the wire struct lands here first.
	sends := map[string]bool{}
	for _, f := range got {
		sends[f] = true
	}
	for _, f := range r.Fields {
		if sends[f] {
			continue
		}
		capID, declared := inv.CreateOptions.UnsentByWeb[f]
		if !declared {
			t.Errorf("CreateSession accepts %q but the web never sends it, and the inventory "+
				"does not declare it as a gap.%s", f, fixHint)
			continue
		}
		if _, ok := inv.byID()[capID]; !ok {
			t.Errorf("create_options.unsent_by_web[%q] names unknown capability %q", f, capID)
		}
	}

	// And nothing is declared a gap that the web has quietly started sending —
	// that would mean a gap got fixed and the table still calls it broken.
	for f := range inv.CreateOptions.UnsentByWeb {
		if sends[f] {
			t.Errorf("inventory declares %q unsent by the web, but the web now sends it: "+
				"update create_options and the capability's verdict.", f)
		}
	}
}
