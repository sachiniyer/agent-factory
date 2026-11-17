//go:build !linux

package systemdunit

import "context"

// RestartJobPending is false off Linux: there is no systemd job to read, so a
// stop signal is a final stop (#7474).
func RestartJobPending(context.Context, string, int) bool { return false }
