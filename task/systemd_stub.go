//go:build !linux && !darwin

package task

import "fmt"

func InstallScheduler(t Task) error {
	return fmt.Errorf("scheduled tasks are not supported on this platform (only Linux and macOS are supported)")
}

func RemoveScheduler(t Task) error {
	return fmt.Errorf("scheduled tasks are not supported on this platform (only Linux and macOS are supported)")
}
