package daemon

// The #3477 regression suite: the not-attempted classification is carried over
// net/rpc by a SUBSTRING of the error text (notDeliveredMarker), and twice now a
// message that reads perfectly well to a human has broken it by saying "event
// not delivered" instead. A substring contract enforced only by prose will break
// again, so it is enforced two ways here:
//
//   - notAttempted() GUARANTEES the marker, so no call site — present, future, or
//     passing an error minted somewhere else entirely — can cost the watcher its
//     rate-slot refund. TestNotAttempted* pins that.
//   - TestNotAttemptedCallSitesSpellTheMarkerExactly walks the package AST and
//     fails on a near-miss wording, so the guarantee stays a floor rather than
//     becoming the mechanism and leaving awkward appended clauses behind.
//
// TestRootAgentDeliveryRefusalsSurviveRPCFlattening then proves the property end
// to end on the two paths #3477 actually reports, using the real manager rather
// than a copied string.

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestNotAttemptedGuaranteesTheWireMarker is the structural half of the fix: the
// invariant holds for ANY message, so a future call site that never heard of the
// marker still refunds. Each case is fed through the exact net/rpc round trip the
// watcher sees — the type is destroyed, only the text comes back.
func TestNotAttemptedGuaranteesTheWireMarker(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{
			name: "the #3477 fail-closed wording",
			msg:  `root agent for "/repo" will not materialize: disabled; event not delivered`,
		},
		{
			name: "the #3477 retryable wording",
			msg:  `root agent for "/repo" is being recreated (tmux momentarily absent); event not delivered this attempt: timed out`,
		},
		{
			name: "a pre-flight message that says nothing about delivery at all",
			msg:  "prompt is required",
		},
		{
			name: "a message that is only a wrapped cause",
			msg:  "failed to load config: permission denied",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := notAttempted(errors.New(tc.msg))
			if !strings.Contains(err.Error(), notDeliveredMarker) {
				t.Fatalf("notAttempted must guarantee the wire marker %q; got %q", notDeliveredMarker, err.Error())
			}

			// Exactly what net/rpc does: reconstitute the server error from its
			// text only. Assert the precondition, so this cannot pass for the
			// wrong reason the way an in-process type check would.
			flattened := fmt.Errorf("%s", err.Error())
			if errors.Is(flattened, errNotAttempted) {
				t.Fatal("precondition failed: flattening did not destroy the type, so this proves nothing about the wire")
			}
			if !isNotAttemptedErr(flattened) {
				t.Fatalf("a pre-flight failure must stay recognizable after net/rpc flattens it; wire text: %q", flattened.Error())
			}
		})
	}
}

// TestNotAttemptedKeepsAlreadyMarkedMessagesVerbatim pins the other half of the
// guarantee. #2512 made this wrapper transparent so nothing internal trails a
// pasteable command; appending unconditionally would put a second marker after
// the `af sessions restore …` suggestion the Archived refusal ends with. The
// message below is the real one from promptTargetLivenessError.
func TestNotAttemptedKeepsAlreadyMarkedMessagesVerbatim(t *testing.T) {
	msg := `target session "captain" is Archived; prompt not delivered; restore it first (af sessions restore -- captain)`
	got := notAttempted(errors.New(msg)).Error()
	if got != msg {
		t.Fatalf("an already-marked message must pass through verbatim (#2512)\n got: %q\nwant: %q", got, msg)
	}
	if n := strings.Count(got, notDeliveredMarker); n != 1 {
		t.Fatalf("marker appears %d times, want 1 — the append must not duplicate it: %q", n, got)
	}
}

// TestNotAttemptedPreservesTheCauseChain: the append inserts a layer between the
// tag and the cause, so errors.Is/As against that cause must still work — the
// concurrency-limit and liveness classifications elsewhere depend on it.
func TestNotAttemptedPreservesTheCauseChain(t *testing.T) {
	cause := errors.New("the underlying failure")
	err := notAttempted(fmt.Errorf("could not check target session state: %w", cause))

	if !errors.Is(err, cause) {
		t.Fatalf("the marker append severed the cause chain: %v", err)
	}
	if !errors.Is(err, errNotAttempted) {
		t.Fatalf("the in-process sentinel must survive the extra layer: %v", err)
	}
	if notAttempted(nil) != nil {
		t.Fatal("nil in, nil out")
	}
}

// notDeliveredPhrase matches any "not delivered" wording a human might reach for
// while writing a pre-flight refusal — "event not delivered", "prompt was not
// delivered", "nothing not delivered". Reaching for one of those and not landing
// on the exact marker is precisely how #3477 happened, twice.
var notDeliveredPhrase = regexp.MustCompile(`(?i)not\s+delivered`)

// TestNotAttemptedCallSitesSpellTheMarkerExactly walks package daemon's AST and
// fails on a NEW near-miss rather than on a hand-list of today's call sites: any
// string literal inside a notAttempted(...) argument that reaches for the
// "not delivered" phrasing must spell notDeliveredMarker exactly.
//
// Scope, stated honestly. This lints the WORDING, not the invariant — the
// invariant is total because notAttempted() enforces it (see above), which is
// what covers the passthrough sites like notAttempted(err), whose message is
// composed in another function the AST here never sees. So a call site that
// omits any delivery phrasing is silent here and harmless at runtime; a call
// site that writes a plausible-but-wrong variant is caught here, before it lands
// as a redundant appended clause in front of a user.
func TestNotAttemptedCallSitesSpellTheMarkerExactly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	callSites, markedLiterals := 0, 0
	var violations []string

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || fn.Name != "notAttempted" {
				return true
			}
			callSites++
			// Every string literal in the argument subtree, so a wrapped
			// fmt.Errorf several levels down is still inspected.
			ast.Inspect(call, func(m ast.Node) bool {
				lit, ok := m.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				text, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					return true
				}
				switch {
				case strings.Contains(text, notDeliveredMarker):
					markedLiterals++
				case notDeliveredPhrase.MatchString(text):
					violations = append(violations, fmt.Sprintf("  %s: %q", fset.Position(lit.Pos()), text))
				}
				return true
			})
			return true
		})
	}

	// Guard against a scan that silently matched nothing and "passed" — a green
	// result here has to mean the code was read, not that the walk was broken.
	if callSites < 10 {
		t.Fatalf("the AST walk found only %d notAttempted(...) call sites in package daemon; the scan is broken, not the code", callSites)
	}
	if markedLiterals == 0 {
		t.Fatalf("the AST walk found %d call sites but not one literal carrying %q; literal extraction is broken", callSites, notDeliveredMarker)
	}

	if len(violations) > 0 {
		t.Fatalf("a notAttempted(...) message reaches for \"not delivered\" but does not spell the wire marker %q (#3477):\n%s\n\n"+
			"That phrase is the PROTOCOL: isNotAttemptedErr re-mints the sentinel by substring-matching it after net/rpc\n"+
			"flattens the error, and the watcher refunds the event's rate slot only on a match. Use the notDeliveredMarker\n"+
			"constant in the format string rather than writing the phrase by hand.", notDeliveredMarker, strings.Join(violations, "\n"))
	}
}

// failClosedRootFixture: a repo whose root agent will never materialize this run
// (resolved disable), so a delivery to the reserved root title is refused
// pre-flight and nothing is ever sent.
func failClosedRootFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	manager, repoPath, _, _ := rootCauseFixture(t, "enabled = false", nil)
	return manager, repoPath
}

// recreatingRootFixture: a repo whose root agent WILL materialize but is absent
// right now and never returns within the wait — the tmux-blip outage path a
// monitor task targeting `root` hits, and the one whose refusal is retryable.
func recreatingRootFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installRecordingBackend(t)
	repoPath := setupControlRepo(t)

	// Bound the wait tightly so the timeout path is exercised fast, not the real 30s.
	origWait := targetDeliverWait
	targetDeliverWait = 150 * time.Millisecond
	t.Cleanup(func() { targetDeliverWait = origWait })

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager, repoPath
}

// rootRefusalCases are the two deliverToReemergingRoot pre-flight refusals, both
// tagged notAttempted and both of which wrote the marker as "event not
// delivered" before #3477. Shared with the watcher-level rate-slot regression.
//
// Measured on master, and worth recording because the two behaved DIFFERENTLY:
// only the fail-closed refusal actually leaked (its composed text carried no
// marker at all, so the flattening and rate-slot tests here went red on it). The
// retryable one was rescued by accident — it wraps waitForTargetSession, whose
// every return path happens to say "prompt not delivered", so the marker rode in
// on the CAUSE. That rescue is transitive and unowned: it evaporates the moment a
// wait path is reworded or a new one is added. It is exactly why the wording lint
// exists alongside these runtime tests — the lint is the only check that went red
// on rootagent.go:604, and the notAttempted() guarantee is what stops a future
// rescue-less variant from leaking at all.
var rootRefusalCases = []struct {
	name string
	// wantIn identifies the path, so a fixture that drifts into some OTHER
	// refusal (one that happens to carry the marker) fails loudly instead of
	// passing for the wrong reason.
	wantIn string
	build  func(t *testing.T) (*Manager, string)
}{
	{name: "fail-closed: the root will not materialize", wantIn: "will not materialize", build: failClosedRootFixture},
	{name: "retryable: the root is being recreated", wantIn: "being recreated", build: recreatingRootFixture},
}

// TestRootAgentDeliveryRefusalsSurviveRPCFlattening drives the REAL manager down
// both refusal paths and flattens the result exactly as net/rpc does on the way
// back to the watcher. It asserts on production text rather than a copy, so
// rewording a message can never quietly move the assertion with it.
func TestRootAgentDeliveryRefusalsSurviveRPCFlattening(t *testing.T) {
	for _, tc := range rootRefusalCases {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoPath := tc.build(t)
			repo, err := config.RepoFromPath(repoPath)
			if err != nil {
				t.Fatalf("RepoFromPath: %v", err)
			}

			_, _, handled, derr := manager.deliverToReemergingRoot(repo, DeliverPromptRequest{
				Title:  session.RootSessionTitle,
				Prompt: "monitor-event",
			})
			if !handled || derr == nil {
				t.Fatalf("precondition: this delivery must be handled with an error; handled=%v err=%v", handled, derr)
			}
			if !strings.Contains(derr.Error(), tc.wantIn) {
				t.Fatalf("precondition: the fixture took the wrong path — %q not in %q", tc.wantIn, derr.Error())
			}
			if !errors.Is(derr, errNotAttempted) {
				t.Fatalf("the manager must tag this pre-flight refusal notAttempted in-process: %v", derr)
			}

			flattened := fmt.Errorf("%s", derr.Error())
			if errors.Is(flattened, errNotAttempted) {
				t.Fatal("precondition failed: flattening did not destroy the type, so this proves nothing about the wire")
			}
			if !isNotAttemptedErr(flattened) {
				t.Fatalf("#3477: the refusal lost its classification across the RPC hop, so the watcher will not refund the rate slot.\n"+
					"wire text: %q\nit must contain the marker %q", flattened.Error(), notDeliveredMarker)
			}
		})
	}
}
