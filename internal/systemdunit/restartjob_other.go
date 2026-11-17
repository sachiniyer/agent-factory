//go:build !linux

package systemdunit

import "time"

// RestartJobPending is false off Linux: there is no systemd job to read, so a
// stop signal is a final stop (#7474).
func RestartJobPending(string, time.Duration) bool { return false }
