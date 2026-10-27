// Package live broadcasts what the daemon is doing with the scale right now,
// for the web app's live activity card.
package live

import (
	"sync"
	"time"
)

// Phase of a scale session.
type Phase string

const (
	Idle       Phase = "idle"       // no session
	Connecting Phase = "connecting" // scale woke up, connecting + authenticating
	Waiting    Phase = "waiting"    // connected, waiting for a weigh-in
	Weighing   Phase = "weighing"   // live weight received
	Analyzing  Phase = "analyzing"  // weight known, impedance still being measured
	Saved      Phase = "saved"      // reading stored (session still open)
	Done       Phase = "done"       // disconnected
	Failed     Phase = "failed"     // session failed
)

// Event is the current live state. Measurement is the stored reading (an
// app.View) once there is one.
type Event struct {
	Seq         int64     `json:"seq"`
	Phase       Phase     `json:"phase"`
	Message     string    `json:"message,omitempty"`
	WeightKg    float64   `json:"weight_kg,omitempty"`
	Measurement any       `json:"measurement,omitempty"`
	Error       string    `json:"error,omitempty"`
	SessionAt   time.Time `json:"session_started_at,omitzero"`
	Updated     time.Time `json:"updated_at"`
}

// Active reports whether a session is in progress.
func (e Event) Active() bool { return e.Phase != Idle && e.Phase != Done && e.Phase != Failed }

// Hub holds the current state and fans updates out to subscribers. A nil
// *Hub is valid and ignores everything.
type Hub struct {
	mu    sync.Mutex
	cur   Event
	subs  map[chan Event]struct{}
	reset *time.Timer
	// Linger is how long Done/Failed stays visible before going Idle.
	Linger time.Duration
}

// New returns an idle hub.
func New() *Hub {
	return &Hub{cur: Event{Phase: Idle, Updated: time.Now()}, subs: map[chan Event]struct{}{}, Linger: 20 * time.Second}
}

// Current returns the current state.
func (h *Hub) Current() Event {
	if h == nil {
		return Event{Phase: Idle}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur
}

// Update applies fn to the state and broadcasts the result.
func (h *Hub) Update(fn func(*Event)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(&h.cur)
	h.cur.Seq++
	h.cur.Updated = time.Now()
	if h.reset != nil {
		h.reset.Stop()
		h.reset = nil
	}
	if h.cur.Phase == Done || h.cur.Phase == Failed {
		seq := h.cur.Seq
		h.reset = time.AfterFunc(h.Linger, func() {
			h.mu.Lock()
			still := h.cur.Seq == seq
			h.mu.Unlock()
			if still {
				h.Update(func(e *Event) { *e = Event{Phase: Idle} })
			}
		})
	}
	ev := h.cur
	for ch := range h.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop the stale event, keep the newest
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

// Start begins a new session.
func (h *Hub) Start(msg string) {
	h.Update(func(e *Event) {
		*e = Event{Phase: Connecting, Message: msg, SessionAt: time.Now()}
	})
}

// Set changes phase and message, keeping the session data.
func (h *Hub) Set(p Phase, msg string) {
	h.Update(func(e *Event) { e.Phase, e.Message = p, msg })
}

// Subscribe returns a channel receiving every update, starting with the
// current state, and a cancel func.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 8)
	if h == nil {
		ch <- Event{Phase: Idle}
		return ch, func() {}
	}
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	ch <- h.cur
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}
