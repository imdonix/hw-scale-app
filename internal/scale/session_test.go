package scale

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tamasmagyar/sscale/internal/hagrid"
)

func testSecrets(t *testing.T) hagrid.Secrets {
	s, err := hagrid.ParseSecrets("0F1E2D3C4B5A69788796A5B4C3D2E1F0", "CA4946D061C9FE534F6044F930EBB69B", "FBCE6E2B4BAF80ED969BA26B4A4B9325")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// record builds a realtime record: weight, timestamp, impedance, hr.
func record(weightKg float64, ts time.Time, imp int, hr int) []byte {
	p := make([]byte, 26)
	w := int(weightKg*100 + 0.5)
	p[0], p[1] = byte(w), byte(w>>8)
	p[4], p[5] = byte(ts.Year()), byte(ts.Year()>>8)
	p[6], p[7], p[8], p[9], p[10] = byte(ts.Month()), byte(ts.Day()), byte(ts.Hour()), byte(ts.Minute()), byte(ts.Second())
	p[12], p[13] = byte(imp), byte(imp>>8)
	p[24], p[25] = byte(hr), byte(hr>>8)
	return p
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func init() {
	SetTiming(20*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond)
}

func TestFullSessionAgainstSimulatedScale(t *testing.T) {
	sec := testSecrets(t)
	now := time.Now().Truncate(time.Second)
	fake := &FakeScale{
		T: t, Secrets: sec, Address: "08:B8:D0:C2:9D:CD", HUID: "123456789",
		Statuses: []byte{1, 1, 1, 0},
		Realtime: record(86.25, now, 5098, 97),
		History: [][]byte{
			append(record(85.1, now.Add(-48*time.Hour), 5000, 0), 0x01, 0x01),
			append(record(85.4, now.Add(-24*time.Hour), 5010, 0), 0x01, 0x00),
		},
	}
	ctx := context.Background()
	s, err := NewSession(ctx, fake, Options{Secrets: sec, Address: fake.Address, Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Authenticate(ctx); err != nil {
		t.Fatalf("auth: %v", err)
	}
	if err := s.SyncTime(ctx, now); err != nil {
		t.Fatal(err)
	}
	mi, err := s.ManagerInfo(ctx)
	if err != nil || mi.HUID != "123456789" || mi.DeviceID != "device-id" {
		t.Fatalf("manager info %+v %v", mi, err)
	}
	ui := hagrid.UserInfo{HUID: mi.HUID, UID: "u:00000001", Gender: 1, Age: 26, HeightCm: 178, WeightKg: 80}
	if err := s.SetUserInfo(ctx, ui); err != nil {
		t.Fatalf("user info: %v", err)
	}
	fake.mu.Lock()
	gotUI, gotTime := fake.UserInfo, fake.TimeSet
	fake.mu.Unlock()
	if string(gotUI) != string(ui.Payload()) {
		t.Fatalf("scale decrypted user info %x", gotUI)
	}
	if string(gotTime) != string(hagrid.CurrentTimePayload(now)) {
		t.Fatalf("time %x", gotTime)
	}
	info := s.ReadDeviceInfo(ctx)
	if info.ProductID != "M00D" || info.Firmware != "2.0.0.70" || info.Unit != "kg" {
		t.Fatalf("device info %+v", info)
	}

	var live int
	res, err := s.Measure(ctx, time.Second, func(*hagrid.Measurement) { live++ })
	if err != nil {
		t.Fatal(err)
	}
	if res.Measurement == nil || res.Measurement.WeightKg != 86.25 || !res.Active || live == 0 {
		t.Fatalf("measure %+v live=%d", res, live)
	}
	if res.Measurement.HeartRate != 97 || res.Measurement.Scale3ImpedanceOhm() != 509.8 {
		t.Fatalf("measurement %+v", res.Measurement)
	}

	hist, err := s.ReadHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[0].WeightKg != 85.1 || hist[1].WeightKg != 85.4 || !hist[1].IsLastHistoryRecord() {
		t.Fatalf("history %+v", hist)
	}
	if fake.Acks != 1 {
		t.Fatalf("expected 1 history ack, got %d", fake.Acks)
	}
	s.Close()
	if !fake.Closed {
		t.Fatal("not disconnected")
	}
}

func TestWrongKeysRejected(t *testing.T) {
	sec := testSecrets(t)
	fake := &FakeScale{T: t, Secrets: sec, Address: "08:B8:D0:C2:9D:CD"}
	wrong := sec
	wrong.CAK = make([]byte, 16)
	s, _ := NewSession(context.Background(), fake, Options{Secrets: wrong, Address: fake.Address, Log: quietLog()})
	if err := s.Authenticate(context.Background()); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected auth failure, got %v", err)
	}
}

func TestWrongAddressRejectedAtWorkKey(t *testing.T) {
	sec := testSecrets(t)
	fake := &FakeScale{T: t, Secrets: sec, Address: "08:B8:D0:C2:9D:CD"}
	// The work key is wrapped with a root key bound to the address; the fake
	// derives it from its own address and acks with 0 regardless, so verify the
	// key it unwrapped differs from the client's when the address is wrong.
	s, _ := NewSession(context.Background(), fake, Options{Secrets: sec, Address: "11:22:33:44:55:66", Log: quietLog()})
	if err := s.Authenticate(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if string(fake.workKey) == string(s.workKey) {
		t.Fatal("work key should not match with the wrong address")
	}
}

func TestIdleScaleReturnsNoMeasurement(t *testing.T) {
	sec := testSecrets(t)
	fake := &FakeScale{T: t, Secrets: sec, Address: "08:B8:D0:C2:9D:CD", Statuses: []byte{0}}
	ctx := context.Background()
	s, _ := NewSession(ctx, fake, Options{Secrets: sec, Address: fake.Address, Log: quietLog()})
	if err := s.Authenticate(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := s.Measure(ctx, 150*time.Millisecond, nil)
	if err != nil || res.Measurement != nil || res.Active {
		t.Fatalf("%+v %v", res, err)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("idle grace not honoured")
	}
	hist, err := s.ReadHistory(ctx)
	if err != nil || len(hist) != 0 {
		t.Fatalf("history %v %v", hist, err)
	}
}

func TestMeasureStopsOnDisconnect(t *testing.T) {
	sec := testSecrets(t)
	fake := &FakeScale{T: t, Secrets: sec, Address: "08:B8:D0:C2:9D:CD", Statuses: []byte{1}}
	ctx := context.Background()
	s, _ := NewSession(ctx, fake, Options{Secrets: sec, Address: fake.Address, Log: quietLog()})
	if err := s.Authenticate(ctx); err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(100 * time.Millisecond); fake.Close() }()
	if _, err := s.Measure(ctx, time.Second, nil); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("expected ErrDisconnected, got %v", err)
	}
}

func TestRawRepliesAreAccepted(t *testing.T) {
	sec := testSecrets(t)
	now := time.Now().Truncate(time.Second)
	fake := &FakeScale{T: t, Secrets: sec, Address: "08:B8:D0:C2:9D:CD", HUID: "HUID-RAW",
		Statuses: []byte{1, 1, 0}, Realtime: record(77.10, now, 5143, 0), RawReplies: true}
	ctx := context.Background()
	s, _ := NewSession(ctx, fake, Options{Secrets: sec, Address: fake.Address, Log: quietLog()})
	if err := s.Authenticate(ctx); err != nil {
		t.Fatal(err)
	}
	mi, err := s.ManagerInfo(ctx)
	if err != nil || mi.HUID != "HUID-RAW" {
		t.Fatalf("raw manager info: %+v %v", mi, err)
	}
	res, err := s.Measure(ctx, time.Second, nil)
	if err != nil || !res.Active || res.Measurement == nil || res.Measurement.WeightKg != 77.10 {
		t.Fatalf("raw status: %+v %v", res, err)
	}
}

func TestSilentStatusFinishesEarly(t *testing.T) {
	sec := testSecrets(t)
	now := time.Now().Truncate(time.Second)
	fake := &FakeScale{T: t, Secrets: sec, Address: "08:B8:D0:C2:9D:CD",
		Realtime: record(77.10, now, 5143, 0), NoStatus: true}
	ctx := context.Background()
	s, _ := NewSession(ctx, fake, Options{Secrets: sec, Address: fake.Address, Log: quietLog()})
	if err := s.Authenticate(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := s.Measure(ctx, 50*time.Millisecond, nil)
	if err != nil || res.Measurement == nil {
		t.Fatalf("%+v %v", res, err)
	}
	if n := fake.PollCount(); n >= maxStatusPolls {
		t.Fatalf("waited for the full timeout (%d polls)", n)
	}
}

// Mirrors a real Scale 3 (fw 2.0.0.70) trace: only the first status poll is
// answered (01), the realtime record is pushed seconds later, 00 never comes.
func TestRealScale3Behaviour(t *testing.T) {
	sec := testSecrets(t)
	now := time.Now().Truncate(time.Second)
	fake := &FakeScale{T: t, Secrets: sec, Address: "08:B8:D0:C2:9D:CD",
		Statuses: []byte{1}, StatusReplies: 1,
		Realtime: record(77.10, now, 5143, 0), RealtimeDelay: 150 * time.Millisecond}
	ctx := context.Background()
	s, _ := NewSession(ctx, fake, Options{Secrets: sec, Address: fake.Address, Log: quietLog()})
	if err := s.Authenticate(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := s.Measure(ctx, time.Second, nil)
	if err != nil || res.Measurement == nil || !res.Active || res.Measurement.WeightKg != 77.10 {
		t.Fatalf("%+v %v", res, err)
	}
	// realtime delay + settle, far below the 30-poll timeout.
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("measure took %s", d)
	}
}

// Woken but nobody weighs: status 01 forever, no realtime record.
func TestIdleScale3Leaves(t *testing.T) {
	sec := testSecrets(t)
	fake := &FakeScale{T: t, Secrets: sec, Address: "08:B8:D0:C2:9D:CD", Statuses: []byte{1}, StatusReplies: 1}
	ctx := context.Background()
	s, _ := NewSession(ctx, fake, Options{Secrets: sec, Address: fake.Address, Log: quietLog()})
	if err := s.Authenticate(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := s.Measure(ctx, 100*time.Millisecond, nil)
	if err != nil || res.Measurement != nil || res.Active {
		t.Fatalf("%+v %v", res, err)
	}
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Fatalf("idle session lingered %s", d)
	}
}

// Real trace 13:04: the first record was weight-only (impedance 0).
func TestWeightOnlyRecordIsRefetched(t *testing.T) {
	sec := testSecrets(t)
	now := time.Now().Truncate(time.Second)
	fake := &FakeScale{T: t, Secrets: sec, Address: "08:B8:D0:C2:9D:CD", Statuses: []byte{1}, StatusReplies: 1,
		RealtimeSeq: [][]byte{record(77.50, now, 0, 0), record(77.50, now, 0, 0), record(77.45, now, 5143, 0)}}
	ctx := context.Background()
	s, _ := NewSession(ctx, fake, Options{Secrets: sec, Address: fake.Address, Log: quietLog()})
	if err := s.Authenticate(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := s.Measure(ctx, time.Second, nil)
	if err != nil || res.Measurement == nil || res.Measurement.LowImpedance[0] != 5143 || res.Measurement.WeightKg != 77.45 {
		t.Fatalf("%+v %v", res.Measurement, err)
	}
}

// Stepped off before the impedance was measured: keep the weight.
func TestWeightOnlyRecordIsKept(t *testing.T) {
	sec := testSecrets(t)
	now := time.Now().Truncate(time.Second)
	fake := &FakeScale{T: t, Secrets: sec, Address: "08:B8:D0:C2:9D:CD", Statuses: []byte{1}, StatusReplies: 1,
		RealtimeSeq: [][]byte{record(77.50, now, 0, 0)}}
	ctx := context.Background()
	s, _ := NewSession(ctx, fake, Options{Secrets: sec, Address: fake.Address, Log: quietLog()})
	if err := s.Authenticate(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := s.Measure(ctx, time.Second, nil)
	if err != nil || res.Measurement == nil || res.Measurement.WeightKg != 77.50 || !res.Active {
		t.Fatalf("%+v %v", res, err)
	}
}
