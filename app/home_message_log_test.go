package app

import (
	"bytes"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
)

// captureHomeMessageLogs separates the two operator signals used by the TUI:
// INFO for a designed action refusal and ERROR for an operation that failed.
// The package loggers are global, so callers must not run in parallel.
func captureHomeMessageLogs(t *testing.T) (info, errors *bytes.Buffer) {
	t.Helper()
	info, errors = &bytes.Buffer{}, &bytes.Buffer{}
	previousInfo := log.InfoLog.Writer()
	previousErrors := log.ErrorLog.Writer()
	log.InfoLog.SetOutput(info)
	log.ErrorLog.SetOutput(errors)
	t.Cleanup(func() {
		log.InfoLog.SetOutput(previousInfo)
		log.ErrorLog.SetOutput(previousErrors)
	})
	return info, errors
}
