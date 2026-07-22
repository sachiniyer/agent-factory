package upgradetxn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func recoveryLockPath(home, id string) string {
	return filepath.Join(upgradeRoot(home), ".recovery-"+id+".lock")
}

func validateRecoveryLockStorage(home string, journal Journal) error {
	path := recoveryLockPath(home, journal.ID)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect upgrade recovery lock: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != journalFileMode {
		return errors.New("upgrade recovery lock is not a private regular file")
	}
	identity, err := fileIdentity(info)
	if err != nil {
		return fmt.Errorf("identify upgrade recovery lock: %w", err)
	}
	if identity != journal.RecoveryLockIdentity {
		return errors.New("upgrade recovery lock identity does not match the journal")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read upgrade recovery lock: %w", err)
	}
	if strings.TrimSpace(string(data)) != journal.RecoveryNonce {
		return errors.New("upgrade recovery lock nonce does not match the journal")
	}
	return nil
}

func fileIdentity(info os.FileInfo) (FileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}, errors.New("file has no device/inode identity")
	}
	identity := FileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
	if identity.Inode == 0 {
		return FileIdentity{}, errors.New("file has an invalid inode identity")
	}
	return identity, nil
}

func acquireFileLock(path string, nonblocking bool) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, journalFileMode)
	if err != nil {
		return nil, err
	}
	operation := syscall.LOCK_EX
	if nonblocking {
		operation |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), operation); err != nil {
		_ = file.Close()
		if nonblocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return nil, ErrRecoveryActive
		}
		return nil, err
	}
	return file, nil
}

func acquireRecoveryLock(
	path string, expected FileIdentity, nonblocking bool,
) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR, journalFileMode)
	if err != nil {
		return nil, err
	}
	if err := validateRecoveryLockHandle(file, path, expected); err != nil {
		_ = file.Close()
		return nil, err
	}
	operation := syscall.LOCK_EX
	if nonblocking {
		operation |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), operation); err != nil {
		_ = file.Close()
		if nonblocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return nil, ErrRecoveryActive
		}
		return nil, err
	}
	if err := validateRecoveryLockHandle(file, path, expected); err != nil {
		_ = releaseFileLock(file)
		return nil, err
	}
	return file, nil
}

func validateRecoveryLockHandle(file *os.File, path string, expected FileIdentity) error {
	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened recovery lock: %w", err)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect recovery lock path: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !openedInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() {
		return errors.New("upgrade recovery lock path is not a regular file")
	}
	openedIdentity, err := fileIdentity(openedInfo)
	if err != nil {
		return err
	}
	pathIdentity, err := fileIdentity(pathInfo)
	if err != nil {
		return err
	}
	if openedIdentity != expected || pathIdentity != expected || !os.SameFile(openedInfo, pathInfo) {
		return errors.New("upgrade recovery lock path was replaced")
	}
	return nil
}

func releaseFileLock(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}
