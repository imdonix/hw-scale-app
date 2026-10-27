// Package config loads keys.json (scale secrets and user profile) and the
// runtime settings.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tamasmagyar/sscale/internal/hagrid"
)

// Keys mirrors keys.json.
type Keys struct {
	Address   string  `json:"address"`
	Model     string  `json:"model"`
	CAK       string  `json:"cak"`
	C1        string  `json:"c1"`
	C2        string  `json:"c2"`
	HeightCm  float64 `json:"height_cm"`
	Birthdate string  `json:"birthdate"`
	Sex       string  `json:"sex"`
	WeightKg  float64 `json:"weight_kg,omitempty"` // optional initial weight
	UserID    int     `json:"user_id,omitempty"`   // optional scale user slot id
}

// Profile is the user the measurements belong to.
type Profile struct {
	HeightCm  float64
	Birthdate time.Time
	HasBirth  bool
	Male      bool
	HasSex    bool
	WeightKg  float64
	UserID    int
}

// Age returns the age in whole years at t.
func (p Profile) Age(t time.Time) int {
	if !p.HasBirth {
		return 0
	}
	b := p.Birthdate
	y := t.Year() - b.Year()
	if t.Month() < b.Month() || (t.Month() == b.Month() && t.Day() < b.Day()) {
		y--
	}
	return y
}

// Config is the full runtime configuration.
type Config struct {
	KeysPath string
	DataDir  string
	Adapter  string
	LogLevel string
	Trace    bool

	Address string
	Model   string
	Secrets hagrid.Secrets
	Profile Profile

	// ScanWindow is how long one discovery window lasts. ScanPause is the
	// pause between windows (0 = scan continuously, recommended).
	ScanWindow time.Duration
	ScanPause  time.Duration
	// QuietPeriod: after a session the daemon waits until the scale has not
	// advertised for this long (i.e. it went to sleep) before connecting
	// again, so it never keeps the scale awake.
	QuietPeriod time.Duration
	// MaxCooldown bounds that wait in case the scale never stops advertising.
	MaxCooldown time.Duration
	// IdleGrace: when connected to an idle scale, how long to wait for a
	// weighing to start before reading history and disconnecting.
	IdleGrace time.Duration
	// SessionTimeout bounds one connection.
	SessionTimeout time.Duration

	// HTTPAddr is the REST API / web app listen address ("" disables it).
	HTTPAddr string
	// TLSCert/TLSKey enable HTTPS (needed for the PWA service worker
	// when not behind an HTTPS reverse proxy).
	TLSCert, TLSKey string
	// APIKey overrides the generated API key.
	APIKey string
}

// Defaults returns the default settings, overridden by SSCALE_* env vars.
func Defaults() Config {
	c := Config{
		KeysPath:       envStr("SSCALE_KEYS", "keys.json"),
		DataDir:        envStr("SSCALE_DATA", "data"),
		Adapter:        envStr("SSCALE_ADAPTER", ""),
		LogLevel:       envStr("SSCALE_LOG_LEVEL", "info"),
		Trace:          envBool("SSCALE_TRACE"),
		ScanWindow:     envDur("SSCALE_SCAN_WINDOW", 30*time.Second),
		ScanPause:      envDur("SSCALE_SCAN_PAUSE", 0),
		QuietPeriod:    envDur("SSCALE_QUIET_PERIOD", 60*time.Second),
		MaxCooldown:    envDur("SSCALE_MAX_COOLDOWN", 15*time.Minute),
		IdleGrace:      envDur("SSCALE_IDLE_GRACE", 25*time.Second),
		SessionTimeout: envDur("SSCALE_SESSION_TIMEOUT", 3*time.Minute),
		HTTPAddr:       envStrAllowEmpty("SSCALE_HTTP", ":8080"),
		TLSCert:        os.Getenv("SSCALE_TLS_CERT"),
		TLSKey:         os.Getenv("SSCALE_TLS_KEY"),
		APIKey:         os.Getenv("SSCALE_API_KEY"),
	}
	return c
}

// LoadKeys reads keys.json into c.
func (c *Config) LoadKeys() error {
	b, err := os.ReadFile(c.KeysPath)
	if err != nil {
		return fmt.Errorf("read keys file: %w", err)
	}
	var k Keys
	if err := json.Unmarshal(b, &k); err != nil {
		return fmt.Errorf("parse %s: %w", c.KeysPath, err)
	}
	if c.Address == "" {
		c.Address = strings.ToUpper(strings.TrimSpace(k.Address))
	}
	c.Model = k.Model
	if c.Secrets, err = hagrid.ParseSecrets(k.CAK, k.C1, k.C2); err != nil {
		return fmt.Errorf("%s: %w", c.KeysPath, err)
	}
	c.Profile = Profile{HeightCm: k.HeightCm, WeightKg: k.WeightKg, UserID: k.UserID}
	if c.Profile.UserID == 0 {
		c.Profile.UserID = 1
	}
	if k.Birthdate != "" {
		t, err := time.ParseInLocation("2006-01-02", k.Birthdate, time.Local)
		if err != nil {
			return fmt.Errorf("%s: birthdate must be YYYY-MM-DD: %w", c.KeysPath, err)
		}
		c.Profile.Birthdate, c.Profile.HasBirth = t, true
	}
	switch strings.ToLower(strings.TrimSpace(k.Sex)) {
	case "male", "m":
		c.Profile.Male, c.Profile.HasSex = true, true
	case "female", "f":
		c.Profile.Male, c.Profile.HasSex = false, true
	case "":
	default:
		return fmt.Errorf("%s: sex must be male or female", c.KeysPath)
	}
	return nil
}

// Validate checks that a target scale is configured.
func (c *Config) Validate() error {
	if c.Address == "" {
		return fmt.Errorf("no scale address: set \"address\" in %s or pass --address", c.KeysPath)
	}
	if _, err := hagrid.C3FromAddress(c.Address); err != nil {
		return err
	}
	return nil
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envStrAllowEmpty(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func envBool(k string) bool {
	b, _ := strconv.ParseBool(os.Getenv(k))
	return b
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
