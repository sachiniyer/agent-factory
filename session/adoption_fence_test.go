package session

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
)

// agentServerMethodWritesToThePTY classifies EVERY method on the agent-server
// contract — AgentServer plus its two optional extensions — as one that puts
// bytes in front of the agent or one that does not.
//
// It is a closed classification on purpose, and the test below fails on a method
// that is missing from it. #2953's fourth P1 was exactly this failure: it marked
// adoption at Manager.SendPrompt and assumed that was THE delivery path, while
// browser-terminal input reached the agent through InputTab and moved nothing.
// Being wrong about the set was silent. Here it is not: adding a method to the
// interface without answering "does this write to the PTY?" breaks the build's
// tests, and answering "yes" without bumping the count breaks them too.
var agentServerMethodWritesToThePTY = map[string]bool{
	// Writes. Each of these is a way a user (or an automation acting for one)
	// makes a finished task session theirs.
	"SendPrompt":           true,
	"SendPromptWithStatus": true,
	"Input":                true,
	"InputTab":             true,

	// Not writes. Reads, lifecycle, and the size negotiation.
	//
	// Resize/ResizeTab are the interesting "no": a browser resizing its terminal
	// is not the user doing work, and counting it would let a window drag cancel a
	// declared teardown. Snapshot is the other one worth naming — it dismisses a
	// pending trust prompt as a side effect, but through the BACKEND rather than
	// through these entry points, so the poll's own ticks never move the count.
	"Provision":    false,
	"Launch":       false,
	"Expose":       false,
	"Snapshot":     false,
	"Preview":      false,
	"PreviewByID":  false,
	"Alive":        false,
	"Subscribe":    false,
	"SubscribeTab": false,
	"Resize":       false,
	"ResizeTab":    false,
	"Kill":         false,
	"Archive":      false,
}

// agentServerContract is the set of interfaces a runtime may satisfy. Enumerated
// here so the test walks the CONTRACT rather than a hand-written list of names.
func agentServerContract() []reflect.Type {
	return []reflect.Type{
		reflect.TypeOf((*AgentServer)(nil)).Elem(),
		reflect.TypeOf((*TabAddressableServer)(nil)).Elem(),
		reflect.TypeOf((*PromptDeliveryReporter)(nil)).Elem(),
	}
}

// agentServerMethodNames returns every method name on the contract, deduped.
func agentServerMethodNames() []string {
	seen := map[string]struct{}{}
	for _, iface := range agentServerContract() {
		for i := 0; i < iface.NumMethod(); i++ {
			seen[iface.Method(i).Name] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestAgentServerContractIsFullyClassified is the fail-closed half: no method on
// the agent-server contract may go unanswered, and no stale name may linger in
// the map after the interface drops it.
func TestAgentServerContractIsFullyClassified(t *testing.T) {
	names := agentServerMethodNames()
	for _, name := range names {
		if _, classified := agentServerMethodWritesToThePTY[name]; !classified {
			t.Errorf("agent-server method %q is not classified: say whether it writes to the PTY. "+
				"If it does, it must call Instance.NoteAdoptionDelivery first (#3865)", name)
		}
	}
	onContract := map[string]struct{}{}
	for _, name := range names {
		onContract[name] = struct{}{}
	}
	for name := range agentServerMethodWritesToThePTY {
		if _, ok := onContract[name]; !ok {
			t.Errorf("agentServerMethodWritesToThePTY classifies %q, which is no longer on the agent-server contract", name)
		}
	}
}

// zeroArgsFor builds a call-ready argument list of zero values for m. Every
// write path takes the adoption mark BEFORE it looks at its arguments, so zero
// values reach the bump; what happens after (an unknown tab id, an empty prompt)
// is irrelevant to the property under test and is asserted separately.
func zeroArgsFor(m reflect.Value) []reflect.Value {
	t := m.Type()
	args := make([]reflect.Value, t.NumIn())
	for i := range args {
		args[i] = reflect.Zero(t.In(i))
	}
	return args
}

// adoptionRuntime is one production agent-server implementation under test,
// paired with the Instance it drives.
type adoptionRuntime struct {
	name string
	// idNative reports whether this runtime implements TabAddressableServer. An
	// ordinal-shaped wire protocol does not (see the interface's own doc), so its
	// missing InputTab is a fact about the runtime, not a gap in the fix.
	idNative bool
	build    func(t *testing.T) (*Instance, AgentServer)
}

// productionAgentServerRuntimes is every runtime a daemon-held session can
// actually be driven through.
//
// deadRemoteAgentServer is deliberately absent, and the reason is not
// convenience: every one of its methods returns an error before a byte moves, it
// holds no Instance to count against, and it exists precisely to report that the
// sandbox is gone. There is nothing there for a delivery to adopt.
func productionAgentServerRuntimes() []adoptionRuntime {
	return []adoptionRuntime{
		{
			name:     "local",
			idNative: true,
			build: func(t *testing.T) (*Instance, AgentServer) {
				inst, _ := newProbeInstance(t)
				return inst, inst.AgentServer()
			},
		},
		{
			name:     "remote",
			idNative: false,
			build: func(t *testing.T) (*Instance, AgentServer) {
				// Built the way agentServerLocked builds it (agentserver_local.go), against
				// a server that answers the send-prompt route. The data-plane calls fail —
				// there is no WS behind this URL — which is exactly the case that must still
				// count: the mark is taken before the write.
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data":  agentSendPromptResp{OK: true, Status: PromptDelivered},
						"error": nil,
					})
				}))
				t.Cleanup(srv.Close)
				rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: srv.URL, Token: "test-token"}, "remote")
				if err != nil {
					t.Fatalf("newRemoteAgentClient: %v", err)
				}
				inst, _ := newProbeInstance(t)
				return inst, &remoteAgentServer{rc: rc, inst: inst}
			},
		},
	}
}

// writeMethodOn resolves one classified write path on a runtime, reporting
// whether that runtime implements it at all.
func writeMethodOn(t *testing.T, rt adoptionRuntime, as AgentServer, name string) (reflect.Value, bool) {
	t.Helper()
	m := reflect.ValueOf(as).MethodByName(name)
	if m.IsValid() {
		return m, true
	}
	// The only legitimate absence: the id-native plane on an ordinal runtime.
	if rt.idNative || !isTabAddressableMethod(name) {
		t.Fatalf("runtime %q does not implement the write path %q", rt.name, name)
	}
	return reflect.Value{}, false
}

func isTabAddressableMethod(name string) bool {
	iface := reflect.TypeOf((*TabAddressableServer)(nil)).Elem()
	for i := 0; i < iface.NumMethod(); i++ {
		if iface.Method(i).Name == name {
			return true
		}
	}
	return false
}

// TestEveryAgentServerWritePathIsAdoption is the property this whole change
// rests on: the set of ways to adopt a session is the set of agent-server entry
// points that write to its PTY, and every one of them counts the delivery — on
// every runtime a daemon-held session can be driven through.
//
// It is deliberately driven by reflection over the interface rather than by
// hand-written calls. A hand-written list is exactly what was wrong before — it
// agrees with the code on the day it is written and then silently stops being
// the whole set.
func TestEveryAgentServerWritePathIsAdoption(t *testing.T) {
	for _, rt := range productionAgentServerRuntimes() {
		for _, name := range agentServerMethodNames() {
			if !agentServerMethodWritesToThePTY[name] {
				continue
			}
			t.Run(rt.name+"/"+name, func(t *testing.T) {
				inst, as := rt.build(t)
				method, implemented := writeMethodOn(t, rt, as, name)
				if !implemented {
					t.Skipf("%s has no id-native data plane; ws_pty.go never binds it", rt.name)
				}
				before := inst.AdoptionDeliveries()
				method.Call(zeroArgsFor(method))
				if after := inst.AdoptionDeliveries(); after != before+1 {
					t.Fatalf("%s must count exactly one delivery: %d → %d", name, before, after)
				}
			})
		}
	}
}

// TestAgentServerWritePathsRefuseAFencedSession is the other half of the fence:
// once a teardown has claimed the session, a write is REFUSED rather than landing
// on a pane that is about to stop existing — and it does not move the count
// either, so the teardown's comparison is not disturbed by what it refused.
func TestAgentServerWritePathsRefuseAFencedSession(t *testing.T) {
	for _, rt := range productionAgentServerRuntimes() {
		for _, name := range agentServerMethodNames() {
			if !agentServerMethodWritesToThePTY[name] {
				continue
			}
			t.Run(rt.name+"/"+name, func(t *testing.T) {
				inst, as := rt.build(t)
				method, implemented := writeMethodOn(t, rt, as, name)
				if !implemented {
					t.Skipf("%s has no id-native data plane; ws_pty.go never binds it", rt.name)
				}
				inst.CloseAdoptionFence()

				before := inst.AdoptionDeliveries()
				out := method.Call(zeroArgsFor(method))
				err, _ := out[len(out)-1].Interface().(error)
				if !errors.Is(err, ErrAdoptionFenced) {
					t.Fatalf("%s on a fenced session = %v, want ErrAdoptionFenced", name, err)
				}
				if after := inst.AdoptionDeliveries(); after != before {
					t.Fatalf("a refused %s must not count: %d → %d", name, before, after)
				}

				inst.ReopenAdoptionFence()
				method.Call(zeroArgsFor(method))
				if after := inst.AdoptionDeliveries(); after != before+1 {
					t.Fatalf("%s must be readmitted once the fence reopens: %d → %d", name, before, after)
				}
			})
		}
	}
}

// TestAFencedWriteNeverReachesTheRuntime is the half the count cannot show: a
// refusal must stop BEFORE the byte moves, not merely decline to record it.
func TestAFencedWriteNeverReachesTheRuntime(t *testing.T) {
	inst, backend := newProbeInstance(t)
	as := inst.AgentServer()
	inst.CloseAdoptionFence()

	if err := as.SendPrompt("this must not land"); !errors.Is(err, ErrAdoptionFenced) {
		t.Fatalf("SendPrompt on a fenced session = %v, want ErrAdoptionFenced", err)
	}
	if backend.sentPrompt != "" {
		t.Fatalf("a fenced SendPrompt reached the backend anyway (sent %q)", backend.sentPrompt)
	}

	inst.ReopenAdoptionFence()
	if err := as.SendPrompt("this one may"); err != nil {
		t.Fatalf("SendPrompt after reopen: %v", err)
	}
	if backend.sentPrompt != "this one may" {
		t.Fatalf("backend saw %q, want the prompt sent after the fence reopened", backend.sentPrompt)
	}
}

// TestAdoptionBaselineIsCapturedAtTheCompletionTransition pins #2953's third P1.
// The baseline must be pinned by the transition that ends the run, not read
// afterwards: the poll does more work (persistPollChange, storage I/O) between
// the run ending and the teardown being dispatched, and a delivery landing in
// that gap must read as LATER than the baseline, never as part of it.
func TestAdoptionBaselineIsCapturedAtTheCompletionTransition(t *testing.T) {
	inst, _ := newProbeInstance(t)
	inst.taskRunActive = true
	inst.SetStatusForTest(Running)

	// Two deliveries during the run itself: they belong to the task, so the
	// baseline must include them.
	if err := inst.AgentServer().SendPrompt("run"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if err := inst.AgentServer().SendPrompt("run again"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}

	if err := inst.Transition(ObserveLiveness(LiveReady)); err != nil {
		t.Fatalf("completion transition: %v", err)
	}
	if inst.TaskRunActive() {
		t.Fatal("precondition: the idle edge must end the run")
	}
	if got, want := inst.AdoptionDeliveriesAtRunEnd(), inst.AdoptionDeliveries(); got != want {
		t.Fatalf("baseline %d, live count %d: the capture must happen at the transition", got, want)
	}
	baseline := inst.AdoptionDeliveriesAtRunEnd()

	// Anything after the transition — including everything the poll still does on
	// that same tick — is the user's.
	if err := inst.AgentServer().SendPrompt("mine now"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if inst.AdoptionDeliveries() == baseline {
		t.Fatal("a delivery after the completion transition must exceed the baseline")
	}
	if inst.AdoptionDeliveriesAtRunEnd() != baseline {
		t.Fatal("the baseline must not move after the run has ended")
	}

	// And the user's turn STARTS AND SETTLES — the exact shape that defeated every
	// level check (#2953's first P1). Both edges move the lifecycle state and the
	// second one is completion-shaped, so if the baseline were pinned on the
	// assignment rather than on the run marker's edge it would be re-pinned here
	// and the delivery above would vanish into it.
	if err := inst.Transition(ObserveLiveness(LiveRunning)); err != nil {
		t.Fatalf("the user's turn starts: %v", err)
	}
	if err := inst.Transition(ObserveLiveness(LiveReady)); err != nil {
		t.Fatalf("the user's turn settles: %v", err)
	}
	if inst.GetLiveness() != LiveReady || inst.TaskRunActive() {
		t.Fatal("precondition: the session reads exactly as it did at completion")
	}
	if inst.AdoptionDeliveriesAtRunEnd() != baseline {
		t.Fatal("a settled turn must not re-pin the baseline; the delivery must stay visible")
	}
	if inst.AdoptionDeliveries() == inst.AdoptionDeliveriesAtRunEnd() {
		t.Fatal("the teardown must still see the adoption after the turn settles")
	}
}

// TestAdoptionBaselineIsCapturedWhenStartupSettlesUnknown covers the OTHER edge
// that clears the run marker. MarkStartupStateUnknown does it directly rather
// than through a transition, so the capture has to be there too — a baseline of
// zero on a session that had already been delivered to would make every later
// comparison unequal, and the guard would never let a legitimate reap through.
func TestAdoptionBaselineIsCapturedWhenStartupSettlesUnknown(t *testing.T) {
	inst, _ := newProbeInstance(t)
	inst.taskRunActive = true
	if err := inst.AgentServer().SendPrompt("the create's own prompt"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}

	inst.MarkStartupStateUnknown()
	if inst.TaskRunActive() {
		t.Fatal("precondition: startup-unknown clears the run marker")
	}
	if got, want := inst.AdoptionDeliveriesAtRunEnd(), inst.AdoptionDeliveries(); got != want {
		t.Fatalf("baseline %d, live count %d: startup-unknown must pin the baseline too", got, want)
	}
	baseline := inst.AdoptionDeliveriesAtRunEnd()

	// MarkStartupStateUnknown clears the marker UNCONDITIONALLY, so the capture has
	// to sit on the edge rather than beside the assignment. A second call after a
	// delivery must not fold that delivery into the baseline — that would hand a
	// teardown a comparison saying nothing had happened.
	if err := inst.AgentServer().SendPrompt("the user's, not the create's"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	inst.MarkStartupStateUnknown()
	if inst.AdoptionDeliveriesAtRunEnd() != baseline {
		t.Fatalf("the baseline moved to %d on a second call; it must be pinned on the run marker's edge only",
			inst.AdoptionDeliveriesAtRunEnd())
	}
}

// TestCloseAdoptionFenceReadsAndShutsTogether is the serialization property
// stated directly: the count a teardown acts on is read in the SAME critical
// section that shuts the fence, so there is no ordering in which a delivery both
// misses the count and reaches the pane.
func TestCloseAdoptionFenceReadsAndShutsTogether(t *testing.T) {
	inst, _ := newProbeInstance(t)
	if err := inst.NoteAdoptionDelivery(); err != nil {
		t.Fatalf("NoteAdoptionDelivery: %v", err)
	}

	got := inst.CloseAdoptionFence()
	if want := inst.AdoptionDeliveries(); got != want {
		t.Fatalf("CloseAdoptionFence returned %d, live count %d", got, want)
	}
	if err := inst.NoteAdoptionDelivery(); !errors.Is(err, ErrAdoptionFenced) {
		t.Fatalf("a delivery after the fence shut = %v, want ErrAdoptionFenced", err)
	}
	if inst.AdoptionDeliveries() != got {
		t.Fatal("a refused delivery must not change the count the teardown already read")
	}
}
