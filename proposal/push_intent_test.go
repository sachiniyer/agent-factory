package proposal

import (
	"testing"
	"time"
)

func TestPushKeepsAHoldPlacedDuringDelivery(t *testing.T) {
	r := newPushRig(t)
	r.save("one\n")
	r.host.onSend = func() {
		r.save("two\n")
		if _, err := r.svc.HoldPush(r.target()); err != nil {
			t.Fatal(err)
		}
	}
	r.advance(PushQuiet)
	r.expect(PushHeld, 1)
	r.advance(time.Minute)
	r.expect(PushHeld, 1)
}

func TestPushDropsASaveMadeWithoutADocsTab(t *testing.T) {
	r := newPushRig(t)
	r.noTab = true
	r.tick()
	r.save("edit\n")
	r.advance(time.Hour)
	r.noTab = false
	r.tick()
	if n := len(r.prompts()); n != 0 {
		t.Fatalf("a save made while the session had no Docs tab was pushed when one opened (%d prompts)", n)
	}
}
