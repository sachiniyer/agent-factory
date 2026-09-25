package proctree

// WithheldEnvCause explains an environment read that came back empty, when the
// platform can name the reason. It returns a short cause and true only when the
// kernel served pid's argv but no environment, AND the platform positively
// attributes that to its redaction policy. Otherwise it returns false: a
// genuinely empty environment, a process that is gone, a read that failed
// outright, and a platform that cannot tell all look the same from here (#3584).
//
// This is DIAGNOSTIC ONLY. It never changes what Environ and LookupEnv report:
// a withheld environment stays EnvUnknown, and no caller may treat a cause as
// a reason to act on the process. It exists so `af doctor` can say why it could
// not attribute a process rather than silently leaving it out.
//
// It is also not the prediction Environ's doc warns against. Nothing here
// decides in advance whether a read will be served. It runs only after the
// kernel has already returned nothing, and it reads the kernel's own inputs to
// that decision (the target's code-signing flags and the SIP state) to name
// which one applied.
func WithheldEnvCause(pid int) (string, bool) {
	return withheldEnvCause(pid)
}
