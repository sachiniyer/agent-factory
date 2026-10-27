package proposal

import "testing"

func TestPushSendNowRetriesAfterAFailedDelivery(t *testing.T) {
	r := newPushRig(t)
	r.mode = PushManual
	r.save("edit\n")
	r.host.delivery = DeliveryFailed
	if _, err := r.svc.SendNow(r.target()); err != nil {
		t.Fatal(err)
	}
	if n := len(r.prompts()); n != 1 {
		t.Fatalf("prompts after send now = %d, want 1", n)
	}
	r.host.delivery = DeliveryConfirmed
	r.advance(PushQuiet)
	r.expect(PushSent, 2)
}
