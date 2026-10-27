package hagrid

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Measurement is a decoded realtime or history weight record.
type Measurement struct {
	Raw          []byte     // record bytes without history flags
	WeightKg     float64    // weight in kg
	FatPercent   float64    // body fat as reported by the scale (often 0)
	Timestamp    *time.Time // scale-local timestamp, nil if invalid
	LowImpedance []int      // 6 raw low-frequency impedance samples
	HighImp      []int      // 6 raw high-frequency samples (Scale 3 Pro only)
	HeartRate    int        // bpm, 0 if absent

	HistorySuspected *int
	HistoryComplete  *int // 0 means this was the last stored record
}

// IsLastHistoryRecord reports whether the scale marked this as the final
// stored record.
func (m *Measurement) IsLastHistoryRecord() bool {
	return m.HistoryComplete != nil && *m.HistoryComplete == 0
}

// Scale3ImpedanceOhm returns the Scale 3 low-frequency impedance in ohms.
// The Scale 3 reports it in 0.1 Ω units.
func (m *Measurement) Scale3ImpedanceOhm() float64 {
	if len(m.LowImpedance) == 0 || m.LowImpedance[0] <= 0 {
		return 0
	}
	return float64(m.LowImpedance[0]) / 10
}

// Fingerprint identifies a record independent of whether it was received
// live or from history.
func (m *Measurement) Fingerprint() string {
	return hex.EncodeToString(m.Raw)
}

func u16(b []byte, off int) int { return int(binary.LittleEndian.Uint16(b[off:])) }

// readTimestamp decodes year(u16le) month day hour minute second in local time.
func readTimestamp(b []byte, off int) *time.Time {
	if off+6 >= len(b) {
		return nil
	}
	y, mo, d := u16(b, off), int(b[off+2]), int(b[off+3])
	h, mi, s := int(b[off+4]), int(b[off+5]), int(b[off+6])
	if y < 2000 || mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 || s > 59 {
		return nil
	}
	t := time.Date(y, time.Month(mo), d, h, mi, s, 0, time.Local)
	return &t
}

func parseWeight(p []byte, allowed []int, requireTS bool) (*Measurement, error) {
	ok := false
	for _, l := range allowed {
		if len(p) == l {
			ok = true
		}
	}
	if !ok {
		return nil, fmt.Errorf("unsupported weight payload length %d", len(p))
	}
	base := 26
	if len(p) >= 38 {
		base = 38
	}
	m := &Measurement{
		Raw:        append([]byte(nil), p[:base]...),
		WeightKg:   float64(u16(p, 0)) / 100,
		FatPercent: float64(u16(p, 2)) / 10,
		Timestamp:  readTimestamp(p, 4),
	}
	if m.Timestamp == nil && requireTS {
		return nil, fmt.Errorf("weight payload has invalid timestamp")
	}
	for i := 0; i < 6; i++ {
		m.LowImpedance = append(m.LowImpedance, u16(p, 12+i*2))
	}
	if base == 38 {
		for i := 0; i < 6; i++ {
			m.HighImp = append(m.HighImp, u16(p, 26+i*2))
		}
	}
	if hr := u16(p, 24); hr >= 1 && hr <= 240 {
		m.HeartRate = hr
	}
	if len(p) > base {
		v := int(p[base])
		m.HistorySuspected = &v
	}
	if len(p) > base+1 {
		v := int(p[base+1])
		m.HistoryComplete = &v
	}
	return m, nil
}

// ParseRealtime decodes a realtime-weight notification (26 or 38 bytes).
func ParseRealtime(p []byte) (*Measurement, error) {
	return parseWeight(p, []int{26, 38}, false)
}

// ParseHistory decodes a stored history record (27/28/39/40 bytes).
func ParseHistory(p []byte) (*Measurement, error) {
	return parseWeight(p, []int{27, 28, 39, 40}, true)
}

// CurrentTimePayload encodes t for the current-time characteristic.
func CurrentTimePayload(t time.Time) []byte {
	dow := (int(t.Weekday())+6)%7 + 1 // Monday=1 .. Sunday=7
	return []byte{
		byte(t.Year()), byte(t.Year() >> 8), byte(t.Month()), byte(t.Day()),
		byte(t.Hour()), byte(t.Minute()), byte(t.Second()), byte(dow),
	}
}

// UserInfo is the user profile written (encrypted) to the scale.
type UserInfo struct {
	HUID     string
	UID      string
	Gender   int // Scale 3: male=1, female=0
	Age      int
	HeightCm int
	WeightKg float64
	UserType int
}

// Payload encodes the fixed 69-byte user-info record.
func (u UserInfo) Payload() []byte {
	p := make([]byte, 69)
	copy(p[0:30], u.HUID)
	copy(p[30:62], u.UID)
	p[62] = byte(clamp(u.Gender, 0, 255))
	p[63] = byte(clamp(u.Age, 0, 255))
	binary.LittleEndian.PutUint16(p[64:], uint16(clamp(u.HeightCm, 0, 0xFFFF)))
	w := 0
	if u.WeightKg > 0 {
		w = clamp(int(u.WeightKg*100+0.5), 0, 0xFFFF)
	}
	binary.LittleEndian.PutUint16(p[66:], uint16(w))
	p[68] = byte(clamp(u.UserType, 0, 255))
	return p
}

func clamp(v, lo, hi int) int { return max(lo, min(hi, v)) }

// ManagerInfo is the account binding stored on the scale.
type ManagerInfo struct {
	HUID, DeviceID, Account string
}

// ParseManagerInfo decodes the manager-info response.
func ParseManagerInfo(p []byte) (*ManagerInfo, error) {
	if len(p) == 0 {
		return &ManagerInfo{}, nil
	}
	if len(p) <= 30 {
		return nil, fmt.Errorf("manager-info too short (%d bytes)", len(p))
	}
	mi := &ManagerInfo{HUID: fixedText(p[:30])}
	end := min(len(p), 70)
	mi.DeviceID = fixedText(p[30:end])
	if len(p) > 70 {
		n := int(p[70])
		e := min(len(p), 71+n)
		if e > 71 {
			mi.Account = fixedText(p[71:e])
		}
	}
	return mi, nil
}

// ParseScaleVersion decodes the firmware version response.
func ParseScaleVersion(p []byte) (version, serial string, err error) {
	if len(p) < 5 || len(p) > 20 {
		return "", "", fmt.Errorf("unexpected scale-version length %d", len(p))
	}
	bits := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
	major := bits >> 20 & 0xF
	minor := bits >> 16 & 0xF
	patch := bits >> 12 & 0xF
	build := bits >> 2 & 0x3FF
	return fmt.Sprintf("%d.%d.%d.%d", major, minor, patch, build), fixedText(p[4:]), nil
}

// ParseWeightUnit decodes the configured display unit.
func ParseWeightUnit(p []byte) string {
	if len(p) == 0 {
		return ""
	}
	// The unit is BCD-ish: the hex representation of the byte is the code.
	switch fmt.Sprintf("%x", p[0]) {
	case "1":
		return "kg"
	case "2":
		return "lb"
	case "3":
		return "g"
	}
	return fmt.Sprintf("unknown(0x%02x)", p[0])
}

// ParseProductInfo returns the product info text (first 4 chars = product id).
func ParseProductInfo(p []byte) string { return fixedText(p) }

func fixedText(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	s := strings.TrimRight(string(b), " ")
	for _, c := range []byte(s) {
		if c < 0x20 || c > 0x7E {
			return "hex:" + strings.ToUpper(hex.EncodeToString([]byte(s)))
		}
	}
	return s
}
