package hagrid

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var (
	authSuffixToScale   = []byte("1123")
	authSuffixFromScale = []byte("9856")
)

// Secrets holds the per-scale key material (CAK, C1, C2), 16 bytes each.
type Secrets struct {
	CAK, C1, C2 []byte
}

// ParseSecrets decodes hex-encoded CAK/C1/C2 values.
func ParseSecrets(cak, c1, c2 string) (Secrets, error) {
	var s Secrets
	var err error
	if s.CAK, err = parseKey("cak", cak); err != nil {
		return s, err
	}
	if s.C1, err = parseKey("c1", c1); err != nil {
		return s, err
	}
	if s.C2, err = parseKey("c2", c2); err != nil {
		return s, err
	}
	return s, nil
}

func parseKey(name, v string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(v))
	if err != nil {
		return nil, fmt.Errorf("%s: invalid hex: %w", name, err)
	}
	if len(b) != 16 {
		return nil, fmt.Errorf("%s: expected 16 bytes (32 hex digits), got %d bytes", name, len(b))
	}
	return b, nil
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func authToken(randA, randB, cak, suffix []byte) []byte {
	rand := append(append([]byte{}, randA...), randB...)
	inner := hmacSHA256(append(append([]byte{}, cak...), suffix...), rand)
	return hmacSHA256(inner, rand)
}

// AuthTokenPayload is written to the auth-token characteristic in response
// to the scale's 16-byte challenge randA: randB || token.
func AuthTokenPayload(randA, randB, cak []byte) []byte {
	return append(append([]byte{}, randB...), authToken(randA, randB, cak, authSuffixToScale)...)
}

// ExpectedAuthResponse is the token the scale must answer with.
func ExpectedAuthResponse(randA, randB, cak []byte) []byte {
	return authToken(randA, randB, cak, authSuffixFromScale)
}

// ValidAuthResponse checks the scale's auth response. Some firmwares append a
// status byte, so only the prefix is compared.
func ValidAuthResponse(resp, randA, randB, cak []byte) bool {
	want := ExpectedAuthResponse(randA, randB, cak)
	return len(resp) >= len(want) && hmac.Equal(resp[:len(want)], want)
}

// C3FromAddress derives the C3 key component from the scale's BT address.
func C3FromAddress(addr string) ([]byte, error) {
	compact := strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(addr))
	if len(compact) != 12 {
		return nil, fmt.Errorf("bluetooth address %q must contain 12 hex digits", addr)
	}
	if _, err := hex.DecodeString(compact); err != nil {
		return nil, fmt.Errorf("bluetooth address %q is not hex", addr)
	}
	return []byte(compact + "0000"), nil
}

// DeriveRootKey derives the root key used to wrap the session work key.
func DeriveRootKey(c1, c2, c3 []byte) ([]byte, error) {
	if len(c1) != 16 || len(c2) != 16 || len(c3) != 16 {
		return nil, errors.New("c1, c2 and c3 must be 16 bytes")
	}
	mixed := make([]byte, 16)
	for i := range mixed {
		mixed[i] = (c1[i] << 4) ^ c2[i]
	}
	kd := sha256.Sum256(mixed)
	folded := make([]byte, 16)
	for i := range folded {
		folded[i] = (kd[i] >> 6) ^ c3[i]
	}
	root := sha256.Sum256(folded)
	return root[:16], nil
}

func aesCTR(key, iv, in []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(in))
	cipher.NewCTR(block, iv).XORKeyStream(out, in)
	return out, nil
}

// Encrypt returns iv || AES-CTR(key, iv, plaintext).
func Encrypt(plaintext, key, iv []byte) ([]byte, error) {
	if len(key) != 16 || len(iv) != 16 {
		return nil, errors.New("AES-CTR key and IV must be 16 bytes")
	}
	ct, err := aesCTR(key, iv, plaintext)
	if err != nil {
		return nil, err
	}
	return append(append([]byte{}, iv...), ct...), nil
}

// Decrypt reverses Encrypt.
func Decrypt(ivAndCiphertext, key []byte) ([]byte, error) {
	if len(ivAndCiphertext) < 16 {
		return nil, errors.New("encrypted payload must include a 16-byte IV")
	}
	if len(key) != 16 {
		return nil, errors.New("AES-CTR key must be 16 bytes")
	}
	return aesCTR(key, ivAndCiphertext[:16], ivAndCiphertext[16:])
}

// BuildEncryptedWriteFrames encrypts plaintext and frames it.
func BuildEncryptedWriteFrames(plaintext, key, iv []byte) ([][]byte, error) {
	enc, err := Encrypt(plaintext, key, iv)
	if err != nil {
		return nil, err
	}
	return BuildWriteFrames(enc, true)
}
