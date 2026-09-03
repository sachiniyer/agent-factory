package daemon

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
)

// #3500: the root-agent sweeps narrated EVERY config.RepoFromPath failure as a
// claim about the path — "does not resolve to a git repository" — including the
// failures where git never answered. The live report was a `git rev-parse` child
// killed by its 100ms WaitDelay on a loaded box, against a directory
// `git rev-parse --show-toplevel` resolves perfectly well; the log sent a
// maintainer to audit a root_agents entry that was never the problem.
//
// These tests pin the split at both narrating sites. They are hermetic on the
// usual rules (temp AGENT_FACTORY_HOME, no real daemon); the git shim is what
// makes an unanswered probe reproducible.

// installUnanswerableGit puts a git shim first on PATH that dies on a signal
// before writing anything. exec reports a signalled exit as a negative exit
// code, which is the "no answer came back" shape #3500 is about — the same
// class as a WaitDelay-abandoned read, and deterministic enough for a test.
//
// The returned function makes the shim start answering again by handing every
// invocation to the real git, so one test can drive the transition #3500's
// review is about: a streak that begins unanswerable and later gets a verdict.
func installUnanswerableGit(t *testing.T) (letGitAnswer func()) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "unanswerable")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	script := fmt.Sprintf("#!/bin/sh\nif [ -f %q ]; then kill -9 $$; fi\nexec %q \"$@\"\n", marker, realGit)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() {
		if err := os.Remove(marker); err != nil {
			t.Fatalf("let git answer: %v", err)
		}
	}
}

// captureRootEnsureLogs redirects the warning and error loggers for one test.
func captureRootEnsureLogs(t *testing.T) (warnings, errors *bytes.Buffer) {
	t.Helper()
	warnings, errors = &bytes.Buffer{}, &bytes.Buffer{}
	prevWarning, prevError := log.WarningLog.Writer(), log.ErrorLog.Writer()
	log.WarningLog.SetOutput(warnings)
	log.ErrorLog.SetOutput(errors)
	t.Cleanup(func() {
		log.WarningLog.SetOutput(prevWarning)
		log.ErrorLog.SetOutput(prevError)
	})
	return warnings, errors
}

// TestRootEnsureDoesNotNarrateAnUnansweredProbeAsNotARepository is the #3500
// headline regression: one ensure pass whose repo probe never completed must
// report the subprocess outcome, not a verdict on the configured path.
func TestRootEnsureDoesNotNarrateAnUnansweredProbeAsNotARepository(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	path := t.TempDir()
	manager, err := NewManager(rootTestConfig(path, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Shim git only once the manager exists, so nothing but the ensure pass
	// under test sees an unanswerable probe.
	_ = installUnanswerableGit(t)
	warnings, _ := captureRootEnsureLogs(t)
	manager.ensureRootAgentsAndWait()

	got := warnings.String()
	if !strings.Contains(got, "root agent ensure for") {
		t.Fatalf("expected the ensure failure to be logged, got: %q", got)
	}
	if strings.Contains(got, "does not resolve to a git repository") {
		t.Fatalf("an unanswered probe was narrated as a verdict on the path — the #3500 defect: %q", got)
	}
	if !strings.Contains(got, "git never answered") {
		t.Fatalf("the message must name the subprocess outcome that actually happened, got: %q", got)
	}
}

// TestRootEnsureStillNarratesAnAnsweredRefusalAsAPathFailure is the
// over-correction guard: when git DID answer — an ordinary directory that is
// not a repository — the definite claim is the honest one and must survive.
func TestRootEnsureStillNarratesAnAnsweredRefusalAsAPathFailure(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	path := t.TempDir() // a real directory, and really not a git repository
	manager, err := NewManager(rootTestConfig(path, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	warnings, _ := captureRootEnsureLogs(t)
	manager.ensureRootAgentsAndWait()

	got := warnings.String()
	if !strings.Contains(got, "does not resolve to a git repository") {
		t.Fatalf("git answered, and the answer is no: that claim must still be made, got: %q", got)
	}
	if strings.Contains(got, "git never answered") {
		t.Fatalf("an answered refusal must not be softened into an unanswered probe: %q", got)
	}
}

// TestRootEnsureEscalationDoesNotAssertPersistenceFromUnansweredProbes covers
// the second half of #3500. Unanswered attempts still back off — the cadence is
// what keeps a loaded box from asking git every tick, and #1122's
// retry-forever contract is untouched — but crossing the escalation threshold
// must not turn probes that established nothing into an ERROR asserting a
// persistent cause.
func TestRootEnsureEscalationDoesNotAssertPersistenceFromUnansweredProbes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	// Zero backoff so every pass is an attempt.
	prevBase := rootEnsureBackoffBase
	rootEnsureBackoffBase = 0
	t.Cleanup(func() { rootEnsureBackoffBase = prevBase })

	path := t.TempDir()
	manager, err := NewManager(rootTestConfig(path, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	_ = installUnanswerableGit(t)
	_, errorLog := captureRootEnsureLogs(t)
	for i := 0; i < rootEnsureEscalationThreshold; i++ {
		manager.ensureRootAgentsAndWait()
	}

	got := errorLog.String()
	if !strings.Contains(got, fmt.Sprintf("failed %d consecutive times", rootEnsureEscalationThreshold)) {
		t.Fatalf("expected the escalation ERROR after %d failures, got: %q", rootEnsureEscalationThreshold, got)
	}
	if strings.Contains(got, "the cause looks persistent") {
		t.Fatalf("every attempt died before git answered; nothing here establishes a persistent cause: %q", got)
	}
	if !strings.Contains(got, "no attempt got an answer out of git") {
		t.Fatalf("the escalation must say what it actually knows, got: %q", got)
	}

	// The backoff itself is unchanged: unanswered attempts still count, or the
	// loop would ask git every tick for as long as the box stays loaded.
	manager.mu.Lock()
	st := manager.rootEnsureStates[path]
	manager.mu.Unlock()
	if st == nil || st.consecutiveFailures != rootEnsureEscalationThreshold {
		t.Fatalf("unanswered attempts must still count toward the retry cadence, got %+v", st)
	}
}

// TestRootEnsureEscalationStillAssertsPersistenceForAnsweredFailures: the
// escalation copy that #1122 added is right whenever git actually answered, and
// must not be weakened for those.
func TestRootEnsureEscalationStillAssertsPersistenceForAnsweredFailures(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	prevBase := rootEnsureBackoffBase
	rootEnsureBackoffBase = 0
	t.Cleanup(func() { rootEnsureBackoffBase = prevBase })

	path := t.TempDir()
	manager, err := NewManager(rootTestConfig(path, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	_, errorLog := captureRootEnsureLogs(t)
	for i := 0; i < rootEnsureEscalationThreshold; i++ {
		manager.ensureRootAgentsAndWait()
	}

	if got := errorLog.String(); !strings.Contains(got, "the cause looks persistent") {
		t.Fatalf("six answered refusals ARE a persistent cause; that ERROR must survive, got: %q", got)
	}
}

// TestRootAgentSnapshotDoesNotNarrateAnUnansweredProbeAsNotARepository covers
// the sibling site: the snapshot's project sweep narrates the same repoErr the
// same way, and had the same defect.
func TestRootAgentSnapshotDoesNotNarrateAnUnansweredProbeAsNotARepository(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	// A well-formed project id keeps the personal-config read (and its own
	// fail-closed warning) out of the buffer this test reads.
	project := config.Project{ID: "prj_0123456789abcdef0123456789abcdef", Root: t.TempDir()}

	_ = installUnanswerableGit(t)
	warnings, _ := captureRootEnsureLogs(t)
	_, _, projectRoots, unresolvedRoots := projectRootAgentLayers([]config.Project{project})

	got := warnings.String()
	if strings.Contains(got, "does not resolve to a git repository") {
		t.Fatalf("the snapshot narrated an unanswered probe as a verdict on the project root: %q", got)
	}
	if !strings.Contains(got, "git never answered") {
		t.Fatalf("the snapshot must name the subprocess outcome, got: %q", got)
	}
	// The fail-open handling itself is unchanged: the root is still recorded as
	// unresolved for this run, and the singleton sweep still starts nothing for
	// it. Only the claim the log makes about WHY changes.
	if _, ok := projectRoots[config.RepoIDForRecordedRoot(project.Root)]; ok {
		t.Fatalf("an unresolved root must stay out of projectRoots")
	}
	if _, ok := unresolvedRoots[config.RepoIDForRecordedRoot(project.Root)]; !ok {
		t.Fatalf("an unresolved root must still be recorded as unresolved")
	}
}

// TestRootEnsureReEscalatesOnceTheCauseIsFinallyEstablished: a streak that
// begins with unanswerable probes escalates as "cause unknown", and when git
// LATER starts answering with a real refusal the now-established cause gets its
// own ERROR. Without that second escalation the strict threshold equality could
// never fire again — the count is already past it — so a genuine persistent
// failure would be logged as warnings forever while the root stayed down
// (#3500 review).
func TestRootEnsureReEscalatesOnceTheCauseIsFinallyEstablished(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	prevBase := rootEnsureBackoffBase
	rootEnsureBackoffBase = 0
	t.Cleanup(func() { rootEnsureBackoffBase = prevBase })

	path := t.TempDir() // a real directory, and really not a git repository
	manager, err := NewManager(rootTestConfig(path, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	letGitAnswer := installUnanswerableGit(t)
	_, errorLog := captureRootEnsureLogs(t)
	for i := 0; i < rootEnsureEscalationThreshold; i++ {
		manager.ensureRootAgentsAndWait()
	}
	if got := errorLog.String(); !strings.Contains(got, "no attempt got an answer out of git") {
		t.Fatalf("expected the unknown-cause escalation first, got: %q", got)
	}

	// git recovers and starts answering: the directory is not a repository.
	// ONE answered failure is not evidence of a persistent cause, and this is
	// not a hypothetical distinction — rootEnsureFailed also records a failed
	// session create and a failed dead-root reap, so a single transient tmux
	// failure here must not upgrade "cause unknown" to "looks persistent"
	// (#3500 review round 2).
	errorLog.Reset()
	letGitAnswer()
	manager.ensureRootAgentsAndWait()
	if got := errorLog.String(); got != "" {
		t.Fatalf("one answered failure is not a persistent cause; it must not re-escalate: %q", got)
	}

	// A full threshold of answered failures is the same bar the first
	// escalation cleared. Now the established cause gets its ERROR.
	for i := 1; i < rootEnsureEscalationThreshold; i++ {
		manager.ensureRootAgentsAndWait()
	}

	got := errorLog.String()
	if !strings.Contains(got, "the cause looks persistent") {
		t.Fatalf("an established cause after an unknown-only escalation must escalate again, got: %q", got)
	}
	if !strings.Contains(got, fmt.Sprintf("though %d of those attempts ended before git could answer", rootEnsureEscalationThreshold)) {
		t.Fatalf("the second ERROR must still account for the attempts that established nothing, got: %q", got)
	}

	// And it stays at two: further answered failures are the same cause, and
	// must not re-log the ERROR on every pass.
	errorLog.Reset()
	for i := 0; i < 3; i++ {
		manager.ensureRootAgentsAndWait()
	}
	if got := errorLog.String(); got != "" {
		t.Fatalf("the escalation must not repeat once the cause is established: %q", got)
	}
}

// TestRootEnsureMixedStreakNeitherClaimsPersistenceNorLocksOutTheUpgrade: a
// first streak that crosses the threshold on MOSTLY unanswered probes plus one
// real error must not claim a persistent cause on that one failure — and it
// must stay eligible for the upgrade, which is what an escalation flag keyed to
// "was the streak all-unanswered" got wrong: a mixed streak recorded itself as
// already-established and could never escalate again (#3500 review round 3).
func TestRootEnsureMixedStreakNeitherClaimsPersistenceNorLocksOutTheUpgrade(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	prevBase := rootEnsureBackoffBase
	rootEnsureBackoffBase = 0
	t.Cleanup(func() { rootEnsureBackoffBase = prevBase })

	path := t.TempDir() // a real directory, and really not a git repository
	manager, err := NewManager(rootTestConfig(path, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	letGitAnswer := installUnanswerableGit(t)
	_, errorLog := captureRootEnsureLogs(t)

	// One short of the threshold, all unanswered; then git answers, and that
	// single real error is what crosses it.
	for i := 0; i < rootEnsureEscalationThreshold-1; i++ {
		manager.ensureRootAgentsAndWait()
	}
	letGitAnswer()
	manager.ensureRootAgentsAndWait()

	got := errorLog.String()
	if !strings.Contains(got, fmt.Sprintf("failed %d consecutive times", rootEnsureEscalationThreshold)) {
		t.Fatalf("the streak still crossed the threshold and still owes an ERROR, got: %q", got)
	}
	if strings.Contains(got, "the cause looks persistent") {
		t.Fatalf("one real error among %d unanswered probes is not a persistent cause: %q", rootEnsureEscalationThreshold-1, got)
	}
	if !strings.Contains(got, "so the cause is not established") {
		t.Fatalf("the ERROR must say what the streak actually established, got: %q", got)
	}

	// The evidence arrives: a full threshold of real errors. The upgrade must
	// still be available — this is the half a streak-class flag locked out.
	errorLog.Reset()
	for i := 1; i < rootEnsureEscalationThreshold; i++ {
		manager.ensureRootAgentsAndWait()
	}
	if got := errorLog.String(); !strings.Contains(got, "the cause looks persistent") {
		t.Fatalf("a mixed first escalation must stay eligible for the upgrade, got: %q", got)
	}
}
