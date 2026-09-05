package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

// captureHomeMessageLogs separates the two operator signals used by the TUI:
// INFO for a designed action refusal and ERROR for an operation that failed.
// The package loggers are global, so callers must not run in parallel.
func captureHomeMessageLogs(t *testing.T) (info, errors *logtest.Buffer) {
	t.Helper()
	info, errors = &logtest.Buffer{}, &logtest.Buffer{}
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
