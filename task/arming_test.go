package task

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oneRead and aLaterRead are two store generations. Everything below is read
// from oneRead unless a test is specifically about two reads that straddled a
// write, which is the case the generation exists to detect.
const (
	oneRead    = "1111aaaa2222bbbb"
	aLaterRead = "3333cccc4444dddd"
)

// armingFixture builds a record the way a loader hands it over: a definition
// plus the identity of the row it came from — which version of tasks.json, and
// which row of it.
//
// The row is a required argument rather than a defaulted one on purpose. It is
// half of the thing being matched on, so a fixture that let it default would pin
// the zero value — which means "no row" and pairs with nothing — and every test
// here would pass for the wrong reason.
func armingFixture(row int, id, cron string) Task {
	return Task{
		StoreGeneration: oneRead, Ordinal: row,
		ID: id, Name: "nightly", CronExpr: cron, ProjectPath: "/repo", Enabled: true,
	}
}

// fromALaterRead moves a record into the generation a later read produced, which
// is what every observation carries once a write has landed between the two
// reads.
func fromALaterRead(t Task) Task {
	t.StoreGeneration = aLaterRead
	return t
}

func TestApplyLiveArmingCarriesTheObservation(t *testing.T) {
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	observed := armingFixture(1, "a", "0 9 * * *")
	observed.Arming = ArmingArmed
	observed.NextRunAt = &next

	got := ApplyLiveArming([]Task{armingFixture(1, "a", "0 9 * * *")}, []Task{observed})

	assert.Equal(t, ArmingArmed, got[0].Arming)
	require.NotNil(t, got[0].NextRunAt, "an armed observation carries the entry's own next fire")
	assert.Equal(t, next, *got[0].NextRunAt)
}

func TestApplyLiveArmingLeavesUnobservedTasksUnknown(t *testing.T) {
	// The whole point of the tri-state: a task the daemon said nothing about must
	// not come back as not-armed. Reported as such it would accuse every task on
	// the box the moment a daemon was slow to answer.
	got := ApplyLiveArming([]Task{armingFixture(1, "a", "0 9 * * *")}, []Task{armingFixture(2, "b", "0 9 * * *")})
	assert.Equal(t, ArmingUnknown, got[0].Arming)
	assert.Nil(t, got[0].NextRunAt)
}

func TestApplyLiveArmingRefusesAnObservationAboutADifferentTrigger(t *testing.T) {
	// The two lists are two separate reads of tasks.json, so they can straddle an
	// edit. An observation adopted across one reports a fire time for a schedule
	// that is no longer the schedule — which is worse than reporting nothing.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	observed := armingFixture(1, "a", "0 9 * * *")
	observed.Arming = ArmingArmed
	observed.NextRunAt = &next

	edited := armingFixture(1, "a", "0 21 * * *") // the user just moved it to 9pm
	got := ApplyLiveArming([]Task{edited}, []Task{observed})

	assert.Equal(t, ArmingUnknown, got[0].Arming, "the armed entry is for the OLD expression")
	assert.Nil(t, got[0].NextRunAt, "and so is the fire time it computed")
}

func TestApplyLiveArmingRefusesAnObservationAboutADisabledTwin(t *testing.T) {
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	observed := armingFixture(1, "a", "0 9 * * *")
	observed.Arming = ArmingArmed
	observed.NextRunAt = &next

	disabled := armingFixture(1, "a", "0 9 * * *")
	disabled.Enabled = false
	got := ApplyLiveArming([]Task{disabled}, []Task{observed})

	assert.Equal(t, ArmingUnknown, got[0].Arming, "a task switched off is not armed, however recently it was")
	assert.Nil(t, got[0].NextRunAt)
}

func TestApplyLiveArmingComparesTheWatcherSignatureForWatchTasks(t *testing.T) {
	// A watcher RUNS a command in a directory under a name; a write commits and
	// reloads the supervisor as a separate step, so the old process can outlive
	// the edit. Each of watcherSignature's three fields has to invalidate.
	base := Task{StoreGeneration: oneRead, Ordinal: 1, ID: "w", Name: "build", WatchCmd: "tail -f log", ProjectPath: "/repo", Enabled: true}
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
	observed := armingFixture(1, "a", "0 9 * * *")
	observed.Arming = ArmingArmed
	watch := Task{StoreGeneration: oneRead, Ordinal: 1, ID: "a", Name: "nightly", WatchCmd: "tail -f log", ProjectPath: "/repo", Enabled: true}

	got := ApplyLiveArming([]Task{watch}, []Task{observed})
	assert.Equal(t, ArmingUnknown, got[0].Arming)
}

func TestApplyLiveArmingReportsEachDuplicateRowFromItsOwnObservation(t *testing.T) {
	// Only the FIRST row for an id is ever armed (#855) — and the daemon returns
	// an observation for EVERY row, marking each one after the first not-armed. So
	// the later duplicate has an authoritative negative waiting for it, and the
	// row that will never run is exactly the one worth marking.
	//
	// The two rows are indistinguishable by content: same id, same expression,
	// same project. Their ROWS are what tell them apart, which is why the
	// observations are handed over in REVERSE file order here — nothing about
	// arrival order or slice position may be doing the work.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	first := armingFixture(1, "a", "0 9 * * *")
	first.Arming = ArmingArmed
	first.NextRunAt = &next
	second := armingFixture(2, "a", "0 9 * * *")
	second.Arming = ArmingNotArmed

	got := ApplyLiveArming(
		[]Task{armingFixture(1, "a", "0 9 * * *"), armingFixture(2, "a", "0 9 * * *")},
		[]Task{second, first},
	)

	assert.Equal(t, ArmingArmed, got[0].Arming, "row 1 is the one the daemon armed")
	require.NotNil(t, got[0].NextRunAt)
	assert.Equal(t, ArmingNotArmed, got[1].Arming, "and row 2 is told it will not fire")
	assert.Nil(t, got[1].NextRunAt, "with no fire time, because nothing is holding it")
}

func TestApplyLiveArmingReportsADuplicateWithItsOwnTrigger(t *testing.T) {
	// The sharpest shape of the same thing: a later duplicate carrying a DIFFERENT
	// expression. It has its own observation, it is unambiguously not the row the
	// daemon armed, and leaving it unknown would render a computed next run off an
	// expression the scheduler is not holding at all.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	armed := armingFixture(1, "a", "0 9 * * *")
	armed.Arming, armed.NextRunAt = ArmingArmed, &next
	skipped := armingFixture(2, "a", "0 21 * * *")
	skipped.Arming = ArmingNotArmed

	got := ApplyLiveArming(
		[]Task{armingFixture(1, "a", "0 9 * * *"), armingFixture(2, "a", "0 21 * * *")},
		[]Task{armed, skipped},
	)

	assert.Equal(t, ArmingArmed, got[0].Arming)
	assert.Equal(t, ArmingNotArmed, got[1].Arming)
	assert.Nil(t, got[1].NextRunAt)
}

func TestApplyLiveArmingLeavesAnUnmatchedDuplicateUnknown(t *testing.T) {
	// The daemon's list is missing row 2 — it read the file before that row was
	// appended. Row 1's observation fits row 2 in every field, and row 2 must
	// still say nothing rather than borrow it.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	only := armingFixture(1, "a", "0 9 * * *")
	only.Arming, only.NextRunAt = ArmingArmed, &next

	got := ApplyLiveArming(
		[]Task{armingFixture(1, "a", "0 9 * * *"), armingFixture(2, "a", "0 9 * * *")},
		[]Task{only},
	)

	assert.Equal(t, ArmingArmed, got[0].Arming)
	assert.Equal(t, ArmingUnknown, got[1].Arming, "nothing was observed about row 2")
	assert.Nil(t, got[1].NextRunAt)
}

func TestApplyLiveArmingCarriesNotArmed(t *testing.T) {
	// The negative observation is the one #2929 was about: an enabled task the
	// daemon refused to arm. It has to cross over as readily as the positive.
	observed := armingFixture(1, "a", "0 9 * * *")
	observed.Arming = ArmingNotArmed

	got := ApplyLiveArming([]Task{armingFixture(1, "a", "0 9 * * *")}, []Task{observed})
	assert.Equal(t, ArmingNotArmed, got[0].Arming)
	assert.Nil(t, got[0].NextRunAt)
}

func TestApplyLiveArmingIsANoOpWithNothingObserved(t *testing.T) {
	tasks := []Task{armingFixture(1, "a", "0 9 * * *")}
	assert.Equal(t, tasks, ApplyLiveArming(tasks, nil))
	assert.Nil(t, ApplyLiveArming(nil, []Task{armingFixture(1, "a", "0 9 * * *")}))
}

func TestApplyLiveArmingTakesTheDuplicateObservationThatIsAboutThisRow(t *testing.T) {
	// The daemon skips a duplicated id GLOBALLY, first row wins, and answers about
	// every repo. A repo-scoped reader sees only its own rows, so the row the
	// daemon SKIPPED can be the only one in this list — locally FIRST, where any
	// first-wins rule cannot see the skip. Its row number still can: filtering
	// drops rows, it does not renumber the survivors.
	//
	// Two ways to get this wrong. Taking the other repo's ARMED row reports the
	// duplicate that will never run as scheduled. Refusing it but stopping there
	// discards the authoritative NOT-ARMED row for this record, which reads as
	// "unobserved" and suppresses the warning just as thoroughly.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	globallyFirst := armingFixture(1, "dup", "0 9 * * *")
	globallyFirst.ProjectPath = "/repo-a"
	globallyFirst.Arming, globallyFirst.NextRunAt = ArmingArmed, &next
	skipped := armingFixture(2, "dup", "0 9 * * *")
	skipped.ProjectPath = "/repo-b"
	skipped.Arming = ArmingNotArmed

	// What the repo-b rail loads: its row alone, first in ITS list and row 2 of
	// the store.
	local := armingFixture(2, "dup", "0 9 * * *")
	local.ProjectPath = "/repo-b"

	got := ApplyLiveArming([]Task{local}, []Task{globallyFirst, skipped})

	assert.Equal(t, ArmingNotArmed, got[0].Arming,
		"the observation about row 2 is the one that answers")
	assert.Nil(t, got[0].NextRunAt, "and it carries no fire time, because nothing is holding it")
}

func TestApplyLiveArmingStaysUnknownWhenNoObservationIsAboutThisRow(t *testing.T) {
	// The daemon answered, but only about the other repo's row — a list truncated
	// mid-write, or a store this reader has not seen all of. "Nothing reported on
	// THIS row" is the honest answer; inheriting the neighbour's is not.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	other := armingFixture(1, "dup", "0 9 * * *")
	other.ProjectPath = "/repo-a"
	other.Arming, other.NextRunAt = ArmingArmed, &next

	local := armingFixture(2, "dup", "0 9 * * *")
	local.ProjectPath = "/repo-b"

	got := ApplyLiveArming([]Task{local}, []Task{other})
	assert.Equal(t, ArmingUnknown, got[0].Arming)
	assert.Nil(t, got[0].NextRunAt)
}

func TestApplyLiveArmingPairsRowsThatAgreeOnEveryContentField(t *testing.T) {
	// The path-reused-across-repos case, and the one that shows why content ran
	// out. repoScope.matches treats a RETAINED RepoID as authoritative over the
	// path — that is what lets a task survive its project moving — so after a path
	// is deleted and reused, two rows can share an id, an expression AND a
	// ProjectPath while belonging to different repos. Every field the old matcher
	// compared agrees here except RepoID, which it had to learn to compare too.
	//
	// Nothing but the row tells these apart now, and the row is enough.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	oldRepo := armingFixture(1, "dup", "0 9 * * *")
	oldRepo.RepoID = "repo-a"
	oldRepo.Arming, oldRepo.NextRunAt = ArmingArmed, &next
	newRepo := armingFixture(2, "dup", "0 9 * * *")
	newRepo.RepoID = "repo-b"
	newRepo.Arming = ArmingNotArmed

	local := armingFixture(2, "dup", "0 9 * * *")
	local.RepoID = "repo-b"

	got := ApplyLiveArming([]Task{local}, []Task{oldRepo, newRepo})
	assert.Equal(t, ArmingNotArmed, got[0].Arming,
		"the observation about this row is the one that answers, whatever the other row is bound to")
	assert.Nil(t, got[0].NextRunAt)
}

func TestApplyLiveArmingIsUnaffectedByAnUnbackfilledRepoID(t *testing.T) {
	// RepoID is daemon-backfilled, so two reads of the same file can straddle a
	// backfill and disagree about a record nobody changed. The old matcher had to
	// tolerate that explicitly, and tolerance is a hole: an empty id matched
	// anything. Rows do not move when a field is filled in, so the straddle is now
	// invisible to the match rather than forgiven by it.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	observed := armingFixture(1, "a", "0 9 * * *")
	observed.RepoID = "repo-a"
	observed.Arming, observed.NextRunAt = ArmingArmed, &next

	legacy := armingFixture(1, "a", "0 9 * * *") // read before the backfill landed
	require.Empty(t, legacy.RepoID)

	got := ApplyLiveArming([]Task{legacy}, []Task{observed})
	assert.Equal(t, ArmingArmed, got[0].Arming, "an unbound id is not a different row")
	require.NotNil(t, got[0].NextRunAt)
	assert.Equal(t, next, *got[0].NextRunAt)
}

func TestApplyLiveArmingResolvesConflictingBindingsByRow(t *testing.T) {
	// The backfill straddle at its worst, and the case that used to end in a
	// REFUSAL — the seventh fix, and the signal that a content match had run out
	// of information. The disk read landed before the daemon wrote this row's
	// RepoID and ListTasks landed after, so the local record is unbound while the
	// observations are not; a reused path means an older row retained to ANOTHER
	// repo shares the id, the path and the expression.
	//
	// An unbound row cannot choose between those two by content, and the honest
	// answer there was "unknown". It does not have to choose: it knows its row,
	// and the row carries the not-armed answer that is actually about it.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	otherRepo := armingFixture(1, "dup", "0 9 * * *")
	otherRepo.RepoID = "repo-a"
	otherRepo.Arming, otherRepo.NextRunAt = ArmingArmed, &next
	thisRepo := armingFixture(2, "dup", "0 9 * * *")
	thisRepo.RepoID = "repo-b"
	thisRepo.Arming = ArmingNotArmed

	unbound := armingFixture(2, "dup", "0 9 * * *") // read before the backfill
	require.Empty(t, unbound.RepoID)

	got := ApplyLiveArming([]Task{unbound}, []Task{otherRepo, thisRepo})
	assert.Equal(t, ArmingNotArmed, got[0].Arming,
		"the row that used to be unanswerable now answers exactly, and answers not-armed")
	assert.Nil(t, got[0].NextRunAt, "nothing is holding it, so there is no fire time to show")
}

func TestApplyLiveArmingWorksOnAStoreWithNoBindingsAtAll(t *testing.T) {
	// The fully legacy store, where NOTHING carries a RepoID on either side. That
	// is the shape the old fallback existed for, and it has to keep working — a
	// mid-backfill poll must not blank the rail.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	observed := armingFixture(1, "a", "0 9 * * *")
	observed.Arming, observed.NextRunAt = ArmingArmed, &next
	require.Empty(t, observed.RepoID)

	unbound := armingFixture(1, "a", "0 9 * * *")
	got := ApplyLiveArming([]Task{unbound}, []Task{observed})
	assert.Equal(t, ArmingArmed, got[0].Arming)
	require.NotNil(t, got[0].NextRunAt)
}

func TestApplyLiveArmingRefusesObservationsShiftedByAStraddlingWrite(t *testing.T) {
	// sameTrigger on its own, with the generation held equal so the guard behind
	// it is what is being measured. In production the generation catches this
	// first — an insert rewrites the file — and this pins that the assertion
	// standing behind it is real rather than decorative.
	//
	// The reads straddled an INSERT, which is the mutation that moves rows: the
	// rail read the file before a task was prepended, the daemon read it after, so
	// the daemon's row 1 is a task the rail has never seen and its row 2 is the
	// rail's row 1.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	inserted := armingFixture(1, "new", "0 6 * * *")
	inserted.Arming, inserted.NextRunAt = ArmingArmed, &next
	shiftedA := armingFixture(2, "a", "0 9 * * *")
	shiftedA.Arming, shiftedA.NextRunAt = ArmingArmed, &next
	shiftedB := armingFixture(3, "b", "0 21 * * *")
	shiftedB.Arming = ArmingNotArmed

	before := []Task{armingFixture(1, "a", "0 9 * * *"), armingFixture(2, "b", "0 21 * * *")}
	got := ApplyLiveArming(before, []Task{inserted, shiftedA, shiftedB})

	assert.Equal(t, ArmingUnknown, got[0].Arming, "row 1 now holds a different task entirely")
	assert.Nil(t, got[0].NextRunAt)
	assert.Equal(t, ArmingUnknown, got[1].Arming, "and row 2 holds the task that used to be row 1")
	assert.Nil(t, got[1].NextRunAt)
}

func TestApplyLiveArmingRefusesAShiftedRowThatAgreesOnEverythingButItsID(t *testing.T) {
	// The straddling REMOVAL, and the twin of the insert above — it shifts rows the
	// other way. The rail read the file before a row was deleted and the daemon
	// after, so the rail's row 1 is a task that no longer exists and the daemon's
	// row 1 is the task that used to be row 2.
	//
	// Configured alike — same project, same expression, both enabled — those two
	// agree on every field this guard compares except the id, which no update verb
	// can change. Without comparing it, the deleted task would take the survivor's
	// "armed" and the survivor's next fire: a fabricated armed for a task the
	// daemon never reported on (#3684 review). The id cannot REPLACE the row, since
	// a hand-edited store can duplicate it; it is the staleness half of the guard,
	// and it was implicit while the lookup was keyed on the id.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	survivor := armingFixture(1, "kept", "0 9 * * *")
	survivor.Arming, survivor.NextRunAt = ArmingArmed, &next

	// What the rail is still holding: the row that was deleted, and the one that
	// has since moved up into its place.
	stale := []Task{armingFixture(1, "gone", "0 9 * * *"), armingFixture(2, "kept", "0 9 * * *")}
	got := ApplyLiveArming(stale, []Task{survivor})

	assert.Equal(t, ArmingUnknown, got[0].Arming,
		"row 1 now holds a different task, and a deleted one must not inherit its answer")
	assert.Nil(t, got[0].NextRunAt, "least of all its next fire")
	assert.Equal(t, ArmingUnknown, got[1].Arming, "and nothing was observed about row 2 any more")
	assert.Nil(t, got[1].NextRunAt)
}

func TestApplyLiveArmingRefusesDuplicateRowsShiftedByARemoval(t *testing.T) {
	// The case sameTrigger cannot see, and the reason the row is qualified by the
	// generation it was read in (#3684 review).
	//
	// The store held an unrelated row 1, then a duplicated id at rows 2 and 3 —
	// same id, same project, same expression, which is the store this whole
	// feature is about. The daemon arms the FIRST occurrence, so row 2 is armed and
	// row 3 is the skipped duplicate. Then row 1 is deleted, and every row below it
	// moves up: the armed task is now row 1 and the skipped duplicate is now row 2.
	//
	// The rail is still holding its earlier read, where the armed task is row 2. A
	// row-number lookup alone hands it the observation now sitting at row 2 — the
	// SKIPPED duplicate's not-armed — and every field sameTrigger compares agrees,
	// id included, because the two rows are copies of each other. The rail would
	// mark a task that is genuinely armed as one that will never fire.
	//
	// Nothing about the two records can distinguish them. What can is that the two
	// reads saw different bytes.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	armedNow := fromALaterRead(armingFixture(1, "dup", "0 9 * * *"))
	armedNow.Arming, armedNow.NextRunAt = ArmingArmed, &next
	skippedNow := fromALaterRead(armingFixture(2, "dup", "0 9 * * *"))
	skippedNow.Arming = ArmingNotArmed

	// What the rail read before the delete: the armed row at 2, its duplicate at 3.
	stale := []Task{armingFixture(2, "dup", "0 9 * * *"), armingFixture(3, "dup", "0 9 * * *")}
	got := ApplyLiveArming(stale, []Task{armedNow, skippedNow})

	assert.Equal(t, ArmingUnknown, got[0].Arming,
		"the armed task must not be reported not-armed by the duplicate that took its row number")
	assert.Nil(t, got[0].NextRunAt)
	assert.Equal(t, ArmingUnknown, got[1].Arming,
		"and the duplicate's row is not comparable across the write either")
	assert.Nil(t, got[1].NextRunAt)
}

func TestApplyLiveArmingPairsWhenBothReadsSawTheSameStore(t *testing.T) {
	// The other half of the generation, and the one that keeps it from being a
	// blanket refusal: reads that saw the same bytes pair exactly, duplicates
	// included. Without this the fix above would be indistinguishable from
	// switching the feature off.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	armed := armingFixture(2, "dup", "0 9 * * *")
	armed.Arming, armed.NextRunAt = ArmingArmed, &next
	skipped := armingFixture(3, "dup", "0 9 * * *")
	skipped.Arming = ArmingNotArmed

	got := ApplyLiveArming(
		[]Task{armingFixture(2, "dup", "0 9 * * *"), armingFixture(3, "dup", "0 9 * * *")},
		[]Task{skipped, armed},
	)

	assert.Equal(t, ArmingArmed, got[0].Arming, "row 2 is the occurrence the daemon armed")
	require.NotNil(t, got[0].NextRunAt)
	assert.Equal(t, ArmingNotArmed, got[1].Arming, "row 3 is the duplicate it skipped")
	assert.Nil(t, got[1].NextRunAt)
}

func TestApplyLiveArmingIgnoresRecordsWithNoGeneration(t *testing.T) {
	// The version-skew half that the ordinal alone cannot cover: an older daemon's
	// response carries neither field, and JSON decodes both absences to their zero
	// values. A record that cannot say which read it came from pairs with nothing.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)

	t.Run("observation from no read", func(t *testing.T) {
		observed := armingFixture(1, "a", "0 9 * * *")
		observed.StoreGeneration = ""
		observed.Arming, observed.NextRunAt = ArmingArmed, &next

		got := ApplyLiveArming([]Task{armingFixture(1, "a", "0 9 * * *")}, []Task{observed})
		assert.Equal(t, ArmingUnknown, got[0].Arming)
		assert.Nil(t, got[0].NextRunAt)
	})

	t.Run("record from no read", func(t *testing.T) {
		observed := armingFixture(1, "a", "0 9 * * *")
		observed.Arming, observed.NextRunAt = ArmingArmed, &next

		local := armingFixture(1, "a", "0 9 * * *")
		local.StoreGeneration = ""
		got := ApplyLiveArming([]Task{local}, []Task{observed})
		assert.Equal(t, ArmingUnknown, got[0].Arming)
		assert.Nil(t, got[0].NextRunAt)
	})
}

func TestApplyLiveArmingIgnoresUnnumberedRecords(t *testing.T) {
	// Version skew. An older daemon's ListTasks response carries no ordinal field
	// at all, and JSON decodes that absence to the zero value — so the type has to
	// be able to say "no row" rather than "row 0", or every such record would
	// claim the first row's observation and a MISSING field would manufacture an
	// answer.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)

	t.Run("unnumbered observation", func(t *testing.T) {
		observed := armingFixture(unstampedOrdinal, "a", "0 9 * * *")
		observed.Arming, observed.NextRunAt = ArmingArmed, &next

		got := ApplyLiveArming([]Task{armingFixture(1, "a", "0 9 * * *")}, []Task{observed})
		assert.Equal(t, ArmingUnknown, got[0].Arming)
		assert.Nil(t, got[0].NextRunAt)
	})

	t.Run("unnumbered record", func(t *testing.T) {
		observed := armingFixture(1, "a", "0 9 * * *")
		observed.Arming, observed.NextRunAt = ArmingArmed, &next

		got := ApplyLiveArming([]Task{armingFixture(unstampedOrdinal, "a", "0 9 * * *")}, []Task{observed})
		assert.Equal(t, ArmingUnknown, got[0].Arming)
		assert.Nil(t, got[0].NextRunAt)
	})
}

func TestApplyLiveArmingRefusesARowTwoObservationsClaim(t *testing.T) {
	// A row number identifies a row within ONE tasks.json, so a list holding two
	// claims on row 1 is not one read of one file — the shape a merge of two
	// stores would produce, and the reason the remote fetch is declined today
	// (app/session_control.go's liveTaskArmingFetcher). Neither claimant is
	// trustworthy, so the row stays unknown rather than being resolved by arrival
	// order.
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	mine := armingFixture(1, "a", "0 9 * * *")
	mine.Arming = ArmingNotArmed
	theirs := armingFixture(1, "a", "0 9 * * *")
	theirs.Arming, theirs.NextRunAt = ArmingArmed, &next

	got := ApplyLiveArming([]Task{armingFixture(1, "a", "0 9 * * *")}, []Task{mine, theirs})
	assert.Equal(t, ArmingUnknown, got[0].Arming,
		"two stores both have a row 1; neither answer may be adopted")
	assert.Nil(t, got[0].NextRunAt)
}
