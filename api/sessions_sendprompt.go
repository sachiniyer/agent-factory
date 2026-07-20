package api

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/spf13/cobra"
)

var (
	sendPromptCreateFlag  bool
	sendPromptProgramFlag string
	// sendPromptPromptFlag is an ALIAS for the positional <prompt>, not a
	// replacement: its sibling `sessions create` takes the prompt as --prompt,
	// so a user who learned that verb hit "unknown flag: --prompt" here — same
	// concept, two shapes, two siblings in one noun group. Accepting both means
	// either spelling works and no existing caller changes.
	sendPromptPromptFlag      string
	sendPromptAllFlag         bool
	sendPromptAllReposFlag    bool
	sendPromptIncludeRootFlag bool
)

// promptFlagGiven reports whether --prompt was passed, empty value included.
// `--prompt ""` is the flag being GIVEN with an empty value — a script whose
// variable came back unset writes exactly that — not the flag being omitted, so
// this asks cobra's Changed bit rather than testing the value (#2139). Testing
// `sendPromptPromptFlag != ""` instead made an empty flag look like no flag: the
// arity check went looking for a positional <prompt> the user never meant to
// type and died on a misleading "takes exactly 2 positional argument(s)", while
// the positional spelling of the same thing (`send-prompt <title> ""`) sailed
// through. Same reasoning as --target-session in tasks.go.
func promptFlagGiven(cmd *cobra.Command) bool {
	return cmd.Flags().Changed("prompt")
}

// sendPromptArgCount is how many positionals send-prompt expects in the current
// mode. The prompt comes from --prompt or the last positional, and --all drops
// the <title>, so the arity is a 2x2 rather than a constant.
func sendPromptArgCount(cmd *cobra.Command) int {
	want := 2 // <title> <prompt>
	if sendPromptAllFlag {
		want-- // broadcast has no target title
	}
	if promptFlagGiven(cmd) {
		want-- // the prompt came from the flag
	}
	return want
}

// sendPromptUsage names the exact invocation the current flags imply, so an
// arity error tells the user what to type instead of just counting (#658/#734:
// a public CLI owes actionable errors).
func sendPromptUsage(cmd *cobra.Command) string {
	switch {
	case sendPromptAllFlag && promptFlagGiven(cmd):
		return "af sessions send-prompt --all --prompt <prompt> (no positional arguments)"
	case sendPromptAllFlag:
		return "af sessions send-prompt --all <prompt>"
	case promptFlagGiven(cmd):
		return "af sessions send-prompt <title> --prompt <prompt>"
	default:
		return "af sessions send-prompt <title> <prompt>"
	}
}

// resolveSendPrompt returns the prompt for this invocation and the positionals
// with it removed, so callers read the title from a consistent place regardless
// of which spelling the user chose.
func resolveSendPrompt(cmd *cobra.Command, args []string) (prompt string, rest []string) {
	if promptFlagGiven(cmd) {
		return sendPromptPromptFlag, args
	}
	if len(args) == 0 {
		return "", args
	}
	return args[len(args)-1], args[:len(args)-1]
}

var sessionsSendPromptCmd = &cobra.Command{
	Use:   "send-prompt <title> <prompt>",
	Short: "Send a prompt to a session (or broadcast to all with --all)",
	Long: `Send a prompt to an existing session. The session must already exist unless --create is used.

If the session does not exist, use --create to automatically create it first,
or use 'af sessions create --name <title> --prompt <prompt>' instead.

With --all, broadcast a single prompt to every live session in scope:

    af sessions send-prompt --all "<prompt>"

Broadcast scope defaults to the current repo (honoring --repo). Pass --all-repos
to broadcast across every repo. The reserved root session is excluded unless
--include-root is given. Delivery is best-effort per session: unreachable (Lost,
Dead) and Archived sessions are skipped and reported, and one failure never
aborts the rest. The command prints a JSON summary (delivered / failed / skipped)
and exits 0 even when some sessions fail, so scripts can inspect per-session
results.`,
	// Validate flag combinations before arity (cobra runs Args before RunE):
	// a broadcast-implying flag without --all must surface its actionable
	// message here, not cobra's generic "accepts 2 arg(s)" (#658/#734: public
	// CLI = actionable errors). Arity is then mode-aware — with --all the single
	// positional is the prompt; otherwise it's <title> <prompt>. Flags are
	// parsed before Args runs, so the mode flags are already set here.
	Args: func(cmd *cobra.Command, args []string) error {
		if err := validateSendPromptFlags(); err != nil {
			return jsonError(err)
		}
		if want := sendPromptArgCount(cmd); len(args) != want {
			return jsonError(fmt.Errorf("%s takes exactly %d positional argument(s); got %d",
				sendPromptUsage(cmd), want, len(args)))
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// Re-check here too: unit tests drive RunE directly (bypassing Args),
		// and it is cheap defense-in-depth against a future caller that skips
		// arg validation. In the real CLI Args already caught these.
		if err := validateSendPromptFlags(); err != nil {
			return jsonError(err)
		}
		// Re-check arity here too: unit tests drive RunE directly (bypassing
		// Args), so indexing rest[0] below must not depend on Args having run.
		if len(args) != sendPromptArgCount(cmd) {
			return jsonError(fmt.Errorf("%s takes exactly %d positional argument(s); got %d",
				sendPromptUsage(cmd), sendPromptArgCount(cmd), len(args)))
		}

		prompt, rest := resolveSendPrompt(cmd, args)
		if sendPromptAllFlag {
			return runBroadcast(prompt)
		}

		title := rest[0]

		// Honor --repo scoping (#776, follow-up to #761/#775). An empty repoID
		// preserves the prior all-repo search; a non-empty one confines both
		// the existence pre-check and the delivery to that repo so a
		// same-titled session in a different repo never receives the prompt.
		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		// --create routes through the daemon's serialized create-or-send path
		// so a session that pops into existence concurrently (another
		// --create, or a task delivering into the same target_session) is
		// delivered into rather than racing creation and dropping a prompt
		// (#865). The daemon decides create-vs-send under its per-target lock,
		// so no existence pre-check is needed here.
		if sendPromptCreateFlag {
			// resolveRepo distinguishes absent --repo ("--repo is required")
			// from a provided-but-invalid path and names it (#892). --create is
			// the only send-prompt mode that needs a resolvable repo, so surface
			// that error directly rather than relabeling an invalid path as
			// "required".
			repo, repoErr := resolveRepo()
			if repoErr != nil {
				return jsonError(repoErr)
			}

			if !git.IsGitRepo(repo.Root) {
				return jsonError(fmt.Errorf("path %s is not a git repository", repo.Root))
			}

			cfg, err := config.ResolveConfig(repo.Root)
			if err != nil {
				return jsonError(err)
			}

			program := sendPromptProgramFlag
			if program == "" {
				program = cfg.DefaultProgram
			} else if err := config.ValidateProgramEnum("--program flag", "--program flag", program, ""); err != nil {
				return jsonError(err)
			}
			if err := preflightLocalSession(&cfg.Config, program); err != nil {
				return jsonError(err)
			}

			if _, err := deliverPromptViaDaemon(daemon.DeliverPromptRequest{
				Title:    title,
				RepoPath: repo.Root,
				Program:  program,
				Prompt:   prompt,
				AutoYes:  cfg.AutoYes,
			}); err != nil {
				return jsonError(err)
			}
			return jsonOut(map[string]bool{"ok": true})
		}

		exists, err := instanceTitleExistsInScope(repoID, title)
		if err != nil {
			return jsonError(err)
		}
		if !exists {
			return jsonError(fmt.Errorf("session %q not found. Use --create to auto-create the session, or run: %s --prompt <prompt>", title, shellsuggest.Command("af", "sessions", "create", "--name", title)))
		}

		if err := sendPromptViaDaemon(daemon.SendPromptRequest{Title: title, RepoID: repoID, Prompt: prompt}); err != nil {
			return jsonError(err)
		}
		return jsonOut(map[string]bool{"ok": true})
	},
}

// broadcastResult is the JSON summary `send-prompt --all` prints: aggregate
// counts plus a per-session breakdown so scripts can tell exactly which
// sessions received the prompt and why any did not.
type broadcastResult struct {
	Prompt    string            `json:"prompt"`
	Scope     string            `json:"scope"`
	Delivered int               `json:"delivered"`
	Failed    int               `json:"failed"`
	Skipped   int               `json:"skipped"`
	Results   []broadcastTarget `json:"results"`
}

// broadcastTarget is one session's outcome in a broadcast. Status is one of
// "delivered", "failed", or "skipped"; Error carries the daemon's reason on a
// failure and Reason explains an intentional skip (root excluded, session lost).
type broadcastTarget struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// validateSendPromptFlags rejects nonsensical send-prompt flag combinations
// with an actionable message (public CLI standard, #658/#734). It runs from both
// Args — so it fires before cobra's arity check, which would otherwise mask a
// broadcast flag used without --all behind a generic "accepts 2 arg(s)" error —
// and RunE, so unit tests that drive RunE directly still get the same guard.
func validateSendPromptFlags() error {
	if !sendPromptAllFlag {
		// The broadcast-only flags are meaningless without --all. Name whichever
		// were passed and point the user at the flag that unlocks them.
		var needsAll []string
		if sendPromptAllReposFlag {
			needsAll = append(needsAll, "--all-repos")
		}
		if sendPromptIncludeRootFlag {
			needsAll = append(needsAll, "--include-root")
		}
		if len(needsAll) > 0 {
			return fmt.Errorf("%s can only be used with --all (broadcast mode); add --all to broadcast the prompt to every session in scope", strings.Join(needsAll, " and "))
		}
		return nil
	}
	if sendPromptCreateFlag {
		return errors.New("--all cannot be combined with --create: broadcast only delivers to existing sessions")
	}
	if sendPromptAllReposFlag && repoFlag != "" {
		return errors.New("--all-repos and --repo are mutually exclusive: --all-repos already spans every repo")
	}
	return nil
}

// runBroadcast implements `af sessions send-prompt --all`: deliver one prompt to
// every live session in scope via the same daemon SendPrompt RPC the single-
// target path uses. Scope defaults to the current repo (honoring --repo) so a
// broadcast can never blast another repo's sessions (#761 data-loss class);
// --all-repos opts into spanning every repo. The reserved root session is
// excluded unless --include-root. Delivery is best-effort: a Lost/unreachable
// target is reported and skipped, a per-session send error is recorded, and
// neither aborts the rest. The command exits 0 with a JSON summary regardless of
// individual failures so callers inspect the per-session results.
func runBroadcast(prompt string) error {
	var (
		targets    []scopedInstance
		scopeLabel string
	)
	if sendPromptAllReposFlag {
		all, corrupted, err := allScopedInstances()
		if err != nil {
			return jsonError(err)
		}
		// Fail loudly on a corrupted repo rather than silently broadcasting to
		// a truncated set — the same loud-fail contract as `sessions list`
		// (#730). A hidden session that never receives the prompt is worse than
		// an error the user can act on.
		if len(corrupted) > 0 {
			return jsonError(corruptedReposError(corrupted))
		}
		targets = all
		scopeLabel = "all-repos"
	} else {
		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}
		if repoID == "" {
			// Refuse to guess the scope: silently broadcasting to every repo
			// here is exactly the #761 wrong-repo hazard the --repo scoping
			// exists to prevent.
			return jsonError(errors.New("broadcast needs a target repo: run inside a git repository, pass --repo <path>, or use --all-repos to broadcast to every repo"))
		}
		scoped, err := scopedInstancesForRepo(repoID)
		if err != nil {
			return jsonError(err)
		}
		targets = scoped
		scopeLabel = "repo:" + repoID
	}

	// Deterministic order (repo, then title) so output is stable across runs
	// and the all-repos map iteration order does not leak into the summary.
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].RepoID != targets[j].RepoID {
			return targets[i].RepoID < targets[j].RepoID
		}
		return targets[i].Title < targets[j].Title
	})

	result := broadcastResult{Prompt: prompt, Scope: scopeLabel, Results: []broadcastTarget{}}
	for _, t := range targets {
		// The reserved root session belongs to the maintainer agent (#1106):
		// don't broadcast into it unless explicitly asked.
		if session.IsReservedTitle(t.Title) && !sendPromptIncludeRootFlag {
			result.Skipped++
			result.Results = append(result.Results, broadcastTarget{
				Title:  t.Title,
				RepoID: t.RepoID,
				Status: "skipped",
				Reason: "reserved root session excluded; pass --include-root to broadcast to it",
			})
			continue
		}
		// Lost/Dead sessions have no live backing session to deliver into
		// (#1108). Report them as skipped-unreachable instead of attempting a
		// send that would only fail — the broadcast tolerates them cleanly.
		if t.Status == session.Lost || t.Status == session.Dead {
			result.Skipped++
			result.Results = append(result.Results, broadcastTarget{
				Title:  t.Title,
				RepoID: t.RepoID,
				Status: "skipped",
				Reason: "session is lost/unreachable; recover it before broadcasting",
			})
			continue
		}
		// Archived sessions are deliberately inert (#1028): there is no running
		// backend to receive a prompt until the user restores them.
		if t.Status == session.Archived {
			result.Skipped++
			result.Results = append(result.Results, broadcastTarget{
				Title:  t.Title,
				RepoID: t.RepoID,
				Status: "skipped",
				Reason: "session is archived; restore it before broadcasting",
			})
			continue
		}
		if err := sendPromptViaDaemon(daemon.SendPromptRequest{Title: t.Title, RepoID: t.RepoID, Prompt: prompt}); err != nil {
			result.Failed++
			result.Results = append(result.Results, broadcastTarget{
				Title:  t.Title,
				RepoID: t.RepoID,
				Status: "failed",
				Error:  err.Error(),
			})
			continue
		}
		result.Delivered++
		result.Results = append(result.Results, broadcastTarget{
			Title:  t.Title,
			RepoID: t.RepoID,
			Status: "delivered",
		})
	}
	return jsonOut(result)
}
