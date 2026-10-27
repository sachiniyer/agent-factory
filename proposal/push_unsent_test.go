package proposal

import (
	"testing"
	"time"
)

func TestPushPullClearsAQueuedSendNow(t *testing.T) {
	r := newPushRig(t)
	r.mode = PushManual
	r.save("edit\n")
	r.ready = false
	if _, err := r.svc.SendNow(r.target()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Changes(sessA); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Second)
	if _, err := r.svc.Write(byRepo(), "p", "goals", "someone else\n", AuthorExternal); err != nil {
		t.Fatal(err)
	}
	r.ready = true
	r.advance(time.Second)
	r.expect(PushIdle, 0)
}

func TestPushKeepsTheUnsentCountWhenItCannotBeRead(t *testing.T) {
	r := newPushRig(t)
	r.save("edit\n")
	r.expect(PushCountdown, 0)
	store, err := Open(testRepoID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.git(nil, "update-ref", "-d", cursorRefPrefix+sessA); err != nil {
		t.Fatal(err)
	}
	r.svc.markUnsentStale("", "", sessA)
	r.advance(time.Second)
	if st := r.state(); st.Unsent != 1 || st.State != PushCountdown {
		t.Fatalf("a failed count must keep the last one, got %+v", st)
	}
}
