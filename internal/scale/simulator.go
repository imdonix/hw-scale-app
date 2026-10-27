package scale

// This file is compiled into the package (not only tests) so other
// packages' tests can drive sessions against a simulated scale.

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/tamasmagyar/sscale/internal/hagrid"
)

// FakeScale simulates the Huawei Scale 3 side of the protocol for tests. It
// independently checks CRCs, the auth token and decrypts what the client
// sends with keys it derives itself.
type FakeScale struct {
	T        interface{ Errorf(string, ...any) }
	Secrets  hagrid.Secrets
	Address  string
	HUID     string
	Statuses []byte // status answers per poll (last one repeats)
	Realtime []byte // realtime record (plaintext); nil = none
	// RealtimeSeq answers successive realtime requests with these records
	// (the last one repeats); overrides Realtime.
	RealtimeSeq [][]byte
	rtIndex     int
	History     [][]byte // stored history records (plaintext)
	// RawReplies sends status and manager-info answers without WSP framing.
	RawReplies bool
	// NoStatus makes the scale ignore status polls.
	NoStatus bool
	// StatusReplies limits how many polls are answered (0 = all). The real
	// Scale 3 answers only the first.
	StatusReplies int
	// RealtimeDelay pushes the realtime record this long after the first
	// request instead of answering every request immediately (real Scale 3).
	RealtimeDelay time.Duration
	realtimeSent  bool

	mu        sync.Mutex
	notify    func(string, []byte)
	partial   map[string][][]byte
	randA     []byte
	workKey   []byte
	UserInfo  []byte
	TimeSet   []byte
	Polls     int
	Acks      int
	disc      chan struct{}
	closeOnce sync.Once
	Closed    bool
}

var _ Transport = (*FakeScale)(nil)

func (f *FakeScale) Has(uuid string) bool { _, ok := charNames[uuid]; return ok }

func (f *FakeScale) Disconnected() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.disc == nil {
		f.disc = make(chan struct{})
	}
	return f.disc
}

func (f *FakeScale) Close() {
	f.Disconnected()
	f.closeOnce.Do(func() { close(f.disc) })
	f.mu.Lock()
	f.Closed = true
	f.mu.Unlock()
}

func (f *FakeScale) Subscribe(_ context.Context, fn func(string, []byte)) error {
	f.mu.Lock()
	f.notify = fn
	f.mu.Unlock()
	return nil
}

// Write reassembles frames, checking the CRC, then handles the request.
func (f *FakeScale) Write(_ context.Context, uuid string, frame []byte) error {
	if len(frame) < 5 || (frame[0] != hagrid.FrameWritePlain && frame[0] != hagrid.FrameWriteEncrypted) {
		return fmt.Errorf("bad write prefix %x", frame)
	}
	chk := append([]byte(nil), frame...)
	chk[len(chk)-2], chk[len(chk)-1] = 0, 0
	crc := hagrid.CRC16(chk)
	if frame[len(frame)-2] != byte(crc) || frame[len(frame)-1] != byte(crc>>8) {
		return fmt.Errorf("bad write CRC on %s", nameOf(uuid))
	}
	if int(frame[1]) != len(frame)-2 {
		return fmt.Errorf("bad length byte")
	}
	total, idx := int(frame[2]>>4)+1, int(frame[2]&0x0F)
	f.mu.Lock()
	if f.partial == nil {
		f.partial = map[string][][]byte{}
	}
	if idx == 0 {
		f.partial[uuid] = nil
	}
	f.partial[uuid] = append(f.partial[uuid], frame[3:len(frame)-2])
	if len(f.partial[uuid]) < total {
		f.mu.Unlock()
		return nil
	}
	payload := bytes.Join(f.partial[uuid], nil)
	delete(f.partial, uuid)
	f.mu.Unlock()
	go f.handle(uuid, payload, frame[0] == hagrid.FrameWriteEncrypted)
	return nil
}

func (f *FakeScale) send(uuid string, payload []byte, encrypt bool) {
	var frames [][]byte
	var err error
	if encrypt {
		iv := make([]byte, 16)
		rand.Read(iv)
		f.mu.Lock()
		key := f.workKey
		f.mu.Unlock()
		frames, err = hagrid.BuildEncryptedWriteFrames(payload, key, iv)
	} else {
		frames, err = hagrid.BuildWriteFrames(payload, false)
	}
	if err != nil {
		f.T.Errorf("%v", err)
		return
	}
	f.mu.Lock()
	fn := f.notify
	f.mu.Unlock()
	for _, fr := range frames {
		n := append([]byte(nil), fr...)
		n[0] = hagrid.FrameNotifyPlain
		if encrypt {
			n[0] = hagrid.FrameNotifyEnc
		}
		crc := hagrid.CRC16Modbus(n[:len(n)-2])
		n[len(n)-2], n[len(n)-1] = byte(crc), byte(crc>>8)
		fn(uuid, n)
	}
}

// PollCount returns how many status polls were received.
func (f *FakeScale) PollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Polls
}

func (f *FakeScale) sendRaw(uuid string, payload []byte) {
	f.mu.Lock()
	fn := f.notify
	f.mu.Unlock()
	fn(uuid, append([]byte(nil), payload...))
}

func (f *FakeScale) handle(uuid string, p []byte, encrypted bool) {
	switch uuid {
	case chrRequestAuth:
		randA := make([]byte, 16)
		rand.Read(randA)
		f.mu.Lock()
		f.randA = randA
		f.mu.Unlock()
		f.send(uuid, append(randA, 0x00), false) // fw 2.0.0.70 appends a status byte
	case chrAuthToken:
		if len(p) != 48 {
			f.T.Errorf("auth token length %d", len(p))
			return
		}
		f.mu.Lock()
		randA := f.randA
		f.mu.Unlock()
		randB := p[:16]
		if !bytes.Equal(p, hagrid.AuthTokenPayload(randA, randB, f.Secrets.CAK)) {
			f.send(uuid, make([]byte, 32), false) // garbage => client must reject
			return
		}
		f.send(uuid, append(hagrid.ExpectedAuthResponse(randA, randB, f.Secrets.CAK), 0x00), false)
	case chrSendWorkKey:
		c3, _ := hagrid.C3FromAddress(f.Address)
		root, _ := hagrid.DeriveRootKey(f.Secrets.C1, f.Secrets.C2, c3)
		wk, err := hagrid.Decrypt(p, root)
		if !encrypted || err != nil || len(wk) != 16 {
			f.send(uuid, []byte{0x01}, false)
			return
		}
		f.mu.Lock()
		f.workKey = wk
		f.mu.Unlock()
		f.send(uuid, []byte{0x00}, false)
	case chrCurrentTime:
		f.mu.Lock()
		f.TimeSet = p
		f.mu.Unlock()
	case chrGetManagerInfo:
		mi := make([]byte, 70)
		copy(mi, f.HUID)
		copy(mi[30:], "device-id")
		if f.RawReplies {
			f.sendRaw(uuid, mi)
			return
		}
		f.send(uuid, mi, false)
	case chrSetUserInfo:
		f.mu.Lock()
		key := f.workKey
		f.mu.Unlock()
		ui, err := hagrid.Decrypt(p, key)
		if !encrypted || err != nil {
			f.send(uuid, []byte{0x01}, true)
			return
		}
		f.mu.Lock()
		f.UserInfo = ui
		f.mu.Unlock()
		f.send(uuid, []byte{0x00}, true)
	case chrProductInfo:
		f.send(uuid, []byte("M00D-HEM-B19"), false)
	case chrScaleVersion:
		f.send(uuid, []byte{0, 0x20, 0x01, 0x18, 'S', 'N', '1'}, false)
	case chrWeightUnit:
		f.send(uuid, []byte{0x01}, false)
	case chrStatusPoll:
		f.mu.Lock()
		st := byte(0)
		if len(f.Statuses) > 0 {
			st = f.Statuses[min(f.Polls, len(f.Statuses)-1)]
		}
		f.Polls++
		limited := f.StatusReplies > 0 && f.Polls > f.StatusReplies
		f.mu.Unlock()
		switch {
		case f.NoStatus, limited:
		case f.RawReplies:
			f.sendRaw(chrStatusResult, []byte{st})
		default:
			f.send(chrStatusResult, []byte{st}, false)
		}
	case chrRealtimeWeight:
		if len(f.RealtimeSeq) > 0 {
			f.mu.Lock()
			rec := f.RealtimeSeq[min(f.rtIndex, len(f.RealtimeSeq)-1)]
			f.rtIndex++
			f.mu.Unlock()
			f.send(uuid, rec, true)
			return
		}
		if f.Realtime == nil {
			return
		}
		if f.RealtimeDelay > 0 {
			f.mu.Lock()
			first := !f.realtimeSent
			f.realtimeSent = true
			f.mu.Unlock()
			if first {
				time.AfterFunc(f.RealtimeDelay, func() { f.send(uuid, f.Realtime, true) })
			}
			return
		}
		f.send(uuid, f.Realtime, true)
	case chrHistoryWeight:
		f.mu.Lock()
		if len(p) == 1 && p[0] == 0 {
			f.Acks++
		}
		var rec []byte
		if len(f.History) > 0 {
			rec, f.History = f.History[0], f.History[1:]
		}
		f.mu.Unlock()
		if rec == nil {
			f.send(uuid, []byte{0x00}, true)
			return
		}
		f.send(uuid, rec, true)
	}
}
