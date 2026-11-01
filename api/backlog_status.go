package api

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/daemon"
)

// statusCheck refuses a status change from the item's shown state before the
// request is sent; nil sends it as is.
type statusCheck func(b *backlog.Backlog, id int, st backlog.State) error

// reopenCheck lets reopen act on done and dropped items, and on a parent with at
// least one done or dropped leaf under it: the service reopens exactly those
// leaves and leaves the open ones alone, so a parent whose shown state is
// queued or working (one child done, another still open) is a valid target.
func reopenCheck(b *backlog.Backlog, id int, st backlog.State) error {
	if b.IsParent(id) {
		for _, it := range b.Items {
			if b.IsAncestor(id, it.ID) && !b.IsParent(it.ID) && !it.Status.Open() {
				return nil
			}
		}
		return fmt.Errorf("%s has no done or dropped children to reopen", backlog.FormatID(id))
	}
	if st != backlog.StateDone && st != backlog.StateDropped {
		return fmt.Errorf("%s is %s; only a done or dropped item can be reopened", backlog.FormatID(id), strings.ToLower(st.Label()))
	}
	return nil
}

// draftCheck sends done and dropped items to reopen and refuses a parent: the
// daemon reads a parent going to draft as "reopen its done and dropped
// children", which would reopen finished work while leaving the queued children
// eligible for the runner.
func draftCheck(b *backlog.Backlog, id int, st backlog.State) error {
	if b.IsParent(id) {
		return fmt.Errorf("%s is a parent; draft its children one by one (\"af backlog reopen %d\" reopens its done and dropped children)", backlog.FormatID(id), id)
	}
	if st == backlog.StateDone || st == backlog.StateDropped {
		return fmt.Errorf("%s is %s; reopen it with \"af backlog reopen %d\"", backlog.FormatID(id), strings.ToLower(st.Label()), id)
	}
	return nil
}

func backlogStatusCmd(use, short, long string, to backlog.Status, check statusCheck) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <id>",
		Short: short,
		Long:  long,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBacklog(func() (any, error) {
				if err := refuseFromItemSession(use); err != nil {
					return nil, err
				}
				id, err := backlog.ParseID(args[0])
				if err != nil {
					return nil, err
				}
				t, err := backlogProject()
				if err != nil {
					return nil, err
				}
				if check != nil {
					v, err := t.get()
					if err != nil {
						return nil, err
					}
					if v.Backlog.Item(id) == nil {
						return nil, &backlog.NotFoundError{ID: id, Project: t.name}
					}
					if err := check(v.Backlog, id, v.States[id].State); err != nil {
						return nil, err
					}
				}
				var resp daemon.SetBacklogItemStatusResponse
				if err := callBacklogRoute("SetBacklogItemStatus", daemon.SetBacklogItemStatusRequest{
					RepoID: t.repoID, RepoPath: t.repoPath, ID: id, Status: to,
				}, &resp); err != nil {
					return nil, err
				}
				return resp.Item, nil
			})
		},
	}
}

var (
	backlogQueueCmd = backlogStatusCmd("queue", "Make an item eligible for the runner",
		"Set the item's status to queued. On a parent, queue its draft children.", backlog.StatusQueued, nil)
	backlogDraftCmd = backlogStatusCmd("draft", "Put a queued or in-review item back to draft",
		"Set the item's status to draft, so the runner does not start it. For a done or dropped "+
			"item, use reopen.", backlog.StatusDraft, draftCheck)
	backlogReviewCmd = backlogStatusCmd("review", "Send a running item to review",
		"Set a running item's status to review, for you to check and mark done.", backlog.StatusReview, nil)
	backlogReopenCmd = backlogStatusCmd("reopen", "Reopen a done or dropped item as a draft",
		"Set a done or dropped item's status back to draft. On a parent, reopen its done and "+
			"dropped children.", backlog.StatusDraft, reopenCheck)
	backlogDropCmd = backlogStatusCmd("drop", "Mark an item dropped",
		"Mark the item dropped. On a parent, drop its open children. Its session stays.", backlog.StatusDropped, nil)
)

var backlogDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete a draft that has no runs and no thread",
	Long:  "Delete a draft item that has no runs, no thread, and no children. To remove any other item, drop it.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			if err := refuseFromItemSession("delete"); err != nil {
				return nil, err
			}
			id, err := backlog.ParseID(args[0])
			if err != nil {
				return nil, err
			}
			t, err := backlogProject()
			if err != nil {
				return nil, err
			}
			var resp daemon.DeleteBacklogItemResponse
			if err := callBacklogRoute("DeleteBacklogItem", daemon.DeleteBacklogItemRequest{
				RepoID: t.repoID, RepoPath: t.repoPath, ID: id,
			}, &resp); err != nil {
				return nil, err
			}
			return map[string]any{"id": id, "deleted": true}, nil
		})
	},
}

var (
	backlogRunnerMaxRunning int
	backlogRunnerOnDone     string
)

// backlogRunnerView is what af backlog runner prints.
type backlogRunnerView struct {
	backlog.RunnerView
	Runner     bool           `json:"runner"`
	MaxRunning int            `json:"max_running"`
	OnDone     string         `json:"on_done"`
	Budget     backlog.Budget `json:"budget"`
}

var backlogRunnerCmd = &cobra.Command{
	Use:   "runner [on | off] [--max-running N] [--on-done keep|archive]",
	Short: "Show or change this project's runner",
	Long: "With no arguments, print the runner's state and reason, its slots, and the budget. " +
		"on and off switch the runner for this project; it is off by default. --max-running " +
		"sets how many items run at once (1-8). --on-done sets what happens to an item's " +
		"session when it is done: keep it or archive it.",
	Args:      cobra.MaximumNArgs(1),
	ValidArgs: []string{"on", "off"},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			var (
				runner, changed bool
				maxRunning      *int
				onDone          *string
				runnerSet       *bool
			)
			if len(args) == 1 {
				switch args[0] {
				case "on":
					runner = true
				case "off":
				default:
					return nil, fmt.Errorf("invalid runner switch %q: use on or off", args[0])
				}
				runnerSet, changed = &runner, true
			}
			if cmd.Flags().Changed("max-running") {
				maxRunning, changed = &backlogRunnerMaxRunning, true
			}
			if cmd.Flags().Changed("on-done") {
				onDone, changed = &backlogRunnerOnDone, true
			}
			if changed {
				if err := refuseControlsFromItemSession(); err != nil {
					return nil, err
				}
			}
			t, err := backlogProject()
			if err != nil {
				return nil, err
			}
			if changed {
				var resp daemon.SetBacklogSettingsResponse
				if err := callBacklogRoute("SetBacklogSettings", daemon.SetBacklogSettingsRequest{
					RepoID: t.repoID, RepoPath: t.repoPath, Runner: runnerSet, MaxRunning: maxRunning, OnDone: onDone,
				}, &resp); err != nil {
					return nil, err
				}
			}
			v, err := t.get()
			if err != nil {
				return nil, err
			}
			var budget daemon.GetBudgetResponse
			if err := callBacklogRoute("GetBudget", daemon.GetBudgetRequest{}, &budget); err != nil {
				return nil, err
			}
			s := v.Backlog.Settings
			return backlogRunnerView{RunnerView: v.Runner, Runner: s.Runner, MaxRunning: s.MaxRunning,
				OnDone: s.OnDone, Budget: budget.Budget}, nil
		})
	},
}

var (
	backlogBudgetLimit     string
	backlogBudgetMaxPerRun string
	backlogBudgetUSDLimit  string
	// backlogBudgetRemoved names the two-gate budget's flags, which v1.0.341
	// shipped. They stay as hidden flags so that giving one is refused with what
	// replaced it, not with "unknown flag".
	backlogBudgetRemoved = []string{"share", "stop-at"}
)

// errBudgetFlagRemoved answers --share and --stop-at. Neither maps onto
// --limit: --stop-at 90 with the default share held the backlog to half the
// plan, and --limit 90 would let it run to 90%.
func errBudgetFlagRemoved(name string) error {
	return fmt.Errorf("--%s was removed: the runner now starts items only while your total plan use, "+
		"from any source, is under one limit (default 50%%) · set it with \"af backlog budget --limit <pct>\"", name)
}

// backlogNow is the clock for reading ages. A var so tests can pin it.
var backlogNow = time.Now

// backlogBudgetView is what af backlog budget prints.
type backlogBudgetView struct {
	daemon.GetBudgetResponse
	// ReadingAge is how old the newest plan reading is.
	ReadingAge string `json:"reading_age,omitempty"`
}

var backlogBudgetCmd = &cobra.Command{
	Use:   "budget [--limit <pct>] [--max-per-run <pct>] [--usd-limit <usd>]",
	Short: "Show or change the global budget",
	Long: "The budget keeps backlog work out of the way of your own: the runner starts items only " +
		"while your total Claude plan use, from any source, is under the limit in both plan windows " +
		"(5-hour and weekly), for all projects. With no flags, print it with each window's plan %, " +
		"the backlog's part of it, the reset time and the age of the newest reading. --limit sets " +
		"the plan % under which the runner starts items (default 50). Item sessions that are " +
		"already running keep going when plan use passes the limit; only new starts wait. " +
		"--max-per-run sets the % of the 5-hour window one run may use (default 10). --usd-limit " +
		"sets a dollar limit at API prices for accounts with no plan readings; 0 turns it off.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBacklog(func() (any, error) {
			for _, name := range backlogBudgetRemoved {
				if cmd.Flags().Changed(name) {
					return nil, errBudgetFlagRemoved(name)
				}
			}
			limit, err := budgetFlag(cmd, "limit", backlogBudgetLimit, false)
			if err != nil {
				return nil, err
			}
			maxPerRun, err := budgetFlag(cmd, "max-per-run", backlogBudgetMaxPerRun, false)
			if err != nil {
				return nil, err
			}
			usdLimit, err := budgetFlag(cmd, "usd-limit", backlogBudgetUSDLimit, true)
			if err != nil {
				return nil, err
			}
			var req daemon.SetBudgetRequest
			req.Limit = limit
			req.MaxPerRun = maxPerRun
			req.USDLimit = usdLimit
			change := limit != nil || maxPerRun != nil || usdLimit != nil
			var resp daemon.GetBudgetResponse
			if change {
				if err := refuseControlsFromItemSession(); err != nil {
					return nil, err
				}
				if err := callBacklogRoute("SetBudget", req, &resp); err != nil {
					return nil, err
				}
			} else if err := callBacklogRoute("GetBudget", daemon.GetBudgetRequest{}, &resp); err != nil {
				return nil, err
			}
			view := backlogBudgetView{GetBudgetResponse: resp}
			if resp.ReadingAt != nil {
				view.ReadingAge = backlogNow().Sub(*resp.ReadingAt).Round(time.Second).String()
			}
			return view, nil
		})
	},
}

// budgetFlag parses one budget flag; nil when it was not given.
func budgetFlag(cmd *cobra.Command, name, val string, usd bool) (*float64, error) {
	if !cmd.Flags().Changed(name) {
		return nil, nil
	}
	parse := func(s string) (float64, error) { return parsePercent(name, s) }
	if usd {
		parse = parseUSD
	}
	v, err := parse(val)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func parsePercent(flag, s string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	if err != nil || math.IsNaN(v) || v < 0 || v > 100 {
		return 0, fmt.Errorf("--%s must be between 0 and 100", flag)
	}
	return v, nil
}

func parseUSD(s string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(s), "$"), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, fmt.Errorf("--usd-limit must be a finite amount of US dollars, 0 or more — for example 5 or 2.50, not %q", s)
	}
	return v, nil
}

func init() {
	backlogRunnerCmd.Flags().IntVar(&backlogRunnerMaxRunning, "max-running", backlog.DefaultMaxRunning, "How many items may run at once (1-8)")
	backlogRunnerCmd.Flags().StringVar(&backlogRunnerOnDone, "on-done", backlog.OnDoneKeep, "What happens to an item's session when it is done: keep or archive")
	f := backlogBudgetCmd.Flags()
	f.StringVar(&backlogBudgetLimit, "limit", "", "Plan percent, from any use, under which the runner starts items in both windows (0-100; default 50)")
	for _, name := range backlogBudgetRemoved {
		f.String(name, "", "Removed; use --limit")
		if err := f.MarkHidden(name); err != nil {
			panic(err)
		}
	}
	f.StringVar(&backlogBudgetMaxPerRun, "max-per-run", "", "Percent of the 5-hour window one run may use (0-100; default 10)")
	f.StringVar(&backlogBudgetUSDLimit, "usd-limit", "", "Dollar limit at API prices for accounts with no plan readings; 0 turns it off")

	BacklogCmd.AddCommand(backlogQueueCmd, backlogDraftCmd, backlogReviewCmd, backlogReopenCmd,
		backlogDropCmd, backlogDeleteCmd, backlogRunnerCmd, backlogBudgetCmd)
}
