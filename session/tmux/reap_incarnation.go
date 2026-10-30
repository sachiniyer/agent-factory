package tmux

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/cmd"
)

// IncarnationProof is the durable, pre-spawn evidence a caller holds about a
// session it believes it created and wants to stop (#6258). Every field must be
// set: an empty one proves nothing, so the session is left alone.
type IncarnationProof struct {
	// SessionName is the sanitized tmux session name (AF_SESSION).
	SessionName string
	// Home is the agent-factory home that owns the session (AF_HOME).
	Home string
	// InstanceID is the instance id of the create attempt, recorded before the
	// spawn and stamped into the session at new-session (AF_INSTANCE_ID).
	InstanceID string
}

// IncarnationReap is what ReapProvenIncarnation established.
type IncarnationReap struct {
	// Stopped: the proven incarnation was killed, or its id was already gone by
	// the time the kill ran. Nothing of it remains to write into a workspace.
	Stopped bool
	// Why, when !Stopped, names the condition that kept the session alone:
	// unproven, replaced, unreadable, or unanswered. Never empty then.
	Why string
}

// ReapProvenIncarnation stops the tmux session answering at proof.SessionName
// ONLY if that exact incarnation proves it was spawned by the attempt proof
// names, and kills it by its tmux session id.
//
// A name and a clock cannot prove creation: any af session in this install can
// carry the same name, and a later creation time is exactly what a concurrent
// create has. The proof here is a value the attempt recorded durably BEFORE it
// spawned, which only that attempt's launch options carry into new-session:
//
//  1. Resolve the incarnation once: its "$N" id, server pid and creation stamp.
//  2. Read AF_INSTANCE_ID, AF_HOME and AF_SESSION from THAT id's environment,
//     not from the name — the name can be re-created between two commands.
//  3. Re-check the id still resolves to the same server and creation stamp, so a
//     replacement server that reissued "$N" is not mistaken for it.
//  4. kill-session -t $N, through the ordinary closeRun. tmux session ids are not
//     reused within a server's lifetime, so a session re-created under the same
//     name after step 1 has a different id and the kill finds nothing.
//
// Every uncertainty leaves the session alone: no marker (a session from before
// the marker existed, or tmux < 3.2 which cannot stamp one), an unreadable
// environment, a probe that did not answer, a mismatch, a replaced incarnation.
// "Unknown" is never "ours".
func ReapProvenIncarnation(cmdExec cmd.Executor, program string, proof IncarnationProof) IncarnationReap {
	if proof.SessionName == "" || proof.Home == "" || proof.InstanceID == "" {
		return IncarnationReap{Why: "the fire recorded no instance id before its spawn, so no session can be proven to be its own"}
	}
	t := NewTmuxSessionFromSanitizedName(proof.SessionName, program)
	t.cmdExec = cmdExec

	gen, answered := t.confirmedGeneration()
	if !answered {
		return IncarnationReap{Why: fmt.Sprintf("tmux did not answer while resolving session %q", proof.SessionName)}
	}
	if gen == nil {
		// Resolved to nothing provable: the session vanished after it was listed,
		// or tmux answered with a failure that is not "absent". Either way there is
		// no incarnation to prove, and nothing may be killed by name instead.
		return IncarnationReap{Why: fmt.Sprintf("session %q did not resolve to a tmux session id", proof.SessionName)}
	}

	for _, want := range []struct{ marker, value string }{
		{EnvMarkerInstance, proof.InstanceID},
		{EnvMarkerHome, proof.Home},
		{EnvMarkerSession, proof.SessionName},
	} {
		got, present, err := sessionEnvMarkerAt(cmdExec, gen.sessionID, proof.SessionName, want.marker)
		switch {
		case err != nil:
			return IncarnationReap{Why: fmt.Sprintf("could not read %s of session %q: %v", want.marker, proof.SessionName, err)}
		case !present:
			return IncarnationReap{Why: fmt.Sprintf("session %q carries no %s, so it cannot be proven to be this fire's", proof.SessionName, want.marker)}
		case got != want.value:
			return IncarnationReap{Why: fmt.Sprintf("session %q carries a different %s, so it was not created by this fire", proof.SessionName, want.marker)}
		}
	}

	same, known, err := t.generationMatches(gen)
	if err != nil || !known {
		return IncarnationReap{Why: fmt.Sprintf("could not re-confirm session %q before stopping it: %v", proof.SessionName, err)}
	}
	if !same {
		return IncarnationReap{Why: fmt.Sprintf("session %q was replaced after it was probed", proof.SessionName)}
	}

	if _, killErr, _ := t.closeTarget(false, gen.sessionID); killErr != nil {
		return IncarnationReap{Why: fmt.Sprintf("stopping session %q failed: %s", proof.SessionName, killErr)}
	}
	return IncarnationReap{Stopped: true}
}
