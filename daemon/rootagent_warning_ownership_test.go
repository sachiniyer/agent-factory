package daemon

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestAForeignManagersWarningCannotSatisfyThisManagersAssertion is the #3787
// part-2 regression: the false pass, made deterministic.
//
// Part 1 put a mutex around the shared capture, which stopped the race. It could
// not stop the CONTAMINATION: a warning emitted by any Manager in the test
// binary still lands in whatever buffer is installed on the process-global
// logger, so "this Manager warned X" was satisfied by another Manager warning X.
// No re-run reveals that — the test is green either way.
//
// The fixture makes the two halves separable. The subject Manager is built while
// the personal config is healthy, so it has nothing to warn about and never
// does. The config is then corrupted and a SECOND Manager is built over it,
// which warns. Both assertions below then run against the same moment:
//
//   - the process-global capture IS satisfied — that is the defect, asserted so
//     this test fails if the contamination ever stops being reachable and the
//     rest of the test stops meaning anything;
//   - the subject's OWN logger is not.
//
// Before the m.warn() routing, the second assertion fails: the subject's logger
// did not exist, and every warning in the package went to the global sink.
func TestAForeignManagersWarningCannotSatisfyThisManagersAssertion(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = false")

	shared := captureWarnings(t)

	// The Manager this test is about. Its config loads, so it must stay silent —
	// and the assertion below is what makes that a fixture invariant rather than
	// an assumption.
	subject, subjectWarnings := newManagerCapturingWarnings(t, rootTestConfig(repoPath, config.RootAgentConfig{}))
	subject.ensureRootAgentsAndWait()
	if got := subjectWarnings.String(); strings.Contains(got, "failing closed") {
		t.Fatalf("fixture: the subject Manager warned, so this test proves nothing:\n%s", got)
	}

	// A Manager this test is NOT about, over a config that is broken.
	breakPersonalRootAgentToml(t, project.ID)
	foreign, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager(foreign): %v", err)
	}
	foreign.ensureRootAgentsAndWait()

	// The contamination is real and reachable: the shared sink every pre-#3787
	// assertion read carries the foreign Manager's warning.
	if got := shared.String(); !strings.Contains(got, "failing closed") || !strings.Contains(got, project.ID) {
		t.Fatalf("fixture: the foreign Manager's warning never reached the shared capture, "+
			"so the contamination this test is about was not exercised:\n%s", got)
	}

	// The property: the subject's own log is untouched by it.
	if got := subjectWarnings.String(); strings.Contains(got, "failing closed") || strings.Contains(got, project.ID) {
		t.Fatalf("a warning emitted by a Manager this test never asserted about landed in "+
			"the subject's own log; an assertion on it would pass on another Manager's "+
			"warning (#3787):\n%s", got)
	}

	// ANTI-VACUITY, and the assertion that makes the negative above mean
	// something. A per-Manager capture that was simply never wired to this
	// warning would satisfy that check too, and would keep satisfying it after a
	// refactor quietly took the routing back out. So: a Manager built the same
	// way, over the same broken config, must find the warning in ITS own log.
	// Same code path, same helper, opposite expectation.
	witness, witnessWarnings := newManagerCapturingWarnings(t, rootTestConfig(repoPath, config.RootAgentConfig{}))
	witness.ensureRootAgentsAndWait()
	if got := witnessWarnings.String(); !strings.Contains(got, "failing closed") || !strings.Contains(got, project.ID) {
		t.Fatalf("a Manager's own warning never reached its own log, so the negative "+
			"assertion above proves nothing — the routing is not wired:\n%s", got)
	}
}

// TestManagerWarningsReachTheGlobalLogByDefault is the other half, and it is not
// a formality: routing warnings to a per-Manager logger is only safe if a
// Manager built the production way still writes where the daemon log expects.
// A defaulting bug here would silence the daemon's warnings in production while
// every routed test stayed green.
func TestManagerWarningsReachTheGlobalLogByDefault(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = false")
	breakPersonalRootAgentToml(t, project.ID)

	warnings := captureWarnings(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.ensureRootAgentsAndWait()

	if got := warnings.String(); !strings.Contains(got, "failing closed") || !strings.Contains(got, project.ID) {
		t.Fatalf("a Manager built through NewManager must warn on the process-global log; got:\n%s", got)
	}
}

// TestNilManagerWarnFallsBackToTheGlobalLog pins the nil-receiver arm of warn().
// Manager methods are called on nil receivers in a few teardown paths, and a
// warn() that dereferenced would turn a diagnostic into a panic.
func TestNilManagerWarnFallsBackToTheGlobalLog(t *testing.T) {
	warnings := captureWarnings(t)
	var m *Manager
	m.warn().Printf("nil-receiver warning")
	if got := warnings.String(); !strings.Contains(got, "nil-receiver warning") {
		t.Fatalf("a nil Manager must warn on the process-global log; got %q", got)
	}
}
