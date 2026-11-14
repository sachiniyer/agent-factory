package session

// CarryRequiredError is a scaffold the next commit wires up (#7282).
type CarryRequiredError struct{ Reason string }

func (e *CarryRequiredError) Error() string { return e.Reason }

// Code is a scaffold.
func (e *CarryRequiredError) Code() string { return carryRefusalCode(e.Reason) }

// ValidateAccountSwapRequiringCarry is a scaffold with the pre-#7282
// behavior: a plan-time fallback still plans a fresh start.
func (i *Instance) ValidateAccountSwapRequiringCarry(name string) error {
	return i.ValidateAccountSwap(name)
}
