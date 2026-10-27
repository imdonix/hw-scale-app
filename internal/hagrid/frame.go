// Package hagrid implements the Huawei "Hagrid" WSP BLE protocol used by the
// Huawei Scale 3 (and related Huawei/Honor scales): framing, CRCs,
// authentication, AES-CTR session encryption and payload parsing.
//
// The protocol description follows openScale's HuaweiHagridWspLib
// (https://github.com/oliexdev/openScale).
package hagrid

import (
	"errors"
	"fmt"
)

// Frame prefixes.
const (
	FrameWritePlain     = 0xDB
	FrameWriteEncrypted = 0xDC
	FrameNotifyPlain    = 0xBD
	FrameNotifyEnc      = 0xCD

	maxPayloadBytes = 240
	chunkBytes      = 15
)

var crcTable = [256]uint16{
	0, 4129, 8258, 12387, 16516, 20645, 24774, 28903, 33032, 37161, 41290, 45419, 49548, 53677, 57806, 61935,
	4657, 528, 12915, 8786, 21173, 17044, 29431, 25302, 37689, 33560, 45947, 41818, 54205, 50076, 62463, 58334,
	9314, 13379, 1056, 5121, 25830, 29895, 17572, 21637, 42346, 46411, 34088, 38153, 58862, 62927, 50604, 54669,
	13907, 9842, 5649, 1584, 30423, 26358, 22165, 18100, 46939, 42874, 38681, 34616, 63455, 59390, 55197, 51132,
	18628, 22757, 26758, 30887, 2112, 6241, 10242, 14371, 51660, 55789, 59790, 63919, 35144, 39273, 43274, 47403,
	23285, 19156, 31415, 27286, 6769, 2640, 14899, 10770, 56317, 52188, 64447, 60318, 39801, 35672, 47931, 43802,
	27814, 31879, 19684, 23749, 11298, 15363, 3168, 7233, 60846, 64911, 52716, 56781, 44330, 48395, 36200, 40265,
	32407, 28342, 24277, 20212, 15891, 11826, 7761, 3696, 65439, 61374, 57309, 53244, 48923, 44858, 40793, 36728,
	37256, 33193, 45514, 41451, 53516, 49453, 61774, 57711, 4224, 97, 12482, 8419, 20484, 16421, 28742, 24679,
	33721, 37784, 41979, 46042, 49981, 54044, 58239, 62302, 689, 4752, 8947, 13010, 16949, 21012, 25207, 29270,
	46570, 42443, 38312, 34185, 62830, 58703, 54572, 50445, 13538, 9411, 5280, 1153, 29798, 25671, 21540, 17413,
	42971, 47098, 34713, 38840, 59231, 63358, 50973, 55100, 9939, 14066, 1681, 5808, 26199, 30326, 17941, 22068,
	55628, 51565, 63758, 59695, 39368, 35305, 47498, 43435, 22596, 18533, 30726, 26663, 6336, 2273, 14466, 10403,
	52093, 56156, 60223, 64286, 35833, 39896, 43963, 48026, 19061, 23124, 27191, 31254, 2801, 6864, 10931, 14994,
	64814, 60687, 56684, 52557, 48554, 44427, 40424, 36297, 31782, 27655, 23652, 19525, 15522, 11395, 7392, 3265,
	61215, 65342, 53085, 57212, 44955, 49082, 36825, 40952, 28183, 32310, 20053, 24180, 11923, 16050, 3793, 7920,
}

// CRC16 is the CRC-16/XMODEM variant used for frames written to the scale.
func CRC16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc = (crc << 8) ^ crcTable[(b^byte(crc>>8))&0xFF]
	}
	return crc
}

// CRC16Modbus is the CRC used by frames notified by the scale.
func CRC16Modbus(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// BuildWriteFrames splits payload into 15-byte WSP write frames.
func BuildWriteFrames(payload []byte, encrypted bool) ([][]byte, error) {
	if len(payload) > maxPayloadBytes {
		return nil, fmt.Errorf("payload of %d bytes exceeds WSP limit of %d", len(payload), maxPayloadBytes)
	}
	total := (len(payload) + chunkBytes - 1) / chunkBytes
	if total == 0 {
		total = 1
	}
	prefix := byte(FrameWritePlain)
	if encrypted {
		prefix = FrameWriteEncrypted
	}
	frames := make([][]byte, 0, total)
	off := 0
	for i := 0; i < total; i++ {
		n := min(chunkBytes, len(payload)-off)
		f := make([]byte, n+5)
		f[0] = prefix
		f[1] = byte(n + 3)
		f[2] = byte(((total-1)&0x0F)<<4 | (i & 0x0F))
		copy(f[3:], payload[off:off+n])
		off += n
		// The CRC is computed over the whole frame with the CRC bytes zeroed.
		crc := CRC16(f)
		f[len(f)-2] = byte(crc)
		f[len(f)-1] = byte(crc >> 8)
		frames = append(frames, f)
	}
	return frames, nil
}

// Frame is one parsed notification frame.
type Frame struct {
	Encrypted bool
	Total     int
	Index     int
	Payload   []byte
}

var (
	ErrNotWSP   = errors.New("not a WSP notification frame")
	ErrBadCRC   = errors.New("WSP frame CRC mismatch")
	ErrBadFrame = errors.New("malformed WSP frame")
)

// IsNotifyFrame reports whether data starts with a WSP notification prefix.
func IsNotifyFrame(data []byte) bool {
	return len(data) > 0 && (data[0] == FrameNotifyPlain || data[0] == FrameNotifyEnc)
}

// ParseNotifyFrame parses and CRC-checks one notification frame.
func ParseNotifyFrame(data []byte) (Frame, error) {
	if len(data) < 5 {
		return Frame{}, ErrBadFrame
	}
	if !IsNotifyFrame(data) {
		return Frame{}, ErrNotWSP
	}
	want := uint16(data[len(data)-2]) | uint16(data[len(data)-1])<<8
	if CRC16Modbus(data[:len(data)-2]) != want {
		return Frame{}, ErrBadCRC
	}
	n := int(data[1]) - 3
	if n < 0 || len(data) < n+5 {
		return Frame{}, ErrBadFrame
	}
	total := int(data[2]>>4) + 1
	idx := int(data[2] & 0x0F)
	if idx >= total {
		return Frame{}, ErrBadFrame
	}
	return Frame{
		Encrypted: data[0] == FrameNotifyEnc,
		Total:     total,
		Index:     idx,
		Payload:   append([]byte(nil), data[3:3+n]...),
	}, nil
}

// Accumulator reassembles multi-frame notifications for one characteristic.
// It is not safe for concurrent use.
type Accumulator struct {
	expected  int
	encrypted bool
	chunks    map[int][]byte
}

// Ingest feeds one raw notification. It returns the complete logical payload
// and whether it was encrypted once all frames of a packet have arrived.
func (a *Accumulator) Ingest(raw []byte) (payload []byte, encrypted bool, done bool, err error) {
	f, err := ParseNotifyFrame(raw)
	if err != nil {
		return nil, false, false, err
	}
	if f.Total <= 1 {
		a.reset()
		return f.Payload, f.Encrypted, true, nil
	}
	if f.Index == 0 {
		a.reset()
		a.expected = f.Total
		a.encrypted = f.Encrypted
	} else {
		if a.expected == 0 {
			return nil, false, false, nil // orphan continuation frame
		}
		if a.expected != f.Total || a.encrypted != f.Encrypted {
			a.reset()
			return nil, false, false, ErrBadFrame
		}
	}
	if a.chunks == nil {
		a.chunks = make(map[int][]byte)
	}
	a.chunks[f.Index] = f.Payload
	if len(a.chunks) < a.expected {
		return nil, false, false, nil
	}
	var out []byte
	for i := 0; i < a.expected; i++ {
		c, ok := a.chunks[i]
		if !ok {
			a.reset()
			return nil, false, false, ErrBadFrame
		}
		out = append(out, c...)
	}
	enc := a.encrypted
	a.reset()
	return out, enc, true, nil
}

func (a *Accumulator) reset() {
	a.expected = 0
	a.encrypted = false
	a.chunks = nil
}
