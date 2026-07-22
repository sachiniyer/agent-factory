package upgradetxn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// recoveryModeArgument is intentionally not a Cobra flag or public command.
// The previous-binary entrypoint consumes it before normal command startup;
// candidates may wake this exact command but cannot acquire its RecoveryLease.
const recoveryModeArgument = "__upgrade-recovery"

const recoveryUnitMode = 0o600

// RecoveryJobController owns only the platform launcher around the recovery
// state machine. Supervisor owns all daemon and transaction decisions. The
// seams keep tests from invoking the host's service manager or starting a
// process, while zero values use the real platform operations.
type RecoveryJobController struct {
	RunCommand    func(context.Context, string, ...string) error
	StartDetached func(string, ...string) error
	UserID        func() int
}

// InstallAndStart publishes the transaction-scoped persistent launcher and
// starts the immutable previous-binary actor. It is valid only before the
// daemon begins shutdown. Once published, an exact unit remains recovery
// state even when the service-manager call reports an ambiguous failure.
func (c RecoveryJobController) InstallAndStart(ctx context.Context, txn *Transaction) error {
	journal, err := currentRecoveryJournal(txn)
	if err != nil {
		return err
	}
	if journal.Phase != PhasePrepared {
		return fmt.Errorf("cannot install upgrade recovery job in phase %s", journal.Phase)
	}
	if err := verifyRecoveryActorArtifact(journal); err != nil {
		return err
	}
	live, err := txn.RecoveryActorLive()
	if err != nil {
		return err
	}
	if live {
		return nil
	}

	switch journal.RecoveryJob.Kind {
	case RecoveryJobDetached:
		return c.startDetached(journal)
	case RecoveryJobSystemd:
		_, err := ensureExactRecoveryUnit(
			journal.RecoveryJob.UnitPath, []byte(renderSystemdRecoveryUnit(journal)))
		if err != nil {
			return err
		}
		if err := c.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return fmt.Errorf("reload systemd recovery job: %w", err)
		}
		if err := c.run(ctx, "systemctl", "--user", "enable", "--now", journal.RecoveryJob.Name); err != nil {
			return fmt.Errorf("enable systemd recovery job: %w", err)
		}
		return nil
	case RecoveryJobLaunchd:
		_, err := ensureExactRecoveryUnit(
			journal.RecoveryJob.UnitPath, []byte(renderLaunchdRecoveryPlist(journal)))
		if err != nil {
			return err
		}
		return c.armAndStartLaunchd(ctx, journal)
	default:
		return fmt.Errorf("unsupported upgrade recovery job kind %q", journal.RecoveryJob.Kind)
	}
}

// Wake asks the journaled launcher to start an inactive recovery actor. It
// never uses restart, kickstart -k, or another operation that can replace a
// live owner. The flock is checked first, then the service manager arbitrates
// the unavoidable probe-to-start race.
func (c RecoveryJobController) Wake(ctx context.Context, txn *Transaction) error {
	journal, err := currentRecoveryJournal(txn)
	if err != nil {
		return err
	}
	if journal.Phase == PhaseRollbackFailed {
		return ErrRollbackRecoveryFailed
	}
	if err := verifyRecoveryActorArtifact(journal); err != nil {
		return err
	}
	live, err := txn.RecoveryActorLive()
	if err != nil {
		return err
	}
	if live {
		return ErrRecoveryActive
	}

	switch journal.RecoveryJob.Kind {
	case RecoveryJobDetached:
		return c.startDetached(journal)
	case RecoveryJobSystemd:
		_, err := ensureExactRecoveryUnit(
			journal.RecoveryJob.UnitPath, []byte(renderSystemdRecoveryUnit(journal)))
		if err != nil {
			return err
		}
		// The unit file may have survived a process death before daemon-reload,
		// or the job may have been disabled immediately before cleanup died.
		// File existence therefore proves neither registration nor enablement.
		if err := c.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return fmt.Errorf("reload systemd recovery job for takeover: %w", err)
		}
		if err := c.run(ctx, "systemctl", "--user", "enable", journal.RecoveryJob.Name); err != nil {
			return fmt.Errorf("enable systemd recovery job for takeover: %w", err)
		}
		if err := c.run(ctx, "systemctl", "--user", "start", journal.RecoveryJob.Name); err != nil {
			return fmt.Errorf("start systemd recovery job for takeover: %w", err)
		}
		return nil
	case RecoveryJobLaunchd:
		_, err := ensureExactRecoveryUnit(
			journal.RecoveryJob.UnitPath, []byte(renderLaunchdRecoveryPlist(journal)))
		if err != nil {
			return err
		}
		return c.armAndStartLaunchd(ctx, journal)
	default:
		return fmt.Errorf("unsupported upgrade recovery job kind %q", journal.RecoveryJob.Kind)
	}
}

// Disable disarms restart without stopping the current recovery actor. The
// actor still owns the flock and must remove the active journal afterward, so
// systemctl --now and launchctl bootout are deliberately forbidden by shape.
// Transaction cleanup removes the exact unit only after active.json is gone.
func (c RecoveryJobController) Disable(ctx context.Context, journal Journal) error {
	current, err := currentJournalCopy(journal)
	if err != nil {
		return err
	}
	if !terminalPhase(current.Phase) && current.Phase != PhaseRollbackFailed {
		return fmt.Errorf("cannot disable upgrade recovery job in phase %s", current.Phase)
	}

	switch current.RecoveryJob.Kind {
	case RecoveryJobDetached:
		return nil
	case RecoveryJobSystemd:
		if err := c.run(ctx, "systemctl", "--user", "disable", current.RecoveryJob.Name); err != nil {
			return fmt.Errorf("disable systemd recovery job: %w", err)
		}
		return nil
	case RecoveryJobLaunchd:
		target := launchdRecoveryDomain(c.userID()) + "/" + current.RecoveryJob.Name
		if err := c.run(ctx, "launchctl", "disable", target); err != nil {
			return fmt.Errorf("disable launchd recovery job: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported upgrade recovery job kind %q", current.RecoveryJob.Kind)
	}
}

func currentRecoveryJournal(txn *Transaction) (Journal, error) {
	if txn == nil {
		return Journal{}, errors.New("upgrade recovery job requires a transaction")
	}
	return currentJournalCopy(txn.Journal())
}

func currentJournalCopy(expected Journal) (Journal, error) {
	if err := validateRecoveryJob(expected.ID, expected.RecoveryJob); err != nil {
		return Journal{}, err
	}
	loaded, err := Load(expected.HomeDir)
	if err != nil {
		return Journal{}, err
	}
	current := loaded.Journal()
	if current.ID != expected.ID || current.RecoveryNonce != expected.RecoveryNonce {
		return Journal{}, errors.New("active upgrade transaction changed before recovery job operation")
	}
	return current, nil
}

func verifyRecoveryActorArtifact(journal Journal) error {
	info, err := os.Lstat(journal.PreviousBinaryPath)
	if err != nil {
		return fmt.Errorf("inspect preserved recovery binary: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("preserved recovery binary is not a regular file")
	}
	if _, err := readAndVerify(journal.PreviousBinaryPath, journal.PreviousBinarySHA256); err != nil {
		return fmt.Errorf("verify preserved recovery binary: %w", err)
	}
	return nil
}

func ensureExactRecoveryUnit(path string, content []byte) (bool, error) {
	for attempt := 0; attempt < 3; attempt++ {
		exists, err := inspectExactRecoveryUnit(path, content)
		if err != nil {
			return false, err
		}
		if exists {
			return false, nil
		}
		if err := publishRecoveryUnitNoReplace(path, content); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return false, fmt.Errorf("publish upgrade recovery unit: %w", err)
		}
		return true, nil
	}
	return false, errors.New("upgrade recovery unit path changed repeatedly during publication")
}

func inspectExactRecoveryUnit(path string, content []byte) (bool, error) {
	data, info, err := readRegularRecoveryUnit(path)
	if err == nil {
		if info.Mode().Perm() != recoveryUnitMode {
			return false, fmt.Errorf("existing upgrade recovery unit %s has mode %04o, want %04o",
				path, info.Mode().Perm(), os.FileMode(recoveryUnitMode))
		}
		if !bytes.Equal(data, content) {
			return false, fmt.Errorf("existing upgrade recovery unit %s does not match the transaction", path)
		}
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect upgrade recovery unit: %w", err)
	}
	return false, nil
}

func publishRecoveryUnitNoReplace(path string, content []byte) (retErr error) {
	directory := filepath.Dir(path)
	if err := validateDirectoryNoSymlink(directory); err != nil {
		return fmt.Errorf("validate recovery unit directory: %w", err)
	}
	file, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	cleaned := false
	closed := false
	defer func() {
		var closeErr error
		if !closed {
			closeErr = file.Close()
		}
		if !cleaned {
			removeErr := os.Remove(temporaryPath)
			if errors.Is(removeErr, os.ErrNotExist) {
				removeErr = nil
			}
			retErr = errors.Join(retErr, closeErr, removeErr, syncTransactionDirectory(directory))
		}
	}()
	if err := file.Chmod(recoveryUnitMode); err != nil {
		return fmt.Errorf("secure recovery unit: %w", err)
	}
	if _, err := file.Write(content); err != nil {
		return fmt.Errorf("write recovery unit: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync recovery unit: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close recovery unit: %w", err)
	}
	closed = true
	// Link publishes the fully-written inode without replacing anything that
	// won the pathname race. Unlike O_EXCL followed by writes, another process
	// can never observe a partial unit and mistake it for tampering.
	if err := os.Link(temporaryPath, path); err != nil {
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return fmt.Errorf("remove linked recovery unit staging file: %w", err)
	}
	cleaned = true
	if err := syncTransactionDirectory(directory); err != nil {
		return fmt.Errorf("sync recovery unit directory: %w", err)
	}
	return nil
}

func (c RecoveryJobController) run(ctx context.Context, name string, args ...string) error {
	if c.RunCommand != nil {
		return c.RunCommand(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).Run()
}

func (c RecoveryJobController) armAndStartLaunchd(ctx context.Context, journal Journal) error {
	domain := launchdRecoveryDomain(c.userID())
	target := domain + "/" + journal.RecoveryJob.Name
	if err := c.run(ctx, "launchctl", "enable", target); err != nil {
		return fmt.Errorf("enable launchd recovery job: %w", err)
	}
	// bootstrap closes the published-but-never-registered crash window. An
	// already registered service rejects bootstrap, in which case a non-killing
	// kickstart is the idempotent wake operation.
	bootstrapErr := c.run(ctx, "launchctl", "bootstrap", domain, journal.RecoveryJob.UnitPath)
	if bootstrapErr == nil {
		return nil
	}
	if kickstartErr := c.run(ctx, "launchctl", "kickstart", target); kickstartErr != nil {
		return errors.Join(
			fmt.Errorf("bootstrap launchd recovery job: %w", bootstrapErr),
			fmt.Errorf("kickstart registered launchd recovery job: %w", kickstartErr),
		)
	}
	return nil
}

func (c RecoveryJobController) startDetached(journal Journal) error {
	command := recoveryCommand(journal)
	if c.StartDetached != nil {
		return c.StartDetached(command[0], command[1:]...)
	}
	return startDetachedRecovery(command[0], command[1:]...)
}

func (c RecoveryJobController) userID() int {
	if c.UserID != nil {
		return c.UserID()
	}
	return os.Getuid()
}

func startDetachedRecovery(name string, args ...string) error {
	command := exec.Command(name, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close()
	command.Stdin = null
	command.Stdout = null
	command.Stderr = null
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

func recoveryCommand(journal Journal) []string {
	return []string{
		journal.PreviousBinaryPath,
		recoveryModeArgument,
		"--home", journal.HomeDir,
		"--transaction", journal.ID,
	}
}

func renderSystemdRecoveryUnit(journal Journal) string {
	command := recoveryCommand(journal)
	quoted := make([]string, len(command))
	for index, argument := range command {
		quoted[index] = systemdQuoteArgument(argument)
	}
	return fmt.Sprintf(`[Unit]
Description=Agent Factory upgrade recovery (%s)
Before=%s
StartLimitIntervalSec=300
StartLimitBurst=3

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=2
UMask=0077

[Install]
WantedBy=default.target
`, journal.ID, journal.Daemon.Owner.ServiceName, strings.Join(quoted, " "))
}

func systemdQuoteArgument(value string) string {
	var quoted strings.Builder
	quoted.WriteByte('"')
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch character {
		case '\\':
			quoted.WriteString(`\\`)
		case '"':
			quoted.WriteString(`\"`)
		case '$':
			quoted.WriteString("$$")
		case '%':
			quoted.WriteString("%%")
		case '\n':
			quoted.WriteString(`\n`)
		case '\r':
			quoted.WriteString(`\r`)
		case '\t':
			quoted.WriteString(`\t`)
		default:
			if character < 0x20 || character == 0x7f {
				fmt.Fprintf(&quoted, `\x%02x`, character)
			} else {
				quoted.WriteByte(character)
			}
		}
	}
	quoted.WriteByte('"')
	return quoted.String()
}

func renderLaunchdRecoveryPlist(journal Journal) string {
	var arguments strings.Builder
	for _, argument := range recoveryCommand(journal) {
		fmt.Fprintf(&arguments, "        <string>%s</string>\n", xmlEscape(argument))
	}
	logPath := xmlEscape(filepath.Join(transactionDir(journal.HomeDir, journal.ID), "recovery.log"))
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
%s    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ProcessType</key>
    <string>Background</string>
    <key>ThrottleInterval</key>
    <integer>2</integer>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, xmlEscape(journal.RecoveryJob.Name), arguments.String(), logPath, logPath)
}

func xmlEscape(value string) string {
	return html.EscapeString(value)
}

func launchdRecoveryDomain(uid int) string {
	return "gui/" + strconv.Itoa(uid)
}
