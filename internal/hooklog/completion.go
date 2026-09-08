package hooklog

import (
	"errors"
	"os"
	"time"
)

type keptLog struct {
	os.FileInfo
	completed time.Time
}

// A new private marker gets its timestamp from creation, without futimes.
// O_EXCL prevents following an unexpected link or overwriting an existing file.
func writeCompletionMarker(file *os.File) error {
	path := file.Name()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return err
	}
	// Never give replacement output this run's completion time, including
	// a symlink back to the opened inode: retention only owns regular paths.
	if !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return nil
	}

	marker, err := os.OpenFile(path+".done", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := marker.WriteString("done\n")
	return errors.Join(writeErr, marker.Close())
}

func retentionTime(path string, info os.FileInfo) (time.Time, error) {
	completed := info.ModTime()
	marker, err := os.Lstat(path + ".done")
	if os.IsNotExist(err) {
		return completed, nil
	}
	if err != nil {
		return completed, err
	}
	if marker.Mode().IsRegular() && marker.ModTime().After(completed) {
		completed = marker.ModTime()
	}
	return completed, nil
}

// Remove deletes completed output and its optional completion marker.
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	err := os.Remove(path + ".done")
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
