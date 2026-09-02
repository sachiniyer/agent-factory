package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session"
)

// Orphaned pinned host-key directories (#3560).
//
// A provision_cmd session pins exactly one host key under
// <af home>/hook-hosts/<slug> and owns that directory. #3454 made both teardown
// paths — live and tombstone-restored — drop it on the same success-only
// condition, so new sessions no longer orphan one. Nothing collects the
// directories already orphaned in the field, and #3454 named three sources that
// still produce them: a tombstone written before that fix, a daemon rolled back
// to a release without the ownership flag (#3122), and a delete_cmd that ANSWERS
// with an error — where the reap latches as known-state and the record may be
// deleted while the pin, correctly, survives.
//
// So this is a collector, and it is the most dangerous shape doctor has: an
// irreversible RemoveAll keyed on "nobody owns this". Deleting a LIVE session's
// pin is far worse than the leak it collects. The pinned known_hosts file is
// embedded in that session's sandbox ssh command, so removing it makes every
// later reap fail host-key verification before it can reach the machine — a
// tombstone that can never complete, and a billable remote machine that can
// never be destroyed. The leak is a few hundred bytes.
//
// Hence the stance, which is doctor's own, applied at its strictest:
//
//   - A FAILED READ IS NOT AN EMPTY RESULT. The owner set is either COMPLETE or
//     it is an error — hookHostOwnerSlugs never returns a partial map with a nil
//     error. Every incompleteness (an unreachable daemon, a daemon that answers
//     with an error, an unreadable instances.json, an unlistable project
//     directory, records that will not parse) becomes one advisory UNKNOWN
//     finding and NO fix closure, so --fix removes nothing. This is the #2874
//     defect — doctor read an unreadable tmux listing as an empty one and armed
//     --fix to kill every live session's processes — and it must not be rebuilt
//     here.
//   - SLUGS ARE ONE GLOBAL NAMESPACE. Hook session names are handed to the hook
//     scripts verbatim, so hook-hosts/ is shared by every project on the box.
//     The other remote checks scope themselves to the repository containing the
//     cwd; this one must not, or another project's live pin reads as an orphan.
//   - THE DAEMON'S ANSWER IS PREFERRED, AND THE DISK IS STILL READ. The pin is
//     written during provisioning, before the record is necessarily
//     checkpointed, so a just-created session exists only in the daemon (whose
//     Snapshot includes pendingCreates). The converse is also true: a persisted
//     row that failed to materialize is skipped by the daemon and is invisible
//     to its snapshot while the agent it describes may still be running. Neither
//     source is a superset of the other, so the owner set is their UNION and
//     both must be readable.
//   - OWNERSHIP OUTLIVES LIVENESS. A live, archived, mid-kill or kill-tombstoned
//     session all own their slug; ownership ends only when nothing can ever need
//     the pin again. That is why ownership is derived from the TITLE of every
//     record, of every kind, rather than from a liveness or a backend type: an
//     archived row, a tombstone awaiting its reap and a row that failed to
//     materialize all carry a title, and every one of them may still need the
//     pin. A record whose backend cannot be read still spares its directory.

// hookHostsCheck is the finding slug for both the orphan and the UNKNOWN.
//
// The two names this check depends on — the store's directory and the one file a
// pin holds — are read from the session package that writes them
// (session.HookHostsRoot, session.HookHostsPinFileName) rather than restated
// here. A private copy would be a second spelling of a path two packages must
// agree on, which is the drift #3454 was.
const hookHostsCheck = "orphaned-hook-host"

// checkOrphanedHookHosts reports pinned host-key directories no session owns,
// and (under --fix) removes them.
//
// It is silent on the common case: a box that never used provision_cmd has no
// hook-hosts directory, so there is nothing to say and nothing to enumerate. The
// owner enumeration below is deliberately reached only when a candidate exists —
// it dials the daemon and walks every project's records, and a local-only user
// must pay neither the cost nor the noise.
func checkOrphanedHookHosts(ctx *scanContext, report *Report) {
	root := filepath.Join(ctx.opts.ConfigDir, session.HookHostsRoot)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		// The directory exists but cannot be listed. That is UNKNOWN, not empty:
		// reporting it is safe, and there is nothing to remove because there is
		// nothing we could name.
		report.addAdvisoryFinding(Finding{
			Check:       hookHostsCheck,
			Section:     sectionRemote,
			Severity:    StatusWarn,
			Detail:      fmt.Sprintf("the pinned host-key store %s could not be listed (%v), so doctor cannot tell whether it holds orphaned directories", root, err),
			Remediation: "repair the directory's permissions, then rerun `af doctor`",
		})
		return
	}

	var candidates, unrecognized []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			unrecognized = append(unrecognized, name)
			continue
		}
		if err := hookHostPinShape(filepath.Join(root, name)); err != nil {
			unrecognized = append(unrecognized, name)
			continue
		}
		candidates = append(candidates, name)
	}

	// Anything that is not one of af's pins is reported and never touched. af
	// writes exactly one file per directory, so this is empty in practice — but
	// --fix must not remove a directory whose shape it did not verify, and once
	// the gate exists, silently dropping what it rejects would hide from the
	// operator the one thing they need to see.
	if len(unrecognized) > 0 {
		shape := "is not one of af's per-session host-key pins"
		if len(unrecognized) > 1 {
			shape = "are not af's per-session host-key pins"
		}
		report.addAdvisoryFinding(Finding{
			Check:    hookHostsCheck,
			Section:  sectionRemote,
			Severity: StatusWarn,
			Detail: fmt.Sprintf("%s under %s %s (%s), so doctor leaves %s alone",
				plural(len(unrecognized), "entry", "entries"), root, shape, strings.Join(unrecognized, ", "), itThem(len(unrecognized))),
			Remediation: "inspect the entries and remove them yourself if they are yours to remove",
		})
	}
	if len(candidates) == 0 {
		return
	}

	owners, err := hookHostOwnerSlugs(ctx)
	if err != nil {
		// The one branch that decides everything. An incomplete inventory means
		// every candidate is UNKNOWN, and an UNKNOWN carries no fix closure, so
		// --fix removes nothing.
		report.addAdvisoryFinding(Finding{
			Check:    hookHostsCheck,
			Section:  sectionRemote,
			Severity: StatusWarn,
			Detail: fmt.Sprintf("%s under %s cannot be checked for an owner because the session inventory is incomplete: %v — nothing was removed",
				plural(len(candidates), "pinned host-key directory", "pinned host-key directories"), root, err),
			Remediation: "resolve the reported cause so every session can be enumerated, then rerun `af doctor`",
		})
		return
	}

	for _, slug := range candidates {
		dir := filepath.Join(root, slug)
		if title, owned := owners[slug]; owned {
			report.Pass(sectionRemote, "hook host key", fmt.Sprintf("%s is owned by session %q", dir, title))
			continue
		}
		report.addActionableFinding(Finding{
			Check:       hookHostsCheck,
			Section:     sectionRemote,
			Severity:    StatusWarn,
			Detail:      fmt.Sprintf("%s pins a host key for a session that no longer exists in any project, so nothing will ever need it again", dir),
			FixAction:   "remove " + dir,
			fix:         hookHostRemoveFix(ctx, root, slug),
			Remediation: "run `af doctor --fix` to remove it, or `" + shellsuggest.Command("rm", "-rf", dir) + "`",
		})
	}
}

// hookHostPinShape reports whether dir is one of af's pins: a directory holding
// exactly one regular file named known_hosts, which is what
// hookProvisionKnownHosts writes and all it writes.
//
// A positive identity test, not a negative one. --fix must remove only a
// directory it has PROVEN af created; an unreadable or unexpected shape means
// "not proven", which spares it.
func hookHostPinShape(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("its contents could not be listed: %w", err)
	}
	if len(entries) != 1 || entries[0].Name() != session.HookHostsPinFileName || !entries[0].Type().IsRegular() {
		return fmt.Errorf("it does not hold exactly one regular %s file, so af did not write it", session.HookHostsPinFileName)
	}
	return nil
}

// hookHostRemoveFix removes one orphaned pin, re-establishing every precondition
// at fix time.
//
// Detection ran before the rest of the sweep, so a session may have been created
// since — and a create that recycles a killed session's title lands on exactly
// the slug we are about to delete. So the owner set is read again here, and a
// re-read that FAILS refuses the removal: a guard that could not run has not
// passed. The shape gate is re-run for the same reason.
//
// Once per REMOVAL, deliberately, rather than once per run. A single memoized
// re-read looked like the cheap version of this, and it is wrong for a batch: the
// removals run in sequence, so a session created after the first RemoveAll but
// before the fifth would be invisible to the fifth, and its pin would go. That is
// the same defect as trusting detection, one step later, and it grows with the
// number of orphans. An enumeration is one in-memory RPC plus one walk of the
// records, and orphans are counted in ones — the wrong thing to economise on when
// the cost of being wrong is a stranded machine.
func hookHostRemoveFix(ctx *scanContext, root, slug string) func() error {
	return func() error {
		dir := filepath.Join(root, slug)
		if !pathInside(root, dir) {
			return fmt.Errorf("refusing to remove %s: it is not inside the pinned host-key store %s", dir, root)
		}
		if err := hookHostPinShape(dir); err != nil {
			return fmt.Errorf("refusing to remove %s: %w", dir, err)
		}
		owners, err := hookHostOwnerSlugs(ctx)
		if err != nil {
			return fmt.Errorf("refusing to remove %s: the session inventory is incomplete: %w", dir, err)
		}
		if title, owned := owners[slug]; owned {
			return fmt.Errorf("refusing to remove %s: session %q now owns it", dir, title)
		}
		return os.RemoveAll(dir)
	}
}

// hookHostOwnerSlugs returns every owned slug mapped to an owning session's
// title, or an error saying why the inventory is INCOMPLETE.
//
// The contract is the whole check: a non-nil map means "this is every session on
// this box", and a partial answer is never returned with a nil error. Callers
// may treat a slug's absence as proof of no owner ONLY because of that.
func hookHostOwnerSlugs(ctx *scanContext) (map[string]string, error) {
	if err := hookHostHomeAgrees(ctx); err != nil {
		return nil, err
	}
	owners := map[string]string{}
	// The daemon first: it holds sessions that are not on disk yet (a create is
	// registered in pendingCreates and joins the instance map only once
	// provisioning completes, and the pin is written inside that window).
	live, err := ctx.opts.sessionInventory()
	if err != nil {
		return nil, fmt.Errorf("the running daemon's session list could not be read: %w", err)
	}
	for _, data := range live {
		addHookHostOwner(owners, data.Title)
	}
	// Then the disk, which holds sessions the daemon does not: a persisted row
	// that failed to materialize is skipped by the daemon and invisible to its
	// snapshot, and a kill tombstone awaiting its reap outlives the daemon that
	// wrote it. Every project, never just the cwd's.
	stored, err := hookHostStoredTitles()
	if err != nil {
		return nil, err
	}
	for _, title := range stored {
		addHookHostOwner(owners, title)
	}
	return owners, nil
}

// addHookHostOwner records the slug a session's title claims. The first title to
// claim a slug keeps the report line; two titles can slugify alike (the create
// path refuses that within a project, not across the box), and which one is
// named matters only to the message.
//
// Slugify never answers with an empty string — a title that survives no
// character of it falls back to "session", which is also the directory such a
// session would have pinned. So a record whose title is unreadable still claims
// a slug rather than none, which is the direction that spares a directory.
func addHookHostOwner(owners map[string]string, title string) {
	slug := session.Slugify(title)
	if _, seen := owners[slug]; !seen {
		owners[slug] = title
	}
}

// hookHostHomeAgrees refuses the enumeration unless the home doctor is
// INSPECTING is the home the inventory would describe.
//
// Options.ConfigDir is injectable, while the daemon socket and the session
// records both resolve from AGENT_FACTORY_HOME. If those ever disagree, the
// owner set would describe one box's sessions while the pins came from another's
// — the single worst way to get this wrong, because the answer looks complete.
func hookHostHomeAgrees(ctx *scanContext) error {
	home, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("the agent-factory home could not be resolved: %w", err)
	}
	if filepath.Clean(home) != filepath.Clean(ctx.opts.ConfigDir) {
		return fmt.Errorf("the session inventory would describe %s, not the home under inspection (%s)", home, ctx.opts.ConfigDir)
	}
	return nil
}

// hookHostStoredTitles returns the title of every session record on disk, across
// every project, or an error if even one project's records could not be read.
//
// Strict by construction: LoadAllRepoInstancesReportingSkipDetails DROPS a repo
// whose file it could not read, and its own doc comment says a caller reasoning
// from what is MISSING must take the skip list instead (#2874). This is exactly
// such a caller — a slug's absence authorises an rm -rf — so one skip fails the
// whole enumeration.
func hookHostStoredTitles() ([]string, error) {
	all, skipped, err := config.LoadAllRepoInstancesReportingSkipDetails()
	if err != nil {
		return nil, fmt.Errorf("the projects holding session records could not be enumerated: %w", err)
	}
	if len(skipped) > 0 {
		return nil, fmt.Errorf("%s (%s)", config.DescribeRepoInstancesSkips(skipped), config.RepoInstancesSkipRemedy(skipped))
	}
	var titles []string
	for _, repoID := range sortedKeys(all) {
		found, err := hookHostRecordTitles(all[repoID])
		if err != nil {
			path, pathErr := config.RepoInstancesPath(repoID)
			if pathErr != nil {
				path = "project " + repoID
			}
			return nil, fmt.Errorf("the session records in %s could not be parsed: %w", path, err)
		}
		titles = append(titles, found...)
	}
	return titles, nil
}

// hookHostRecordTitles decodes one project's records down to their titles.
//
// It accepts both stored shapes because the all-projects loader hands back the
// file's RAW bytes whenever it could not unwrap the envelope itself, and refuses
// anything else: a file that will not decode is a failed read, and a failed read
// is not an empty project.
func hookHostRecordTitles(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	type record struct {
		Title string `json:"title"`
	}
	var rows []record
	arrayErr := json.Unmarshal(trimmed, &rows)
	if arrayErr != nil {
		var envelope struct {
			Instances json.RawMessage `json:"instances"`
		}
		if err := json.Unmarshal(trimmed, &envelope); err != nil || len(bytes.TrimSpace(envelope.Instances)) == 0 {
			return nil, arrayErr
		}
		if err := json.Unmarshal(envelope.Instances, &rows); err != nil {
			return nil, err
		}
	}
	titles := make([]string, 0, len(rows))
	for _, row := range rows {
		titles = append(titles, row.Title)
	}
	return titles, nil
}

// daemonSessionInventory reads the running daemon's global session list — every
// project, since RepoID is empty.
//
// apiclient.New, not NewTargeted: the pins under inspection are THIS machine's
// af home, and a CLI aimed at a remote daemon would answer with the remote box's
// sessions. That answer looks complete and is about the wrong machine, so every
// local pin would read as an orphan. The local socket is the only inventory that
// can speak for a local directory.
func daemonSessionInventory() ([]session.InstanceData, error) {
	client, err := apiclient.New()
	if err != nil {
		return nil, err
	}
	// BOUNDED, because the local socket is not. apiclient leaves the local
	// request timeout at zero on purpose, so a daemon that accepts the connection
	// and never answers would hang `af doctor` outright — the 250ms dial timeout
	// is long satisfied by then, and a read-only diagnostic that never returns is
	// worse than one that says it could not tell. The deadline lands in the same
	// place every other failure does: the inventory is incomplete, so nothing is
	// removed.
	ctx, cancel := context.WithTimeout(context.Background(), hookHostInventoryTimeout)
	defer cancel()
	return client.SnapshotCtx(ctx, daemon.SnapshotRequest{})
}

// hookHostInventoryTimeout bounds the daemon read above. Generous: the answer is
// an in-memory snapshot that returns at once on any healthy daemon, so this is a
// wedged-daemon ceiling rather than a latency budget. A var so a test can drive
// the deadline without waiting on it.
var hookHostInventoryTimeout = 10 * time.Second

// itThem keeps the plural-sensitive clause above readable.
func itThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
