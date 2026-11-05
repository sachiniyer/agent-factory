package daemon

import (
	stdlog "log"

	"github.com/sachiniyer/agent-factory/log"
)

// warn returns the logger this Manager's warnings go to. Nil-safe on both the
// receiver and the field, so a Manager built by any path — including the shells
// that predate managerOptions — logs to the process-global sink exactly as
// before.
func (m *Manager) warn() *stdlog.Logger {
	if m == nil {
		return log.WarningLog
	}
	return warnLoggerOr(m.warnLog)
}

// info returns the logger this Manager's INFO lines go to (#3797), nil-safe on
// both the receiver and the field.
func (m *Manager) info() *stdlog.Logger {
	if m == nil {
		return log.InfoLog
	}
	return infoLoggerOr(m.infoLog)
}

// err returns the logger this Manager's ERROR lines go to (#3797), nil-safe on
// both the receiver and the field.
func (m *Manager) err() *stdlog.Logger {
	if m == nil {
		return log.ErrorLog
	}
	return errorLoggerOr(m.errorLog)
}

// The three resolvers below exist separately from the accessors for the callers
// that need them before a Manager exists: the root-agent snapshot is built
// inside the constructor, and its diagnostics are the ones #3787's race report
// actually names. loggerOr carries the nil rule once so the three cannot drift.
func loggerOr(logger, fallback *stdlog.Logger) *stdlog.Logger {
	if logger == nil {
		return fallback
	}
	return logger
}

func warnLoggerOr(logger *stdlog.Logger) *stdlog.Logger {
	return loggerOr(logger, log.WarningLog)
}

func infoLoggerOr(logger *stdlog.Logger) *stdlog.Logger {
	return loggerOr(logger, log.InfoLog)
}

func errorLoggerOr(logger *stdlog.Logger) *stdlog.Logger {
	return loggerOr(logger, log.ErrorLog)
}
