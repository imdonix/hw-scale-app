// Package scale drives one BLE session with a Huawei Scale 3: connect,
// authenticate, sync time and profile, capture the measurement and read the
// stored history.
package scale

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tamasmagyar/sscale/internal/bluez"
	"github.com/tamasmagyar/sscale/internal/hagrid"
)

// GATT UUIDs.
const (
	chrRequestAuth      = "02b2a08e-f8b0-4047-b1fd-f4e0efeee679"
	chrAuthToken        = "32330a04-15d9-421a-91c5-2a2d5c7525c9"
	chrSendWorkKey      = "a3d330f8-b84f-4f48-a78c-f8d1e33b597a"
	chrBindRequest      = "42596cbe-d291-4da3-8ca6-d1ae5d1c9174"
	chrSetUserInfo      = "8cc61d7d-66c0-4802-89c3-38c5a163592e"
	chrGetManagerInfo   = "4338c65e-ed8e-4085-bbea-a25e33ca6b54"
	chrCurrentTime      = "00002a2b-0000-1000-8000-00805f9b34fb"
	chrProductInfo      = "75143e79-f878-4a00-a628-edc40509de7e"
	chrScaleVersion     = "1f5d3d5c-496d-4290-af03-c7a8d5419741"
	chrWeightUnit       = "7e6dbc73-42e7-45b9-a6ec-6aa2d7834695"
	chrStatusPoll       = "bfc36f6e-4150-4a4b-9052-3d359e52962e"
	chrStatusResult     = "ba216311-1787-472b-bef6-3eb29e62293e"
	chrWeightMeasure    = "00002a9d-0000-1000-8000-00805f9b34fb"
	chrBodyComposition  = "00002a9c-0000-1000-8000-00805f9b34fb"
	chrRealtimeWeight   = "46797c17-d639-488d-9476-4789e8472878"
	chrHistoryWeight    = "0212f42a-5f19-4bc1-ba52-d7ec7ccb71a4"
	inboxStatus         = "status"
	inboxRealtime       = "realtime"
	maxStatusPolls      = 30
	maxHistoryRecords   = 64
	defaultReplyTimeout = 5 * time.Second
)

var charNames = map[string]string{
	chrRequestAuth: "request-auth", chrAuthToken: "auth-token", chrSendWorkKey: "send-work-key",
	chrBindRequest: "bind-request", chrSetUserInfo: "set-user-info", chrGetManagerInfo: "manager-info",
	chrCurrentTime: "current-time", chrProductInfo: "product-info", chrScaleVersion: "scale-version",
	chrWeightUnit: "weight-unit", chrStatusPoll: "status-poll", chrStatusResult: "status-result",
	chrWeightMeasure: "weight-measurement", chrBodyComposition: "body-composition",
	chrRealtimeWeight: "realtime-weight", chrHistoryWeight: "history-weight",
}

func inboxFor(uuid string) string {
	switch uuid {
	case chrStatusPoll, chrStatusResult:
		return inboxStatus
	case chrRealtimeWeight, chrBindRequest, chrBodyComposition, chrWeightMeasure:
		return inboxRealtime
	}
	return uuid
}

// Timing knobs (variables so tests can shorten them).
var (
	statusPollInterval = 3 * time.Second
	finalRecordWait    = 4 * time.Second
	// settleWait: after a complete record (with impedance), wait this long
	// for a refined one.
	settleWait = 3 * time.Second
	// A weight-only record is re-requested every refetchInterval for up to
	// incompleteWait, until the scale has measured the impedance.
	refetchInterval = 2 * time.Second
	incompleteWait  = 15 * time.Second
)

// SetTiming scales all session timings for tests: poll interval, final
// record wait and settle time (refetch = poll, incomplete wait = 3*settle).
func SetTiming(poll, finalWait, settle time.Duration) {
	statusPollInterval, finalRecordWait, settleWait = poll, finalWait, settle
	refetchInterval, incompleteWait = poll, 3*settle
}

// Errors.
var (
	ErrDisconnected = errors.New("scale disconnected")
	ErrAuthFailed   = errors.New("authentication failed (check cak/c1/c2 and address in keys.json)")
)

// Options configures a session.
type Options struct {
	Secrets hagrid.Secrets
	Address string
	Log     *slog.Logger
	// Trace logs every raw frame (hex). Never logs key material.
	Trace bool
}

type packet struct {
	data []byte
	at   time.Time
}

// Session is a connected, optionally authenticated scale.
type Session struct {
	opt     Options
	log     *slog.Logger
	t       Transport
	inbox   map[string]chan packet
	writeMu sync.Mutex

	mu      sync.Mutex
	workKey []byte
	accum   map[string]*hagrid.Accumulator
}

// Open connects to the scale over BlueZ and subscribes to its notifications.
func Open(ctx context.Context, adapter *bluez.Adapter, opt Options) (*Session, error) {
	t, err := Dial(ctx, adapter, opt.Address)
	if err != nil {
		return nil, err
	}
	s, err := NewSession(ctx, t, opt)
	if err != nil {
		t.Close()
		return nil, err
	}
	return s, nil
}

// NewSession starts a session on an established transport.
func NewSession(ctx context.Context, t Transport, opt Options) (*Session, error) {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	s := &Session{
		opt:   opt,
		log:   opt.Log,
		t:     t,
		inbox: map[string]chan packet{},
		accum: map[string]*hagrid.Accumulator{},
	}
	for _, name := range []string{inboxStatus, inboxRealtime, chrRequestAuth, chrAuthToken, chrSendWorkKey,
		chrSetUserInfo, chrGetManagerInfo, chrCurrentTime, chrProductInfo, chrScaleVersion, chrWeightUnit, chrHistoryWeight} {
		s.inbox[name] = make(chan packet, 64)
	}
	if err := t.Subscribe(ctx, s.onNotify); err != nil {
		return nil, err
	}
	return s, nil
}

// Close disconnects.
func (s *Session) Close() { s.t.Close() }

// Disconnected is closed when the scale drops the link.
func (s *Session) Disconnected() <-chan struct{} { return s.t.Disconnected() }

func (s *Session) onNotify(uuid string, raw []byte) {
	if s.opt.Trace {
		s.log.Debug("rx", "char", charNames[uuid], "data", hex.EncodeToString(raw))
	}
	payload := raw
	if hagrid.IsNotifyFrame(raw) {
		s.mu.Lock()
		acc := s.accum[uuid]
		if acc == nil {
			acc = &hagrid.Accumulator{}
			s.accum[uuid] = acc
		}
		p, enc, done, err := acc.Ingest(raw)
		key := s.workKey
		s.mu.Unlock()
		if err != nil {
			s.log.Warn("dropping malformed WSP frame", "char", charNames[uuid], "err", err, "data", hex.EncodeToString(raw))
			return
		}
		if !done {
			return
		}
		if enc {
			if key == nil {
				s.log.Warn("encrypted notification before session key", "char", charNames[uuid])
				return
			}
			if p, err = hagrid.Decrypt(p, key); err != nil {
				s.log.Warn("decrypt failed", "char", charNames[uuid], "err", err)
				return
			}
		}
		payload = p
	} else if uuid == chrWeightMeasure || uuid == chrBodyComposition {
		// Standard GATT weight/body-composition values are not used by the
		// Scale 3 protocol.
		s.log.Debug("ignoring standard GATT measurement", "char", charNames[uuid], "data", hex.EncodeToString(raw))
		return
	}
	// Anything else without a WSP header is a raw payload; some firmwares
	// answer status/profile/info requests unframed (openScale handles these
	// the same way).
	if s.opt.Trace {
		s.log.Debug("payload", "char", charNames[uuid], "data", hex.EncodeToString(payload))
	}
	ch := s.inbox[inboxFor(uuid)]
	if ch == nil {
		return
	}
	select {
	case ch <- packet{data: payload, at: time.Now()}:
	default:
		s.log.Warn("inbox full, dropping payload", "char", charNames[uuid])
	}
}

func (s *Session) drain(name string) {
	for {
		select {
		case <-s.inbox[name]:
		default:
			return
		}
	}
}

func (s *Session) await(ctx context.Context, name string, timeout time.Duration) ([]byte, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case p := <-s.inbox[name]:
		return p.data, nil
	case <-t.C:
		return nil, fmt.Errorf("timeout waiting for %s response", nameOf(name))
	case <-s.t.Disconnected():
		return nil, ErrDisconnected
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func nameOf(n string) string {
	if v, ok := charNames[n]; ok {
		return v
	}
	return n
}

func (s *Session) has(uuid string) bool { return s.t.Has(uuid) }

func (s *Session) writeFrames(ctx context.Context, uuid string, frames [][]byte) error {
	if !s.t.Has(uuid) {
		return fmt.Errorf("characteristic %s not present", nameOf(uuid))
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for _, f := range frames {
		if s.opt.Trace {
			s.log.Debug("tx", "char", charNames[uuid], "data", hex.EncodeToString(f))
		}
		wctx, cancel := context.WithTimeout(ctx, defaultReplyTimeout)
		err := s.t.Write(wctx, uuid, f)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) writePlain(ctx context.Context, uuid string, payload []byte) error {
	frames, err := hagrid.BuildWriteFrames(payload, false)
	if err != nil {
		return err
	}
	return s.writeFrames(ctx, uuid, frames)
}

func (s *Session) writeEncrypted(ctx context.Context, uuid string, plaintext, key []byte) error {
	iv, err := randomBytes(16)
	if err != nil {
		return err
	}
	frames, err := hagrid.BuildEncryptedWriteFrames(plaintext, key, iv)
	if err != nil {
		return err
	}
	return s.writeFrames(ctx, uuid, frames)
}

// request writes a plain payload to uuid and waits for its notification.
func (s *Session) request(ctx context.Context, uuid string, payload []byte, timeout time.Duration) ([]byte, error) {
	s.drain(inboxFor(uuid))
	if err := s.writePlain(ctx, uuid, payload); err != nil {
		return nil, err
	}
	return s.await(ctx, inboxFor(uuid), timeout)
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return b, err
}

// Authenticate runs the challenge/response handshake and installs a fresh
// AES session key.
func (s *Session) Authenticate(ctx context.Context) error {
	challenge, err := s.request(ctx, chrRequestAuth, nil, 10*time.Second)
	if err != nil {
		return fmt.Errorf("auth challenge: %w", err)
	}
	if len(challenge) < 16 {
		return fmt.Errorf("auth challenge too short (%d bytes)", len(challenge))
	}
	randA := challenge[:16]
	randB, err := randomBytes(16)
	if err != nil {
		return err
	}
	cak := s.opt.Secrets.CAK
	resp, err := s.request(ctx, chrAuthToken, hagrid.AuthTokenPayload(randA, randB, cak), 10*time.Second)
	if err != nil {
		return fmt.Errorf("auth token: %w", err)
	}
	if !hagrid.ValidAuthResponse(resp, randA, randB, cak) {
		return ErrAuthFailed
	}

	c3, err := hagrid.C3FromAddress(s.opt.Address)
	if err != nil {
		return err
	}
	root, err := hagrid.DeriveRootKey(s.opt.Secrets.C1, s.opt.Secrets.C2, c3)
	if err != nil {
		return err
	}
	workKey, err := randomBytes(16)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.workKey = workKey
	s.mu.Unlock()

	s.drain(chrSendWorkKey)
	if err := s.writeEncrypted(ctx, chrSendWorkKey, workKey, root); err != nil {
		return fmt.Errorf("send work key: %w", err)
	}
	ack, err := s.await(ctx, chrSendWorkKey, 10*time.Second)
	if err != nil {
		return fmt.Errorf("work key ack: %w", err)
	}
	if len(ack) == 0 || ack[0] != 0 {
		return fmt.Errorf("%w: work key rejected (status %x) - c1/c2 or address likely wrong", ErrAuthFailed, ack)
	}
	return nil
}

// SyncTime sets the scale clock to t.
func (s *Session) SyncTime(ctx context.Context, t time.Time) error {
	s.drain(chrCurrentTime)
	return s.writePlain(ctx, chrCurrentTime, hagrid.CurrentTimePayload(t))
}

// ManagerInfo reads the account binding (HUID) stored on the scale.
func (s *Session) ManagerInfo(ctx context.Context) (*hagrid.ManagerInfo, error) {
	if !s.has(chrGetManagerInfo) {
		return nil, errors.New("manager-info not supported")
	}
	p, err := s.request(ctx, chrGetManagerInfo, nil, 3*time.Second)
	if err != nil {
		return nil, err
	}
	return hagrid.ParseManagerInfo(p)
}

// SetUserInfo writes the (encrypted) user profile and waits for the ack.
func (s *Session) SetUserInfo(ctx context.Context, u hagrid.UserInfo) error {
	if !s.has(chrSetUserInfo) {
		return errors.New("set-user-info not supported")
	}
	s.mu.Lock()
	key := s.workKey
	s.mu.Unlock()
	if key == nil {
		return errors.New("not authenticated")
	}
	s.drain(chrSetUserInfo)
	if err := s.writeEncrypted(ctx, chrSetUserInfo, u.Payload(), key); err != nil {
		return err
	}
	ack, err := s.await(ctx, chrSetUserInfo, 4*time.Second)
	if err != nil {
		return err
	}
	if len(ack) == 0 || ack[0] != 0 {
		return fmt.Errorf("scale rejected user info (status %x)", ack)
	}
	return nil
}

// DeviceInfo is static information reported by the scale.
type DeviceInfo struct {
	ProductInfo string `json:"product_info,omitempty"`
	ProductID   string `json:"product_id,omitempty"`
	Firmware    string `json:"firmware,omitempty"`
	Serial      string `json:"serial,omitempty"`
	Unit        string `json:"unit,omitempty"`
}

// ReadDeviceInfo queries product info, firmware version and unit (best effort).
func (s *Session) ReadDeviceInfo(ctx context.Context) DeviceInfo {
	var d DeviceInfo
	if p, err := s.request(ctx, chrProductInfo, nil, 3*time.Second); err == nil {
		d.ProductInfo = hagrid.ParseProductInfo(p)
		if len(d.ProductInfo) >= 4 {
			d.ProductID = d.ProductInfo[:4]
		}
	} else {
		s.log.Debug("product info", "err", err)
	}
	if p, err := s.request(ctx, chrScaleVersion, nil, 3*time.Second); err == nil {
		d.Firmware, d.Serial, _ = hagrid.ParseScaleVersion(p)
	} else {
		s.log.Debug("scale version", "err", err)
	}
	if p, err := s.request(ctx, chrWeightUnit, nil, 3*time.Second); err == nil {
		d.Unit = hagrid.ParseWeightUnit(p)
	} else {
		s.log.Debug("weight unit", "err", err)
	}
	return d
}

// MeasureResult is the outcome of Measure.
type MeasureResult struct {
	Measurement *hagrid.Measurement // final realtime record, nil if none
	ReceivedAt  time.Time
	// Active is true when the record was pushed during this connection
	// (someone weighed), false when it only came back in answer to the
	// closing realtime request.
	Active bool
}

// Measure captures the current weighing.
//
// Observed on a Scale 3 (fw 2.0.0.70): after the realtime request the scale
// pushes one realtime record once weight and impedance are measured. It
// answers only the first status poll, with 01 ("no completed measurement
// yet") even when nobody stands on it, and never sends 00. So a realtime
// record is the signal that a weighing happened. The scale answers each
// realtime request with one record; the first can be weight-only (impedance
// still being measured), so Measure re-requests until a record carries the
// impedance, then waits a short settle time for a refined record. A status 00 (other firmwares)
// returns immediately. Without a record within idleGrace it asks once more
// and gives up.
func (s *Session) Measure(ctx context.Context, idleGrace time.Duration, onLive func(*hagrid.Measurement)) (MeasureResult, error) {
	var res MeasureResult
	s.drain(inboxRealtime)
	s.drain(inboxStatus)
	if err := s.writePlain(ctx, chrRealtimeWeight, nil); err != nil {
		return res, err
	}
	pollStatus := s.has(chrStatusPoll)
	pollErrLogged := false
	polls := 0
	poll := func() {
		if !pollStatus || polls >= maxStatusPolls {
			return
		}
		polls++
		if err := s.writePlain(ctx, chrStatusPoll, nil); err != nil && !pollErrLogged {
			pollErrLogged = true
			s.log.Warn("status poll write failed", "err", err)
		}
	}
	poll()
	ticker := time.NewTicker(statusPollInterval)
	defer ticker.Stop()

	idle := time.After(idleGrace)
	deadline := time.After(time.Duration(maxStatusPolls) * statusPollInterval)
	var settle, final, refetch <-chan time.Time
	var incompleteSince time.Time
	requestFinal := func(why string) {
		if final != nil {
			return
		}
		s.log.Debug("requesting final realtime record", "reason", why)
		if err := s.writePlain(ctx, chrRealtimeWeight, nil); err != nil {
			s.log.Debug("final realtime request", "err", err)
		}
		final = time.After(finalRecordWait)
	}

	for {
		select {
		case p := <-s.inbox[inboxRealtime]:
			m, err := hagrid.ParseRealtime(p.data)
			if err != nil || m.WeightKg <= 0 {
				s.log.Debug("ignoring realtime payload", "len", len(p.data), "err", err)
				continue
			}
			res.Measurement, res.ReceivedAt = m, p.at
			if final != nil {
				return res, nil // answer to the closing request
			}
			res.Active = true
			if onLive != nil {
				onLive(m)
			}
			if m.LowImpedance[0] > 0 {
				refetch = nil
				settle = time.After(settleWait)
				continue
			}
			// Weight only: ask again until the impedance is in.
			if incompleteSince.IsZero() {
				incompleteSince = p.at
				settle = time.After(incompleteWait)
				s.log.Debug("record has no impedance yet; re-requesting")
			}
			refetch = time.After(refetchInterval)
		case <-refetch:
			refetch = nil
			if err := s.writePlain(ctx, chrRealtimeWeight, nil); err != nil {
				s.log.Debug("realtime re-request", "err", err)
			}
		case p := <-s.inbox[inboxStatus]:
			if len(p.data) == 0 {
				continue
			}
			s.log.Debug("measurement status", "status", p.data[0])
			if p.data[0] == 0x00 && res.Active {
				return res, nil
			}
		case <-ticker.C:
			if final == nil {
				poll()
			}
		case <-idle:
			if res.Measurement == nil {
				requestFinal("no weighing within idle grace")
			}
		case <-settle:
			if m := res.Measurement; m != nil && m.LowImpedance[0] == 0 {
				s.log.Info("no impedance measured (stand barefoot on the electrodes until the scale finishes); recording weight only")
			}
			return res, nil
		case <-final:
			return res, nil
		case <-deadline:
			s.log.Warn("measurement did not complete in time")
			if res.Measurement != nil {
				return res, nil
			}
			requestFinal("timeout")
		case <-s.t.Disconnected():
			return res, ErrDisconnected
		case <-ctx.Done():
			return res, ctx.Err()
		}
	}
}

// ReadHistory downloads the measurements stored on the scale.
func (s *Session) ReadHistory(ctx context.Context) ([]*hagrid.Measurement, error) {
	if !s.has(chrHistoryWeight) {
		return nil, nil
	}
	var out []*hagrid.Measurement
	s.drain(chrHistoryWeight)
	req := []byte(nil)
	for i := 0; i < maxHistoryRecords; i++ {
		p, err := s.request(ctx, chrHistoryWeight, req, 5*time.Second)
		if err != nil {
			if errors.Is(err, ErrDisconnected) || ctx.Err() != nil {
				return out, err
			}
			s.log.Debug("history read ended", "err", err)
			return out, nil
		}
		m, err := hagrid.ParseHistory(p)
		if err != nil || m.WeightKg <= 0 {
			s.log.Debug("history read finished", "len", len(p), "data", hex.EncodeToString(p))
			return out, nil
		}
		out = append(out, m)
		if m.IsLastHistoryRecord() {
			return out, nil
		}
		req = []byte{0x00} // acknowledge and fetch the next record
	}
	return out, nil
}
