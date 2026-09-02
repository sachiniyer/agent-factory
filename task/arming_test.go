package task

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func armingFixture(id, cron string) Task {
	return Task{ID: id, Name: "nightly", CronExpr: cron, ProjectPath: "/repo", Enabled: true}
}

func TestApplyLiveArmingCarriesTheObservation(t *testing.T) {
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	observed := armingFixture("a", "0 9 * * *")
	observed.Arming = ArmingArmed
	observed.NextRunAt = &next

	got := ApplyLiveArming([]Task{armingFixture("a", "0 9 * * *")}, []Task{observed})

	assert.Equal(t, ArmingArmed, got[0].Arming)
	require.NotNil(t, got[0].NextRunAt, "an armed observation carries the entry's own next fire")
	assert.Equal(t, next, *got[0].NextRunAt)
}

func TestApplyLiveArmingLeavesUnobservedTasksUnknown(t *testing.T) {
	// The whole point of the tri-state: a task the daemon said nothing about must
	// not come back as not-armed. Reported as such it would accuse every task on
	// the box the moment a daemon was slow to answer.
	got := ApplyLiveArming([]Task{armingFixture("a", "0 9 * * *")}, []Task{armingFixture("b", "0 9 * * *")})
	assert.Equal(t, ArmingUnknown, got[0].Arming)
	assert.Nil(t, got[0].NextRunAt)
}

func TestApplyLiveArmingRefusesAnObservationAboutADifferentTrigger(t *testing.T) {
	// The two lists are two separate reads of tasks.json, so they can straddle an
	// edit. An observation adopted across one reports a fire time for a schedule
	// that is no longer the schedule — which is worse than reporting nothing.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	observed := armingFixture("a", "0 9 * * *")
	observed.Arming = ArmingArmed
	observed.NextRunAt = &next

	edited := armingFixture("a", "0 21 * * *") // the user just moved it to 9pm
	got := ApplyLiveArming([]Task{edited}, []Task{observed})

	assert.Equal(t, ArmingUnknown, got[0].Arming, "the armed entry is for the OLD expression")
	assert.Nil(t, got[0].NextRunAt, "and so is the fire time it computed")
}

func TestApplyLiveArmingRefusesAnObservationAboutADisabledTwin(t *testing.T) {
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	observed := armingFixture("a", "0 9 * * *")
	observed.Arming = ArmingArmed
	observed.NextRunAt = &next

	disabled := armingFixture("a", "0 9 * * *")
	disabled.Enabled = false
	got := ApplyLiveArming([]Task{disabled}, []Task{observed})

	assert.Equal(t, ArmingUnknown, got[0].Arming, "a task switched off is not armed, however recently it was")
	assert.Nil(t, got[0].NextRunAt)
}

func TestApplyLiveArmingComparesTheWatcherSignatureForWatchTasks(t *testing.T) {
	// A watcher RUNS a command in a directory under a name; a write commits and
	// reloads the supervisor as a separate step, so the old process can outlive
	// the edit. Each of watcherSignature's three fields has to invalidate.
	base := Task{ID: "w", Name: "build", WatchCmd: "tail -f log", ProjectPath: "/repo", Enabled: true}
	observed := base
	observed.Arming = ArmingArmed

	for name, edit := range map[string]func(Task) Task{
		"watch_cmd":    func(t Task) Task { t.WatchCmd = "tail -f other"; return t },
		"project_path": func(t Task) Task { t.ProjectPath = "/elsewhere"; return t },
		"name":         func(t Task) Task { t.Name = "renamed"; return t },
	} {
		t.Run(name, func(t *testing.T) {
			got := ApplyLiveArming([]Task{edit(base)}, []Task{observed})
			assert.Equal(t, ArmingUnknown, got[0].Arming)
		})
	}

	t.Run("unchanged", func(t *testing.T) {
		got := ApplyLiveArming([]Task{base}, []Task{observed})
		assert.Equal(t, ArmingArmed, got[0].Arming)
	})
}

func TestApplyLiveArmingRefusesAcrossTriggerKinds(t *testing.T) {
	// A hand-edited store can turn a cron task into a watch task under the same
	// id. The two subsystems arm different things; neither observation describes
	// the other.
	observed := armingFixture("a", "0 9 * * *")
	observed.Arming = ArmingArmed
	watch := Task{ID: "a", Name: "nightly", WatchCmd: "tail -f log", ProjectPath: "/repo", Enabled: true}

	got := ApplyLiveArming([]Task{watch}, []Task{observed})
	assert.Equal(t, ArmingUnknown, got[0].Arming)
}

func TestApplyLiveArmingReportsEachDuplicateRowFromItsOwnObservation(t *testing.T) {
	// Only the FIRST row for an id is ever armed (#855) — and the daemon returns
	// an observation for EVERY row, marking each one after the first not-armed. So
	// the later duplicate has an authoritative negative waiting for it, and the
	// row that will never run is exactly the one worth marking.
	//
	// Both observations describe both rows equally here (same repo, same
	// expression), so what keeps them apart is that an observation describes ONE
	// row: taken, it is spent. Without that the second row would take the first's
	// "armed" and report a task the scheduler skipped as scheduled; with a blanket
	// skip instead it stayed UNKNOWN, which hides the mark behind a computed fire
	// time — the same false clean bill in the other direction.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	first := armingFixture("a", "0 9 * * *")
	first.Arming = ArmingArmed
	first.NextRunAt = &next
	second := armingFixture("a", "0 9 * * *")
	second.Arming = ArmingNotArmed

	got := ApplyLiveArming([]Task{armingFixture("a", "0 9 * * *"), armingFixture("a", "0 9 * * *")}, []Task{first, second})

	assert.Equal(t, ArmingArmed, got[0].Arming, "the first row is the one the daemon armed")
	require.NotNil(t, got[0].NextRunAt)
	assert.Equal(t, ArmingNotArmed, got[1].Arming, "and the second is told it will not fire")
	assert.Nil(t, got[1].NextRunAt, "with no fire time, because nothing is holding it")
}

func TestApplyLiveArmingReportsADuplicateWithItsOwnTrigger(t *testing.T) {
	// The sharpest shape of the same thing: a later duplicate carrying a DIFFERENT
	// expression. It has its own observation, it is unambiguously not the row the
	// daemon armed, and leaving it unknown would render a computed next run off an
	// expression the scheduler is not holding at all.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	armed := armingFixture("a", "0 9 * * *")
	armed.Arming, armed.NextRunAt = ArmingArmed, &next
	skipped := armingFixture("a", "0 21 * * *")
	skipped.Arming = ArmingNotArmed

	got := ApplyLiveArming(
		[]Task{armingFixture("a", "0 9 * * *"), armingFixture("a", "0 21 * * *")},
		[]Task{armed, skipped},
	)

	assert.Equal(t, ArmingArmed, got[0].Arming)
	assert.Equal(t, ArmingNotArmed, got[1].Arming)
	assert.Nil(t, got[1].NextRunAt)
}

func TestApplyLiveArmingLeavesAnUnmatchedDuplicateUnknown(t *testing.T) {
	// One observation, two local rows that both fit it. The first claims it; the
	// second has nothing describing it and must say so rather than borrow.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	only := armingFixture("a", "0 9 * * *")
	only.Arming, only.NextRunAt = ArmingArmed, &next

	got := ApplyLiveArming([]Task{armingFixture("a", "0 9 * * *"), armingFixture("a", "0 9 * * *")}, []Task{only})

	assert.Equal(t, ArmingArmed, got[0].Arming)
	assert.Equal(t, ArmingUnknown, got[1].Arming, "an observation is spent when it is taken")
	assert.Nil(t, got[1].NextRunAt)
}

func TestApplyLiveArmingCarriesNotArmed(t *testing.T) {
	// The negative observation is the one #2929 was about: an enabled task the
	// daemon refused to arm. It has to cross over as readily as the positive.
	observed := armingFixture("a", "0 9 * * *")
	observed.Arming = ArmingNotArmed

	got := ApplyLiveArming([]Task{armingFixture("a", "0 9 * * *")}, []Task{observed})
	assert.Equal(t, ArmingNotArmed, got[0].Arming)
	assert.Nil(t, got[0].NextRunAt)
}

func TestApplyLiveArmingIsANoOpWithNothingObserved(t *testing.T) {
	tasks := []Task{armingFixture("a", "0 9 * * *")}
	assert.Equal(t, tasks, ApplyLiveArming(tasks, nil))
	assert.Nil(t, ApplyLiveArming(nil, []Task{armingFixture("a", "0 9 * * *")}))
}

func TestApplyLiveArmingTakesTheDuplicateObservationThatIsAboutThisRow(t *testing.T) {
	// The daemon skips a duplicated id GLOBALLY, first row wins, and answers about
	// every repo. A repo-scoped reader sees only its own rows, so the row the
	// daemon SKIPPED can be the only one in this list — locally first, where
	// first-wins cannot see the skip.
	//
	// Two ways to get this wrong, and the second is the one that survived a review
	// round. Taking the other repo's ARMED row reports the duplicate that will
	// never run as scheduled. Refusing it but stopping there discards the
	// authoritative NOT-ARMED row for this record, which reads as "unobserved" and
	// suppresses the warning just as thoroughly — the task still renders a
	// computed fire time and no mark.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	globallyFirst := armingFixture("dup", "0 9 * * *")
	globallyFirst.ProjectPath = "/repo-a"
	globallyFirst.Arming, globallyFirst.NextRunAt = ArmingArmed, &next
	skipped := armingFixture("dup", "0 9 * * *")
	skipped.ProjectPath = "/repo-b"
	skipped.Arming = ArmingNotArmed

	// What the repo-b rail loads: its row alone, locally first.
	local := armingFixture("dup", "0 9 * * *")
	local.ProjectPath = "/repo-b"

	got := ApplyLiveArming([]Task{local}, []Task{globallyFirst, skipped})

	assert.Equal(t, ArmingNotArmed, got[0].Arming,
		"the observation ABOUT this row is the one that answers")
	assert.Nil(t, got[0].NextRunAt, "and it carries no fire time, because nothing is holding it")
}

func TestApplyLiveArmingStaysUnknownWhenNoObservationIsAboutThisRow(t *testing.T) {
	// The daemon answered, but only about the other repo's row — a list truncated
	// mid-write, or a store this reader has not seen all of. "Nothing reported on
	// THIS record" is the honest answer; inheriting the neighbour's is not.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	other := armingFixture("dup", "0 9 * * *")
	other.ProjectPath = "/repo-a"
	other.Arming, other.NextRunAt = ArmingArmed, &next

	local := armingFixture("dup", "0 9 * * *")
	local.ProjectPath = "/repo-b"

	got := ApplyLiveArming([]Task{local}, []Task{other})
	assert.Equal(t, ArmingUnknown, got[0].Arming)
	assert.Nil(t, got[0].NextRunAt)
}

func TestApplyLiveArmingRefusesAnObservationBoundToAnotherRepo(t *testing.T) {
	// A path can be deleted and reused, and repoScope.matches treats a RETAINED
	// RepoID as authoritative over the path — that is what lets a task survive its
	// project moving. So two rows can share an id, a trigger AND a ProjectPath and
	// still belong to different repos, and the pane displays whichever one its
	// RepoID selects. Matching on id and path alone would hand the displayed row
	// the OTHER repo's armed entry, complete with a fire time.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	oldRepo := armingFixture("dup", "0 9 * * *")
	oldRepo.RepoID = "repo-a"
	oldRepo.Arming, oldRepo.NextRunAt = ArmingArmed, &next
	newRepo := armingFixture("dup", "0 9 * * *")
	newRepo.RepoID = "repo-b"
	newRepo.Arming = ArmingNotArmed

	local := armingFixture("dup", "0 9 * * *")
	local.RepoID = "repo-b"

	got := ApplyLiveArming([]Task{local}, []Task{oldRepo, newRepo})
	assert.Equal(t, ArmingNotArmed, got[0].Arming,
		"the observation bound to THIS repo is the one that answers")
	assert.Nil(t, got[0].NextRunAt)
}

func TestApplyLiveArmingToleratesAnUnbackfilledRepoID(t *testing.T) {
	// RepoID is daemon-backfilled, so two reads of the same file can straddle a
	// backfill and disagree about a record nobody changed. An empty id on either
	// side means "not bound yet", not "a different project" — demanding equality
	// there would throw away real observations for a field in transition.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	observed := armingFixture("a", "0 9 * * *")
	observed.RepoID = "repo-a"
	observed.Arming, observed.NextRunAt = ArmingArmed, &next

	legacy := armingFixture("a", "0 9 * * *") // read before the backfill landed
	require.Empty(t, legacy.RepoID)

	got := ApplyLiveArming([]Task{legacy}, []Task{observed})
	assert.Equal(t, ArmingArmed, got[0].Arming, "an unbound id is not a different project")
	require.NotNil(t, got[0].NextRunAt)
	assert.Equal(t, next, *got[0].NextRunAt)
}

func TestApplyLiveArmingWillNotGuessBetweenConflictingBindings(t *testing.T) {
	// The backfill straddle, at its worst: the disk read landed before the daemon
	// wrote this row's RepoID and ListTasks landed after, so the local record is
	// unbound while the observations are not. A reused path means an older row
	// retained to ANOTHER repo shares the id, the path and the expression — and a
	// missing id matching anything would take that row's armed answer over this
	// record's own not-armed one, reporting a task the daemon skipped as scheduled.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	otherRepo := armingFixture("dup", "0 9 * * *")
	otherRepo.RepoID = "repo-a"
	otherRepo.Arming, otherRepo.NextRunAt = ArmingArmed, &next
	thisRepo := armingFixture("dup", "0 9 * * *")
	thisRepo.RepoID = "repo-b"
	thisRepo.Arming = ArmingNotArmed

	unbound := armingFixture("dup", "0 9 * * *") // read before the backfill
	require.Empty(t, unbound.RepoID)

	got := ApplyLiveArming([]Task{unbound}, []Task{otherRepo, thisRepo})
	assert.Equal(t, ArmingUnknown, got[0].Arming,
		"two candidates bound to different repos are not something an unbound row may choose between")
	assert.Nil(t, got[0].NextRunAt)
}

func TestApplyLiveArmingStillTakesAnUnambiguousObservationWhileUnbound(t *testing.T) {
	// The fallback has to keep working for the case it exists for. One candidate
	// is never ambiguous however it is bound, and neither is a set that agrees —
	// refusing those would turn every mid-backfill poll into a blank rail.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	observed := armingFixture("a", "0 9 * * *")
	observed.RepoID = "repo-a"
	observed.Arming, observed.NextRunAt = ArmingArmed, &next

	unbound := armingFixture("a", "0 9 * * *")
	got := ApplyLiveArming([]Task{unbound}, []Task{observed})
	assert.Equal(t, ArmingArmed, got[0].Arming)
	require.NotNil(t, got[0].NextRunAt)
}
