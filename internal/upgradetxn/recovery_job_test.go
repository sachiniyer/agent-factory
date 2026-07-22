package upgradetxn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type recoveryCommandCall struct {
	name string
	args []string
}

type fakeRecoveryJobRuntime struct {
	commands       []recoveryCommandCall
	detached       []recoveryCommandCall
	commandErr     error
	commandResults []error
	detachedErr    error
	launchdUserID  int
}

func (f *fakeRecoveryJobRuntime) controller() RecoveryJobController {
	return RecoveryJobController{
		RunCommand: func(_ context.Context, name string, args ...string) error {
			f.commands = append(f.commands, recoveryCommandCall{name: name, args: append([]string(nil), args...)})
			if len(f.commandResults) > 0 {
				result := f.commandResults[0]
				f.commandResults = f.commandResults[1:]
				return result
			}
			return f.commandErr
		},
		StartDetached: func(name string, args ...string) error {
			f.detached = append(f.detached, recoveryCommandCall{name: name, args: append([]string(nil), args...)})
			return f.detachedErr
		},
		UserID: func() int { return f.launchdUserID },
	}
}

func prepareRecoveryJobFixture(t *testing.T, kind RecoveryJobKind) *Transaction {
	t.Helper()
	home := t.TempDir()
	binDir := t.TempDir()
	executable := filepath.Join(binDir, "af")
	require.NoError(t, os.WriteFile(executable, []byte("known-running-binary"), 0o755))
	unitDir := ""
	if kind != RecoveryJobDetached {
		unitDir = t.TempDir()
	}
	job, err := NewRecoveryJob(kind, "txn-recovery-job", unitDir)
	require.NoError(t, err)
	owner := DaemonOwner{Kind: SupervisionAdHoc}
	if kind == RecoveryJobSystemd {
		owner = DaemonOwner{Kind: SupervisionSystemd, ServiceName: "agent-factory-daemon.service"}
	}
	if kind == RecoveryJobLaunchd {
		owner = DaemonOwner{Kind: SupervisionLaunchd, ServiceName: "com.agent-factory.daemon"}
	}
	txn, err := Prepare(Plan{
		ID:             "txn-recovery-job",
		HomeDir:        home,
		ExecutablePath: executable,
		FromVersion:    "1.0.206",
		ToVersion:      "1.0.207",
		Candidate:      []byte("candidate-binary"),
		Daemon: DaemonSnapshot{
			WasRunning: true,
			BootID:     "previous-boot-id",
			Owner:      owner,
		},
		RecoveryJob: job,
	})
	require.NoError(t, err)
	return txn
}

func TestNewRecoveryJobCanonicalizesExistingUnitDirectory(t *testing.T) {
	realDirectory := t.TempDir()
	alias := filepath.Join(t.TempDir(), "units")
	require.NoError(t, os.Symlink(realDirectory, alias))
	canonicalDirectory, err := filepath.EvalSymlinks(realDirectory)
	require.NoError(t, err)

	job, err := NewRecoveryJob(RecoveryJobSystemd, "txn-canonical-unit-dir", alias)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(canonicalDirectory, job.Name), job.UnitPath)

	created, err := ensureExactRecoveryUnit(job.UnitPath, []byte("recovery unit\n"))
	require.NoError(t, err)
	require.True(t, created)
}

func TestRecoveryUnitReadersRejectFIFOWithoutBlocking(t *testing.T) {
	tests := []struct {
		name string
		run  func(string) error
	}{
		{
			name: "startup inspection",
			run: func(path string) error {
				_, err := inspectExactRecoveryUnit(path, []byte("expected unit\n"))
				return err
			},
		},
		{
			name: "cleanup inspection",
			run: func(path string) error {
				return removeExactRecoveryUnit(Journal{
					ID: "txn-fifo",
					RecoveryJob: RecoveryJob{
						Kind:     RecoveryJobSystemd,
						Name:     "agent-factory-upgrade-recovery-txn-fifo.service",
						UnitPath: path,
					},
				})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "recovery.service")
			require.NoError(t, syscall.Mkfifo(path, recoveryUnitMode))
			result := make(chan error, 1)
			go func() { result <- tc.run(path) }()

			select {
			case err := <-result:
				require.ErrorContains(t, err, "not a regular file")
			case <-time.After(2 * time.Second):
				// Unblock the old read-before-stat implementation before failing so
				// the regression test never leaves a goroutine behind.
				writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				select {
				case <-result:
				case <-time.After(time.Second):
					require.FailNow(t, "FIFO inspection remained blocked after releasing its reader")
				}
				require.FailNow(t, "FIFO inspection blocked before checking the file type")
			}
		})
	}
}

func TestSystemdRecoveryJobIsPersistentAndRunsOnlyPreviousBinary(t *testing.T) {
	txn := prepareRecoveryJobFixture(t, RecoveryJobSystemd)
	runtime := &fakeRecoveryJobRuntime{}

	require.NoError(t, runtime.controller().InstallAndStart(context.Background(), txn))

	journal := txn.Journal()
	content, err := os.ReadFile(journal.RecoveryJob.UnitPath)
	require.NoError(t, err)
	info, err := os.Stat(journal.RecoveryJob.UnitPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(recoveryUnitMode), info.Mode().Perm())
	unit := string(content)
	require.Contains(t, unit, "Before="+journal.Daemon.Owner.ServiceName)
	require.Contains(t, unit, "Restart=on-failure")
	require.Contains(t, unit, "WantedBy=default.target")
	require.Contains(t, unit, systemdQuoteArgument(journal.PreviousBinaryPath))
	require.Contains(t, unit, systemdQuoteArgument(journal.HomeDir))
	require.NotContains(t, unit, systemdQuoteArgument(journal.ExecutablePath),
		"the replaceable canonical binary must never be the recovery actor")
	require.Equal(t, journal.PreviousBinaryPath, recoveryCommand(journal)[0])
	require.Equal(t, []recoveryCommandCall{
		{name: "systemctl", args: []string{"--user", "daemon-reload"}},
		{name: "systemctl", args: []string{"--user", "enable", "--now", journal.RecoveryJob.Name}},
	}, runtime.commands)
}

func TestLaunchdRecoveryJobSurvivesActorLossAndUsesTokenizedArguments(t *testing.T) {
	txn := prepareRecoveryJobFixture(t, RecoveryJobLaunchd)
	runtime := &fakeRecoveryJobRuntime{launchdUserID: 501}

	require.NoError(t, runtime.controller().InstallAndStart(context.Background(), txn))

	journal := txn.Journal()
	content, err := os.ReadFile(journal.RecoveryJob.UnitPath)
	require.NoError(t, err)
	plist := string(content)
	for _, value := range recoveryCommand(journal) {
		require.Contains(t, plist, "<string>"+xmlEscape(value)+"</string>")
	}
	require.Contains(t, plist, "<key>RunAtLoad</key>")
	require.Contains(t, plist, "<key>KeepAlive</key>")
	require.Contains(t, plist, "<key>SuccessfulExit</key>")
	require.NotContains(t, plist, "<key>PathState</key>",
		"a clean lock loser must not be relaunched while the winning actor is live")
	require.Equal(t, []recoveryCommandCall{{
		name: "launchctl", args: []string{"enable", "gui/501/" + journal.RecoveryJob.Name},
	}, {
		name: "launchctl", args: []string{"bootstrap", "gui/501", journal.RecoveryJob.UnitPath},
	}}, runtime.commands)
}

func TestDetachedRecoveryJobStartsASeparatePreviousBinaryActor(t *testing.T) {
	txn := prepareRecoveryJobFixture(t, RecoveryJobDetached)
	runtime := &fakeRecoveryJobRuntime{}

	require.NoError(t, runtime.controller().InstallAndStart(context.Background(), txn))

	command := recoveryCommand(txn.Journal())
	require.Equal(t, []recoveryCommandCall{{name: command[0], args: command[1:]}}, runtime.detached)
	require.Empty(t, runtime.commands)
}

func TestRecoveryJobWakeUsesNonDestructiveManagerStartVerbs(t *testing.T) {
	tests := []struct {
		kind           RecoveryJobKind
		want           func(Journal) []recoveryCommandCall
		commandResults []error
	}{
		{
			kind: RecoveryJobSystemd,
			want: func(Journal) []recoveryCommandCall {
				return []recoveryCommandCall{
					{name: "systemctl", args: []string{"--user", "daemon-reload"}},
					{name: "systemctl", args: []string{"--user", "enable", "agent-factory-upgrade-recovery-txn-recovery-job.service"}},
					{name: "systemctl", args: []string{"--user", "start", "agent-factory-upgrade-recovery-txn-recovery-job.service"}},
				}
			},
		},
		{
			kind: RecoveryJobLaunchd,
			want: func(journal Journal) []recoveryCommandCall {
				return []recoveryCommandCall{
					{name: "launchctl", args: []string{"enable", "gui/502/com.agent-factory.upgrade-recovery.txn-recovery-job"}},
					{name: "launchctl", args: []string{"bootstrap", "gui/502", journal.RecoveryJob.UnitPath}},
					{name: "launchctl", args: []string{"kickstart", "gui/502/com.agent-factory.upgrade-recovery.txn-recovery-job"}},
				}
			},
			commandResults: []error{nil, errors.New("already bootstrapped"), nil},
		},
	}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			txn := prepareRecoveryJobFixture(t, tc.kind)
			runtime := &fakeRecoveryJobRuntime{launchdUserID: 502}
			controller := runtime.controller()
			require.NoError(t, controller.InstallAndStart(context.Background(), txn))
			runtime.commands = nil
			runtime.commandResults = append([]error(nil), tc.commandResults...)

			require.NoError(t, controller.Wake(context.Background(), txn))
			require.Equal(t, tc.want(txn.Journal()), runtime.commands)
			for _, call := range runtime.commands {
				require.NotContains(t, call.args, "restart")
				require.NotContains(t, call.args, "-k")
			}
		})
	}
}

func TestRecoveryJobWakeRepairsPublishedButUnregisteredJobAfterCrash(t *testing.T) {
	tests := []struct {
		kind RecoveryJobKind
		want func(Journal) []recoveryCommandCall
	}{
		{
			kind: RecoveryJobSystemd,
			want: func(Journal) []recoveryCommandCall {
				return []recoveryCommandCall{
					{name: "systemctl", args: []string{"--user", "daemon-reload"}},
					{name: "systemctl", args: []string{"--user", "enable", "agent-factory-upgrade-recovery-txn-recovery-job.service"}},
					{name: "systemctl", args: []string{"--user", "start", "agent-factory-upgrade-recovery-txn-recovery-job.service"}},
				}
			},
		},
		{
			kind: RecoveryJobLaunchd,
			want: func(journal Journal) []recoveryCommandCall {
				return []recoveryCommandCall{
					{name: "launchctl", args: []string{"enable", "gui/504/com.agent-factory.upgrade-recovery.txn-recovery-job"}},
					{name: "launchctl", args: []string{"bootstrap", "gui/504", journal.RecoveryJob.UnitPath}},
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			txn := prepareRecoveryJobFixture(t, tc.kind)
			journal := txn.Journal()
			var content []byte
			switch tc.kind {
			case RecoveryJobSystemd:
				content = []byte(renderSystemdRecoveryUnit(journal))
			case RecoveryJobLaunchd:
				content = []byte(renderLaunchdRecoveryPlist(journal))
			}
			created, err := ensureExactRecoveryUnit(journal.RecoveryJob.UnitPath, content)
			require.NoError(t, err)
			require.True(t, created)
			runtime := &fakeRecoveryJobRuntime{launchdUserID: 504}

			require.NoError(t, runtime.controller().Wake(context.Background(), txn))
			require.Equal(t, tc.want(journal), runtime.commands,
				"a durable unit file does not prove the service manager registered or enabled it")
		})
	}
}

func TestRecoveryJobWakeLeavesKernelLiveActorAlone(t *testing.T) {
	txn := prepareRecoveryJobFixture(t, RecoveryJobSystemd)
	runtime := &fakeRecoveryJobRuntime{}
	controller := runtime.controller()
	require.NoError(t, controller.InstallAndStart(context.Background(), txn))
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	runtime.commands = nil

	err = controller.Wake(context.Background(), txn)
	require.ErrorIs(t, err, ErrRecoveryActive)
	require.Empty(t, runtime.commands,
		"a positive flock observation must precede and suppress every manager wake")
}

func TestRecoveryJobInstallRetryAcceptsAnAlreadyLiveActor(t *testing.T) {
	txn := prepareRecoveryJobFixture(t, RecoveryJobSystemd)
	runtime := &fakeRecoveryJobRuntime{}
	controller := runtime.controller()
	require.NoError(t, controller.InstallAndStart(context.Background(), txn))
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	runtime.commands = nil

	require.NoError(t, controller.InstallAndStart(context.Background(), txn))
	require.Empty(t, runtime.commands,
		"an ambiguous first start may have succeeded; retry must trust the flock instead of starting again")
}

func TestDisableRecoveryJobDisarmsRestartWithoutStoppingCurrentActor(t *testing.T) {
	tests := []struct {
		kind RecoveryJobKind
		want []recoveryCommandCall
	}{
		{
			kind: RecoveryJobSystemd,
			want: []recoveryCommandCall{{
				name: "systemctl",
				args: []string{"--user", "disable", "agent-factory-upgrade-recovery-txn-recovery-job.service"},
			}},
		},
		{
			kind: RecoveryJobLaunchd,
			want: []recoveryCommandCall{{
				name: "launchctl",
				args: []string{"disable", "gui/503/com.agent-factory.upgrade-recovery.txn-recovery-job"},
			}},
		},
	}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			txn := prepareRecoveryJobFixture(t, tc.kind)
			runtime := &fakeRecoveryJobRuntime{launchdUserID: 503}
			controller := runtime.controller()
			require.NoError(t, controller.InstallAndStart(context.Background(), txn))
			runtime.commands = nil
			lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
			require.NoError(t, err)
			require.NoError(t, lease.Abort())

			require.NoError(t, controller.Disable(context.Background(), txn.Journal()))

			require.Equal(t, tc.want, runtime.commands)
			for _, call := range runtime.commands {
				require.NotContains(t, call.args, "--now",
					"disable must not kill the actor before it removes the active journal")
				require.NotContains(t, call.args, "bootout",
					"launchd bootout would kill the actor which still owns cleanup")
			}
			_, err = os.Stat(txn.Journal().RecoveryJob.UnitPath)
			require.NoError(t, err,
				"disable disarms the manager, but active-journal cleanup owns artifact removal")
			require.NoError(t, lease.Cleanup())
			_, err = os.Stat(txn.Journal().RecoveryJob.UnitPath)
			require.ErrorIs(t, err, os.ErrNotExist)
			require.NoError(t, lease.Release())
		})
	}
}

func TestRecoveryJobManagerFailureRetainsTheDurableUnitForTakeover(t *testing.T) {
	txn := prepareRecoveryJobFixture(t, RecoveryJobSystemd)
	runtime := &fakeRecoveryJobRuntime{commandErr: errors.New("manager unavailable")}

	err := runtime.controller().InstallAndStart(context.Background(), txn)
	require.ErrorContains(t, err, "reload systemd recovery job")
	exact, readErr := os.ReadFile(txn.Journal().RecoveryJob.UnitPath)
	require.NoError(t, readErr,
		"an ambiguous service-manager failure must not erase durable recovery state")

	// The exact file is idempotent across both successful and failed retries.
	runtime.commandErr = nil
	require.NoError(t, runtime.controller().InstallAndStart(context.Background(), txn))
	got, err := os.ReadFile(txn.Journal().RecoveryJob.UnitPath)
	require.NoError(t, err)
	require.Equal(t, exact, got)
	runtime.commandErr = errors.New("manager unavailable")
	err = runtime.controller().InstallAndStart(context.Background(), txn)
	require.Error(t, err)
	stillThere, err := os.ReadFile(txn.Journal().RecoveryJob.UnitPath)
	require.NoError(t, err)
	require.Equal(t, exact, stillThere)
}

func TestRecoveryJobRefusesToOverwriteUnexpectedUnitContent(t *testing.T) {
	txn := prepareRecoveryJobFixture(t, RecoveryJobSystemd)
	path := txn.Journal().RecoveryJob.UnitPath
	require.NoError(t, os.WriteFile(path, []byte("foreign unit\n"), 0o600))
	runtime := &fakeRecoveryJobRuntime{}

	err := runtime.controller().InstallAndStart(context.Background(), txn)
	require.ErrorContains(t, err, "does not match")
	require.Empty(t, runtime.commands)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "foreign unit\n", string(content))
}

func TestRecoveryUnitPublicationIsAtomicAcrossEntrypointRaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.service")
	content := []byte("complete unit\n")
	const contenders = 20
	start := make(chan struct{})
	results := make(chan bool, contenders)
	errorsCh := make(chan error, contenders)
	var wait sync.WaitGroup
	for range contenders {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			created, err := ensureExactRecoveryUnit(path, content)
			results <- created
			errorsCh <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsCh)
	createdCount := 0
	for created := range results {
		if created {
			createdCount++
		}
	}
	for err := range errorsCh {
		require.NoError(t, err)
	}
	require.Equal(t, 1, createdCount)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestRecoveryCleanupRefusesToDeleteAReplacedUnit(t *testing.T) {
	txn := prepareRecoveryJobFixture(t, RecoveryJobSystemd)
	runtime := &fakeRecoveryJobRuntime{}
	controller := runtime.controller()
	require.NoError(t, controller.InstallAndStart(context.Background(), txn))
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Abort())
	require.NoError(t, controller.Disable(context.Background(), txn.Journal()))
	path := txn.Journal().RecoveryJob.UnitPath
	require.NoError(t, os.WriteFile(path, []byte("replacement unit\n"), recoveryUnitMode))

	err = lease.Cleanup()
	require.ErrorContains(t, err, "refusing to remove")
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "replacement unit\n", string(content))
}

func TestRecoveryCommandEscapesServiceManagerMetacharacters(t *testing.T) {
	journal := Journal{
		ID:                 "txn-escape",
		HomeDir:            `/tmp/home with $dollar%percent"quote&xml`,
		PreviousBinaryPath: `/tmp/bin with $dollar%percent"quote&xml`,
	}
	unit := renderSystemdRecoveryUnit(journal)
	require.NotContains(t, unit, journal.PreviousBinaryPath)
	require.NotContains(t, unit, journal.HomeDir)
	require.Contains(t, unit, `$$dollar%%percent\"quote&xml`)
	plist := renderLaunchdRecoveryPlist(journal)
	require.NotContains(t, plist, `&xml`)
	require.Contains(t, plist, `&amp;xml`)
	require.True(t, strings.Contains(plist, `&#34;quote`))
	require.NotContains(t, systemdQuoteArgument("safe\n[Service]\nExecStart=/tmp/evil"), "\n",
		"a newline in a path must not inject another unit directive")
}
