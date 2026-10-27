package hagrid

import (
	"bytes"
	"encoding/hex"
	"math"
	"testing"
	"time"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func toNotify(f []byte) []byte {
	n := append([]byte(nil), f...)
	n[0] = FrameNotifyPlain
	crc := CRC16Modbus(n[:len(n)-2])
	n[len(n)-2] = byte(crc)
	n[len(n)-1] = byte(crc >> 8)
	return n
}

func TestFramesRoundTrip(t *testing.T) {
	payload := make([]byte, 40)
	for i := range payload {
		payload[i] = byte(i)
	}
	frames, err := BuildWriteFrames(payload, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames", len(frames))
	}
	var acc Accumulator
	for i, f := range frames {
		out, _, done, err := acc.Ingest(toNotify(f))
		if err != nil {
			t.Fatal(err)
		}
		if done != (i == 2) {
			t.Fatalf("frame %d done=%v", i, done)
		}
		if done && !bytes.Equal(out, payload) {
			t.Fatalf("mismatch %x", out)
		}
	}
}

func TestEmptyWriteFrame(t *testing.T) {
	frames, _ := BuildWriteFrames(nil, false)
	if len(frames) != 1 || len(frames[0]) != 5 || frames[0][1] != 3 || frames[0][2] != 0 {
		t.Fatalf("unexpected empty frame %x", frames)
	}
}

func TestAccumulatorOrphan(t *testing.T) {
	a := make([]byte, 26)
	b := bytes.Repeat([]byte{7}, 26)
	fa, _ := BuildWriteFrames(a, false)
	fb, _ := BuildWriteFrames(b, false)
	var acc Accumulator
	if _, _, done, _ := acc.Ingest(toNotify(fa[1])); done {
		t.Fatal("orphan completed")
	}
	acc.Ingest(toNotify(fb[0]))
	out, _, done, _ := acc.Ingest(toNotify(fb[1]))
	if !done || !bytes.Equal(out, b) {
		t.Fatal("expected payload b")
	}
}

func TestAuthToken(t *testing.T) {
	randA, randB, cak := make([]byte, 16), make([]byte, 16), make([]byte, 16)
	for i := 0; i < 16; i++ {
		randA[i], randB[i], cak[i] = byte(i), byte(i+16), byte(i+32)
	}
	tok := AuthTokenPayload(randA, randB, cak)
	if len(tok) != 48 || !bytes.Equal(tok[:16], randB) {
		t.Fatal("bad token layout")
	}
	resp := ExpectedAuthResponse(randA, randB, cak)
	if !ValidAuthResponse(resp, randA, randB, cak) || !ValidAuthResponse(append(resp, 0), randA, randB, cak) {
		t.Fatal("valid response rejected")
	}
	if ValidAuthResponse(resp[:31], randA, randB, cak) {
		t.Fatal("short response accepted")
	}
}

// Vector from openScale's Huawei Scale 3 PoC capture.
func TestScale3Capture(t *testing.T) {
	c3, err := C3FromAddress("38:1E:C7:66:08:D3")
	if err != nil {
		t.Fatal(err)
	}
	root, err := DeriveRootKey(mustHex(t, "CA4946D061C9FE534F6044F930EBB69B"), mustHex(t, "FBCE6E2B4BAF80ED969BA26B4A4B9325"), c3)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(root) != "d680f0f3b50576a74c8815653e8b7243" {
		t.Fatalf("root key %x", root)
	}
	deframe := func(frames ...string) []byte {
		var out []byte
		for _, f := range frames {
			b := mustHex(t, f)
			out = append(out, b[3:len(b)-2]...)
		}
		return out
	}
	wk, err := Decrypt(deframe("dc12203b033efce70954e24a3a06ac75603b418e", "dc12218a68cb34e5250a6c185813b62e6958c13f", "dc0522a49ddc06"), root)
	if err != nil || hex.EncodeToString(wk) != "36697370363671706d64386733696b62" {
		t.Fatalf("work key %x %v", wk, err)
	}

	// Feed the encrypted notification through the accumulator (validates CRC-Modbus).
	var acc Accumulator
	var enc []byte
	for _, f := range []string{"cd1220ddae5ad500d352ad002d9ab60055a313f1", "cd12216d4c5b1e4fa529d8fc4225d8db8b3726b3", "cd0f222285c5b012308c6ee4a7829efa52"} {
		out, encrypted, done, err := acc.Ingest(mustHex(t, f))
		if err != nil {
			t.Fatal(err)
		}
		if done {
			if !encrypted {
				t.Fatal("expected encrypted")
			}
			enc = out
		}
	}
	plain, err := Decrypt(enc, wk)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(plain) != "b1212d01ea07080917271da0ea13000000000000000000006100" {
		t.Fatalf("plain %x", plain)
	}
	m, err := ParseRealtime(plain)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(m.WeightKg-86.25) > 1e-9 || math.Abs(m.FatPercent-30.1) > 1e-9 || m.HeartRate != 97 {
		t.Fatalf("measurement %+v", m)
	}
	if m.Timestamp == nil || !m.Timestamp.Equal(time.Date(2026, 8, 9, 23, 39, 29, 0, time.Local)) {
		t.Fatalf("timestamp %v", m.Timestamp)
	}
	if m.Scale3ImpedanceOhm() != 509.8 {
		t.Fatalf("impedance %v", m.Scale3ImpedanceOhm())
	}
}

func realtimePayload() []byte {
	return []byte{
		0x34, 0x1E, 0xB9, 0x00,
		0xEA, 0x07, 0x06, 0x16, 0x0F, 0x0E, 0x2A, 0x00,
		0xF4, 0x01, 0xF5, 0x01, 0xF6, 0x01, 0xF7, 0x01, 0xF8, 0x01, 0xF9, 0x01,
		0x48, 0x00,
		0x58, 0x02, 0x59, 0x02, 0x5A, 0x02, 0x5B, 0x02, 0x5C, 0x02, 0x5D, 0x02,
	}
}

func TestRealtimeAndHistory(t *testing.T) {
	m, err := ParseRealtime(realtimePayload())
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(m.WeightKg-77.32) > 1e-9 || m.HeartRate != 72 || m.LowImpedance[0] != 500 || m.HighImp[5] != 605 {
		t.Fatalf("%+v", m)
	}
	h, err := ParseHistory(append(realtimePayload(), 0x01, 0x00))
	if err != nil {
		t.Fatal(err)
	}
	if !h.IsLastHistoryRecord() || h.Fingerprint() != m.Fingerprint() {
		t.Fatal("history flags / fingerprint mismatch")
	}
	bad := append(realtimePayload(), 1, 0)
	bad[4], bad[5] = 0, 0
	if _, err := ParseHistory(bad); err == nil {
		t.Fatal("history with invalid timestamp accepted")
	}
	if m2, err := ParseRealtime(bad[:38]); err != nil || m2.Timestamp != nil {
		t.Fatal("realtime with invalid timestamp should parse")
	}
}

func TestUserInfoPayload(t *testing.T) {
	p := UserInfo{HUID: "openscale:001122334455", UID: "u:00000007", Gender: 1, Age: 26, HeightCm: 171, WeightKg: 83.2}.Payload()
	if len(p) != 69 || p[62] != 1 || p[63] != 26 || p[64] != 171 || p[65] != 0 || p[66] != 0x80 || p[67] != 0x20 {
		t.Fatalf("%x", p)
	}
}

func TestCurrentTimePayload(t *testing.T) {
	p := CurrentTimePayload(time.Date(2026, 10, 12, 13, 14, 15, 0, time.UTC)) // Monday
	want := []byte{0xEA, 0x07, 10, 12, 13, 14, 15, 1}
	if !bytes.Equal(p, want) {
		t.Fatalf("%x", p)
	}
	if p := CurrentTimePayload(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)); p[7] != 7 {
		t.Fatal("sunday should be 7")
	}
}

func TestScaleVersion(t *testing.T) {
	// 2.0.0.70 => bits 0010 0000 0000 0001000110 00
	v, _, err := ParseScaleVersion([]byte{0, 0x20, 0x01, 0x18, 'S'})
	if err != nil {
		t.Fatal(err)
	}
	if v != "2.0.0.70" {
		t.Fatalf("version %s", v)
	}
}

func TestComposition(t *testing.T) {
	check := func(h, w float64, age int, male bool, z, fat, water, smm, bone float64, bmr, vfl int) {
		t.Helper()
		c := CalculateComposition(h, w, age, male, z)
		if c == nil {
			t.Fatal("nil composition")
		}
		near := func(a, b, tol float64) bool { return math.Abs(a-b) <= tol }
		if !near(c.BodyFatPercent, fat, 0.01) || !near(c.WaterPercent, water, 0.01) || !near(c.SkeletalMuscleKg, smm, 0.01) ||
			!near(c.BoneMineralKg, bone, 0.01) || math.Abs(float64(c.BMRKcal-bmr)) > 1 || c.VisceralFatLevel != vfl {
			t.Fatalf("%+v", c)
		}
	}
	check(178, 91.45, 46, true, 362.5, 25.341705, 54.293285, 37.3210, 3.51146, 1943, 14)
	check(178, 91.50, 46, true, 370.5, 25.73119, 53.47523, 37.13919, 3.50382, 1931, 14)
	check(165, 65, 40, false, 400, 30.9025, 55.7124, 23.6117, 2.7600, 1340, 5)
	if CalculateComposition(150, 45, 17, true, 450) != nil || CalculateComposition(178, 91, 46, true, 0) != nil {
		t.Fatal("expected nil")
	}
}
