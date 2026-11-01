package tmux

import "testing"

// forceLaunchCapabilityOwner makes the test process the capability owner (the
// daemon's role) or not, and restores the previous value.
func forceLaunchCapabilityOwner(t *testing.T, owner bool) {
	t.Helper()
	t.Cleanup(SetLaunchCapabilityOwnerForTest(owner))
}

// observeLaunchSpecs records every launch spec minted until the test ends.
func observeLaunchSpecs(t *testing.T) *[]launchSpec {
	t.Helper()
	var specs []launchSpec
	launchSpecObserver = func(s launchSpec) { specs = append(specs, s) }
	t.Cleanup(func() { launchSpecObserver = nil })
	return &specs
}

// mainLaunchSpec is the spec of the initial pane's (single-use) capability.
func mainLaunchSpec(t *testing.T, specs []launchSpec) launchSpec {
	t.Helper()
	for _, s := range specs {
		if !s.perPane {
			return s
		}
	}
	t.Fatal("no single-use launch capability was minted")
	return launchSpec{}
}
