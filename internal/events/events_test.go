package events

import "testing"

func TestHub(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe()
	h.Publish(Jobs)
	if got := <-ch; got != Jobs {
		t.Fatalf("got %q", got)
	}
	// A full subscriber does not block publishers.
	for range 100 {
		h.Publish(Scan)
	}
	cancel()
	h.Publish(Jobs)
}
