package session

func archiveWarningOperation(liveness Liveness) string {
	switch liveness {
	case LiveArchived:
		return "archive"
	case LiveLost:
		// A report can be installed before registration repair succeeds. Until
		// recovery settles, neither archive nor restore is known to have completed.
		return ""
	default:
		return "restore"
	}
}
