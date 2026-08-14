package log

import (
	"io"
	"os"
)

// liveRotatingWriter re-reads the shared rotation settings before each write.
// It is intended for helper processes that outlive daemon config reloads and
// therefore cannot receive an in-process ReconfigureRotation call.
type liveRotatingWriter struct {
	writer *rotatingWriter
}

func (w *liveRotatingWriter) Write(p []byte) (int, error) {
	maxBytes, backups := rotationPolicy()
	w.writer.setRotation(maxBytes, backups)
	return w.writer.Write(p)
}

func (w *liveRotatingWriter) Close() error {
	return w.writer.Close()
}

// NewLiveRotatingFile opens a bounded log whose write path observes current
// log_max_size_mb and log_max_backups settings. Ordinary per-file logs should
// use NewRotatingFile when their policy is intentionally fixed at open time.
func NewLiveRotatingFile(path string, perm os.FileMode) (io.WriteCloser, error) {
	writer, err := newRotatingFile(path, perm)
	if err != nil {
		return nil, err
	}
	return &liveRotatingWriter{writer: writer}, nil
}
