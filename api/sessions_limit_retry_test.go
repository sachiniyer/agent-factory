package api

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

type committedLimitRetryError struct{ message string }

func (e committedLimitRetryError) Error() string         { return e.message }
func (committedLimitRetryError) MutationCommitted() bool { return true }

// TestSessionsRetryLimit_UsesSharedDaemonPath pins the surface-parity fix: the
// CLI must hand the selected session to the existing ResumeFromLimit daemon
// action, scoped exactly like the other title-addressed session mutations. The
// daemon owns every resume detail (respawn, pending-prompt delivery, and durable
// liveness clearing); the CLI must not grow a second implementation of them.
func TestSessionsRetryLimit_UsesSharedDaemonPath(t *testing.T) {
	repoID := setupRepoForCmd(t)

	var gotReq daemon.ResumeFromLimitRequest
	prev := resumeFromLimitViaDaemon
	resumeFromLimitViaDaemon = func(req daemon.ResumeFromLimitRequest) (daemon.ResumeFromLimitResponse, error) {
		gotReq = req
		return daemon.ResumeFromLimitResponse{OK: true}, nil
	}
	t.Cleanup(func() { resumeFromLimitViaDaemon = prev })

	out, err := runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	if err != nil {
		t.Fatalf("retry-limit returned error: %v", err)
	}
	if gotReq.Title != "worker" || gotReq.RepoID != repoID || gotReq.ID != "" {
		t.Fatalf("ResumeFromLimit request = %+v, want title %q scoped to repo %q with no web-only id", gotReq, "worker", repoID)
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("output is not JSON (%q): %v", out, err)
	}
	if payload["ok"] != true || payload["title"] != "worker" {
		t.Fatalf("JSON output = %v, want ok=true and title=worker", payload)
	}
}

// TestSessionsRetryLimit_SurfacesDaemonRejection keeps the CLI from claiming a
// retry happened when the shared daemon path says the target is not limit-
// blocked (or rejects it for any other reason).
func TestSessionsRetryLimit_SurfacesDaemonRejection(t *testing.T) {
	setupRepoForCmd(t)

	prev := resumeFromLimitViaDaemon
	resumeFromLimitViaDaemon = func(daemon.ResumeFromLimitRequest) (daemon.ResumeFromLimitResponse, error) {
		return daemon.ResumeFromLimitResponse{}, errors.New("session \"worker\" is not blocked on a usage limit")
	}
	t.Cleanup(func() { resumeFromLimitViaDaemon = prev })

	_, err := runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	if err == nil {
		t.Fatal("retry-limit must surface the daemon rejection")
	}
	if !strings.Contains(err.Error(), "not blocked on a usage limit") {
		t.Fatalf("error = %v, want the daemon's rejection message", err)
	}
}

func TestSessionsRetryLimit_PrintsCommittedSettlementWarningAsSuccess(t *testing.T) {
	setupRepoForCmd(t)

	prev := resumeFromLimitViaDaemon
	resumeFromLimitViaDaemon = func(daemon.ResumeFromLimitRequest) (daemon.ResumeFromLimitResponse, error) {
		return daemon.ResumeFromLimitResponse{OK: true}, committedLimitRetryError{message: "mission delivered; settlement pending"}
	}
	t.Cleanup(func() { resumeFromLimitViaDaemon = prev })

	out, err := runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	if err != nil {
		t.Fatalf("committed retry returned error: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("output is not JSON (%q): %v", out, err)
	}
	if payload["ok"] != true || payload["warning"] != "mission delivered; settlement pending" {
		t.Fatalf("JSON output = %v, want ok=true with committed warning", payload)
	}
}

// #6768: the retry output must hand the caller the conversation verdict the
// daemon computed — carried, or the stated fresh start — rather than ok:true
// alone, which previously hid a replacement that lost the transcript.
func TestSessionsRetryLimit_ReportsTheConversationOutcome(t *testing.T) {
	setupRepoForCmd(t)

	prev := resumeFromLimitViaDaemon
	resumeFromLimitViaDaemon = func(daemon.ResumeFromLimitRequest) (daemon.ResumeFromLimitResponse, error) {
		return daemon.ResumeFromLimitResponse{
			OK:           true,
			Conversation: session.HandoffConversation{CarryFailure: "af recorded no conversation id for this codex session"},
		}, nil
	}
	t.Cleanup(func() { resumeFromLimitViaDaemon = prev })

	out, err := runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(out, &payload))
	require.Equal(t,
		map[string]any{"carried": false, "carry_failure": "af recorded no conversation id for this codex session"},
		payload["conversation"])

	resumeFromLimitViaDaemon = func(daemon.ResumeFromLimitRequest) (daemon.ResumeFromLimitResponse, error) {
		return daemon.ResumeFromLimitResponse{
			OK:           true,
			Conversation: session.HandoffConversation{Carried: true},
		}, nil
	}
	out, err = runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(out, &payload))
	require.Equal(t, map[string]any{"carried": true}, payload["conversation"])
}

// #6768: --delivered retires the pending swap whose record held the carry
// verdict, so its response must answer the same question.
func TestSessionsRetryLimit_DeliveredReportsTheConversationOutcome(t *testing.T) {
	setupRepoForCmd(t)

	prev := confirmHandoffDeliveryViaDaemon
	confirmHandoffDeliveryViaDaemon = func(daemon.ConfirmHandoffDeliveryRequest) (daemon.ConfirmHandoffDeliveryResponse, error) {
		return daemon.ConfirmHandoffDeliveryResponse{
			OK:           true,
			Conversation: session.HandoffConversation{Carried: true},
		}, nil
	}
	t.Cleanup(func() { confirmHandoffDeliveryViaDaemon = prev })
	oldDelivered := sessionsRetryLimitDelivered
	sessionsRetryLimitDelivered = true
	t.Cleanup(func() { sessionsRetryLimitDelivered = oldDelivered })

	out, err := runCmdCaptureStdout(t, sessionsRetryLimitCmd, []string{"worker"})
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(out, &payload))
	require.Equal(t, true, payload["delivered"])
	require.Equal(t, map[string]any{"carried": true}, payload["conversation"])
}

// TestSessionsRetryLimit_OwedMissionSuggestionParsesVerbatim is the end of the
// #7803 chain: each remedy the owed-mission refusal PRINTS must be a command
// this command accepts. The refusal text comes from
// session.runtimeActionRefusal's pending-handoff-mission arm; here the two
// backtick-fenced commands it prints are split by a real POSIX shell — the
// tokenization a user pasting them gets — and the words after `af sessions
// retry-limit` run through this command's own flag parser and Args validator.
// The resend must yield exactly one positional (the title) with --delivered
// unset; the retire the same positional with --delivered set. The
// prose-described alternative — `-- <title> --delivered`, the flag appended
// after the terminator — is what both the raw-%s original and the
// first-superseded fix led the user to paste, and pflag reads that flag as a
// second positional, which cobra.ExactArgs(1) then refuses.
func TestSessionsRetryLimit_OwedMissionSuggestionParsesVerbatim(t *testing.T) {
	silenceCLIOutput(t)

	for _, title := range []string{"my session", "-worker", "don't; echo hi", "owing"} {
		t.Run(title, func(t *testing.T) {
			view := session.LifecycleView{
				Title: title, Liveness: session.LiveRunning, Started: true,
				PendingHandoffMission: true,
			}
			err := view.ValidateRuntimeAction(session.RuntimeActionHandoff)
			require.Error(t, err)

			parts := strings.Split(err.Error(), "`")
			require.Len(t, parts, 5, "the refusal must fence exactly two commands, got %q", err.Error())

			assertRetryLimitSuggestionParses(t, shellSplitWords(t, parts[1]), title, false)
			assertRetryLimitSuggestionParses(t, shellSplitWords(t, parts[3]), title, true)
		})
	}
}

// shellSplitWords has a real POSIX shell parse a printed command into the argv
// a paste would produce, without executing any of it: `set --` binds the words
// as positional parameters, so the command's own name never runs. This is the
// same check the shellsuggest suite runs against three shells; here one sh
// suffices because the assertion that matters is downstream — what the REAL
// cobra command does with the resulting argv.
func shellSplitWords(t *testing.T, cmdline string) []string {
	t.Helper()
	out, err := exec.Command("sh", "-c", `set -- `+cmdline+`; printf '%s\n' "$@"`).CombinedOutput()
	require.NoError(t, err, "the printed command must parse under sh: %s", out)
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// assertRetryLimitSuggestionParses feeds the words after `af sessions
// retry-limit` through the real leaf command's flag parser and Args validator
// — no daemon, no RunE — and pins the parse the printed suggestion must
// produce: exactly one positional, the session title, with --delivered in the
// stated state.
func assertRetryLimitSuggestionParses(t *testing.T, argv []string, title string, wantDelivered bool) {
	t.Helper()
	require.GreaterOrEqual(t, len(argv), 3,
		"each printed remedy must be a complete `af sessions retry-limit` command, got %q", argv)
	require.Equal(t, []string{"af", "sessions", "retry-limit"}, argv[:3],
		"the printed command must address retry-limit")

	resetRetryLimitParseState(t)
	require.NoError(t, sessionsRetryLimitCmd.ParseFlags(argv[3:]),
		"the command's flag parser must accept the printed tail %q", argv[3:])
	rest := sessionsRetryLimitCmd.Flags().Args()
	require.NoError(t, sessionsRetryLimitCmd.Args(sessionsRetryLimitCmd, rest),
		"the Args validator must accept the positionals %q", rest)
	require.Equal(t, []string{title}, rest, "exactly one positional: the title")
	require.Equal(t, wantDelivered, sessionsRetryLimitDelivered,
		"--delivered set only on the retire command")
}

// resetRetryLimitParseState restores the bound flag variable and cobra's
// Changed bit between parses, so flag state never leaks from one suggested
// command to the next — the same discipline resetSendPromptState applies to
// send-prompt's parser.
func resetRetryLimitParseState(t *testing.T) {
	t.Helper()
	prev := sessionsRetryLimitDelivered
	t.Cleanup(func() { sessionsRetryLimitDelivered = prev })
	sessionsRetryLimitDelivered = false
	if f := sessionsRetryLimitCmd.Flags().Lookup("delivered"); f != nil {
		f.Changed = false
	}
}
