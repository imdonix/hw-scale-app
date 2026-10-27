package live

import (
	"testing"
	"time"
)

func next(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	return Event{}
}

func TestSessionFlowAndLinger(t *testing.T) {
	h := New()
	h.Linger = 50 * time.Millisecond
	ch, cancel := h.Subscribe()
	defer cancel()
	if e := next(t, ch); e.Phase != Idle {
		t.Fatalf("initial %v", e.Phase)
	}
	h.Start("connecting")
	if e := next(t, ch); e.Phase != Connecting || e.SessionAt.IsZero() || !e.Active() {
		t.Fatalf("%+v", e)
	}
	h.Update(func(e *Event) { e.Phase, e.WeightKg = Weighing, 77.5 })
	if e := next(t, ch); e.WeightKg != 77.5 {
		t.Fatalf("%+v", e)
	}
	h.Set(Done, "")
	if e := next(t, ch); e.Phase != Done || e.WeightKg != 77.5 || e.Active() {
		t.Fatalf("done should keep the session data: %+v", e)
	}
	if e := next(t, ch); e.Phase != Idle || e.WeightKg != 0 {
		t.Fatalf("should return to idle after linger: %+v", e)
	}
}

func TestNewSessionCancelsLinger(t *testing.T) {
	h := New()
	h.Linger = 50 * time.Millisecond
	h.Set(Done, "")
	h.Start("again")
	time.Sleep(100 * time.Millisecond)
	if p := h.Current().Phase; p != Connecting {
		t.Fatalf("linger reset a running session: %v", p)
	}
}

func TestNilHub(t *testing.T) {
	var h *Hub
	h.Start("x")
	h.Set(Done, "")
	if h.Current().Phase != Idle {
		t.Fatal("nil hub must be idle")
	}
	ch, cancel := h.Subscribe()
	defer cancel()
	if (<-ch).Phase != Idle {
		t.Fatal("nil hub subscription")
	}
}

func TestSlowSubscriberGetsNewest(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe()
	defer cancel()
	for i := 0; i < 50; i++ {
		h.Update(func(e *Event) { e.Phase, e.WeightKg = Weighing, float64(i) })
	}
	var last Event
	for len(ch) > 0 {
		last = <-ch
	}
	if last.WeightKg != 49 {
		t.Fatalf("newest event lost, got %v", last.WeightKg)
	}
}
