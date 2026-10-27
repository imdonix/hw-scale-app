// Package db stores measurements and settings in SQLite.
package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, keeps the binary static

	"github.com/tamasmagyar/sscale/internal/trend"
)

// FileName is the database file inside the data directory.
const FileName = "sscale.db"

// Measurement is one stored reading (raw values; derived values are
// computed from the current settings when served).
type Measurement struct {
	ID              int64      `json:"id"`
	Time            time.Time  `json:"time"`
	ScaleTime       *time.Time `json:"scale_time,omitempty"`
	ReceivedAt      time.Time  `json:"received_at"`
	Source          string     `json:"source"` // live | latest | history
	WeightKg        float64    `json:"weight_kg"`
	ScaleFatPercent float64    `json:"scale_fat_percent,omitempty"`
	HeartRateBPM    int        `json:"heart_rate_bpm,omitempty"`
	ImpedanceRaw    []int      `json:"impedance_raw,omitempty"`
	HighImpedance   []int      `json:"high_impedance_raw,omitempty"`
	Fingerprint     string     `json:"-"`
	AutoValid       bool       `json:"auto_valid"`
	FilterReason    string     `json:"filter_reason,omitempty"`
	ManualValid     *bool      `json:"manual_valid"`
}

// Valid is the owner's decision if any, otherwise the filter's.
func (m *Measurement) Valid() bool {
	if m.ManualValid != nil {
		return *m.ManualValid
	}
	return m.AutoValid
}

// Settings are the user-editable settings.
type Settings struct {
	HeightCm  float64        `json:"height_cm"`
	Birthdate string         `json:"birthdate"` // YYYY-MM-DD
	Sex       string         `json:"sex"`       // male | female
	Filter    FilterSettings `json:"filter"`
}

// FilterSettings configures the trend filter.
type FilterSettings struct {
	Enabled      bool    `json:"enabled"`
	ToleranceKg  float64 `json:"tolerance_kg"`
	TolerancePct float64 `json:"tolerance_pct"`
}

// Params converts to trend parameters.
func (f FilterSettings) Params() trend.Params {
	return trend.Params{Enabled: f.Enabled, ToleranceKg: f.ToleranceKg, TolerancePct: f.TolerancePct}
}

// Validate checks the settings.
func (s Settings) Validate() error {
	if s.HeightCm != 0 && (s.HeightCm < 100 || s.HeightCm > 250) {
		return errors.New("height must be between 100 and 250 cm")
	}
	if s.Birthdate != "" {
		t, err := time.Parse("2006-01-02", s.Birthdate)
		if err != nil {
			return errors.New("birthdate must be YYYY-MM-DD")
		}
		if t.After(time.Now()) || t.Year() < 1900 {
			return errors.New("birthdate is not plausible")
		}
	}
	if s.Sex != "" && s.Sex != "male" && s.Sex != "female" {
		return errors.New("sex must be male or female")
	}
	if s.Filter.ToleranceKg < 0.5 || s.Filter.ToleranceKg > 30 {
		return errors.New("filter tolerance must be between 0.5 and 30 kg")
	}
	if s.Filter.TolerancePct < 0 || s.Filter.TolerancePct > 30 {
		return errors.New("filter tolerance must be between 0 and 30 %")
	}
	return nil
}

// DB is the sscale database.
type DB struct{ sql *sql.DB }

// ErrNotFound is returned for unknown ids.
var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS measurements (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	time              INTEGER NOT NULL,          -- unix ms
	scale_time        INTEGER,
	received_at       INTEGER NOT NULL,
	source            TEXT    NOT NULL,
	weight_kg         REAL    NOT NULL,
	scale_fat_percent REAL    NOT NULL DEFAULT 0,
	heart_rate_bpm    INTEGER NOT NULL DEFAULT 0,
	impedance_raw     TEXT    NOT NULL DEFAULT '[]',
	high_impedance    TEXT    NOT NULL DEFAULT '[]',
	fingerprint       TEXT    NOT NULL UNIQUE,
	auto_valid        INTEGER NOT NULL DEFAULT 1,
	filter_reason     TEXT    NOT NULL DEFAULT '',
	manual_valid      INTEGER
);
CREATE INDEX IF NOT EXISTS measurements_time ON measurements(time);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`

// Open opens (and migrates) the database in dir.
func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(dir, FileName) +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	s, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := s.Exec(schema); err != nil {
		s.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return &DB{sql: s}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).In(time.Local) }

func ints(s string) []int {
	var v []int
	_ = json.Unmarshal([]byte(s), &v)
	return v
}

func intsJSON(v []int) string {
	if v == nil {
		return "[]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

const cols = `id, time, scale_time, received_at, source, weight_kg, scale_fat_percent, heart_rate_bpm,
	impedance_raw, high_impedance, fingerprint, auto_valid, filter_reason, manual_valid`

func scan(row interface{ Scan(...any) error }) (Measurement, error) {
	var m Measurement
	var t, recv int64
	var st, manual sql.NullInt64
	var imp, high string
	err := row.Scan(&m.ID, &t, &st, &recv, &m.Source, &m.WeightKg, &m.ScaleFatPercent, &m.HeartRateBPM,
		&imp, &high, &m.Fingerprint, &m.AutoValid, &m.FilterReason, &manual)
	if err != nil {
		return m, err
	}
	m.Time, m.ReceivedAt = fromMS(t), fromMS(recv)
	if st.Valid {
		v := fromMS(st.Int64)
		m.ScaleTime = &v
	}
	if manual.Valid {
		v := manual.Int64 != 0
		m.ManualValid = &v
	}
	m.ImpedanceRaw, m.HighImpedance = ints(imp), ints(high)
	return m, nil
}

// IsDuplicate reports whether m was already stored: same raw record, or the
// same weight and impedance within 10 minutes.
func (d *DB) IsDuplicate(ctx context.Context, m *Measurement) (bool, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM measurements WHERE fingerprint = ?`, m.Fingerprint).Scan(&n)
	if err != nil || n > 0 {
		return n > 0, err
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT `+cols+` FROM measurements WHERE time BETWEEN ? AND ?`,
		ms(m.Time.Add(-10*time.Minute)), ms(m.Time.Add(10*time.Minute)))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		o, err := scan(rows)
		if err != nil {
			return false, err
		}
		if int(o.WeightKg*100+0.5) == int(m.WeightKg*100+0.5) && len(o.ImpedanceRaw) > 0 && len(m.ImpedanceRaw) > 0 &&
			o.ImpedanceRaw[0] == m.ImpedanceRaw[0] {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Insert stores m (ignoring an exact duplicate) and sets m.ID.
func (d *DB) Insert(ctx context.Context, m *Measurement) (bool, error) {
	var st any
	if m.ScaleTime != nil {
		st = ms(*m.ScaleTime)
	}
	r, err := d.sql.ExecContext(ctx, `INSERT INTO measurements
		(time, scale_time, received_at, source, weight_kg, scale_fat_percent, heart_rate_bpm,
		 impedance_raw, high_impedance, fingerprint, auto_valid, filter_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, '') ON CONFLICT(fingerprint) DO NOTHING`,
		ms(m.Time), st, ms(m.ReceivedAt), m.Source, m.WeightKg, m.ScaleFatPercent, m.HeartRateBPM,
		intsJSON(m.ImpedanceRaw), intsJSON(m.HighImpedance), m.Fingerprint)
	if err != nil {
		return false, err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return false, nil
	}
	m.ID, _ = r.LastInsertId()
	m.AutoValid = true
	return true, nil
}

// List returns measurements with from <= time < to, oldest first. Zero
// bounds are open.
func (d *DB) List(ctx context.Context, from, to time.Time) ([]Measurement, error) {
	q := `SELECT ` + cols + ` FROM measurements WHERE 1=1`
	var args []any
	if !from.IsZero() {
		q += ` AND time >= ?`
		args = append(args, ms(from))
	}
	if !to.IsZero() {
		q += ` AND time < ?`
		args = append(args, ms(to))
	}
	rows, err := d.sql.QueryContext(ctx, q+` ORDER BY time, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Measurement
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Get returns one measurement.
func (d *DB) Get(ctx context.Context, id int64) (Measurement, error) {
	m, err := scan(d.sql.QueryRowContext(ctx, `SELECT `+cols+` FROM measurements WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// Latest returns the newest valid measurement, if any.
func (d *DB) Latest(ctx context.Context) (*Measurement, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+cols+` FROM measurements ORDER BY time DESC, id DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, err
		}
		if m.Valid() {
			return &m, nil
		}
	}
	return nil, rows.Err()
}

// Count returns the number of stored and valid measurements.
func (d *DB) Count(ctx context.Context) (total, valid int, err error) {
	err = d.sql.QueryRowContext(ctx, `SELECT count(*),
		coalesce(sum(CASE WHEN coalesce(manual_valid, auto_valid) != 0 THEN 1 ELSE 0 END), 0) FROM measurements`).
		Scan(&total, &valid)
	return
}

// SetManual records the owner's decision (nil = let the filter decide).
func (d *DB) SetManual(ctx context.Context, id int64, v *bool) error {
	var arg any
	if v != nil {
		arg = *v
	}
	r, err := d.sql.ExecContext(ctx, `UPDATE measurements SET manual_valid = ? WHERE id = ?`, arg, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a measurement.
func (d *DB) Delete(ctx context.Context, id int64) error {
	r, err := d.sql.ExecContext(ctx, `DELETE FROM measurements WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Reclassify re-runs the trend filter over all measurements and stores the
// results. It returns the number of readings whose validity changed.
func (d *DB) Reclassify(ctx context.Context, p trend.Params) (int, error) {
	all, err := d.List(ctx, time.Time{}, time.Time{})
	if err != nil {
		return 0, err
	}
	pts := make([]trend.Point, len(all))
	prev := map[int64]Measurement{}
	for i, m := range all {
		pts[i] = trend.Point{ID: m.ID, Time: m.Time, Weight: m.WeightKg, Manual: m.ManualValid}
		prev[m.ID] = m
	}
	results := trend.Classify(pts, p)
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	changed := 0
	for _, r := range results {
		old := prev[r.ID]
		if old.AutoValid == r.Valid && old.FilterReason == r.Reason {
			continue
		}
		if old.AutoValid != r.Valid {
			changed++
		}
		if _, err := tx.ExecContext(ctx, `UPDATE measurements SET auto_valid = ?, filter_reason = ? WHERE id = ?`,
			r.Valid, r.Reason, r.ID); err != nil {
			return 0, err
		}
	}
	return changed, tx.Commit()
}

// ---------------------------------------------------------------- settings

func (d *DB) getKV(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (d *DB) setKV(ctx context.Context, key, value string) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Settings returns the stored settings, or def if none were saved yet.
func (d *DB) Settings(ctx context.Context, def Settings) (Settings, error) {
	v, ok, err := d.getKV(ctx, "settings")
	if err != nil || !ok {
		return def, err
	}
	s := def
	if err := json.Unmarshal([]byte(v), &s); err != nil {
		return def, fmt.Errorf("stored settings: %w", err)
	}
	return s, nil
}

// SaveSettings stores the settings.
func (d *DB) SaveSettings(ctx context.Context, s Settings) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return d.setKV(ctx, "settings", string(b))
}

// HasSettings reports whether settings were saved.
func (d *DB) HasSettings(ctx context.Context) (bool, error) {
	_, ok, err := d.getKV(ctx, "settings")
	return ok, err
}

// APIKey returns the API key, generating and storing one if needed.
// created reports whether it was generated now.
func (d *DB) APIKey(ctx context.Context) (key string, created bool, err error) {
	v, ok, err := d.getKV(ctx, "api_key")
	if err != nil || ok {
		return v, false, err
	}
	key, err = d.RotateAPIKey(ctx)
	return key, err == nil, err
}

// RotateAPIKey replaces the API key with a fresh random one.
func (d *DB) RotateAPIKey(ctx context.Context) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	key := "ssk_" + base64.RawURLEncoding.EncodeToString(b)
	return key, d.setKV(ctx, "api_key", key)
}
