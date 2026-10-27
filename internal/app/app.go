// Package app ties the database, settings and trend filter together. It is
// used by the BLE daemon, the REST API and the CLI.
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tamasmagyar/sscale/internal/config"
	"github.com/tamasmagyar/sscale/internal/db"
	"github.com/tamasmagyar/sscale/internal/hagrid"
	"github.com/tamasmagyar/sscale/internal/trend"
)

// App is the measurement service.
type App struct {
	DB     *db.DB
	Log    *slog.Logger
	Scale3 bool // apply the Scale 3 impedance unit and composition model

	defaults db.Settings
	mu       sync.Mutex // serialises writes + reclassification
}

// Open opens the database in cfg.DataDir, seeds settings from keys.json and
// imports a legacy measurements.jsonl.
func Open(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	d, err := db.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	model := strings.ToUpper(cfg.Model)
	a := &App{
		DB:       d,
		Log:      log,
		Scale3:   model == "" || (strings.Contains(model, "SCALE 3") && !strings.Contains(model, "PRO")),
		defaults: DefaultSettings(cfg.Profile),
	}
	if has, err := d.HasSettings(ctx); err == nil && !has {
		if err := d.SaveSettings(ctx, a.defaults); err != nil {
			d.Close()
			return nil, err
		}
	}
	if err := a.importJSONL(ctx, cfg.DataDir); err != nil {
		log.Warn("importing measurements.jsonl failed", "err", err)
	}
	return a, nil
}

// Close closes the database.
func (a *App) Close() error { return a.DB.Close() }

// DefaultSettings derives initial settings from the keys.json profile.
func DefaultSettings(p config.Profile) db.Settings {
	s := db.Settings{HeightCm: p.HeightCm, Filter: db.FilterSettings{
		Enabled:      trend.DefaultParams.Enabled,
		ToleranceKg:  trend.DefaultParams.ToleranceKg,
		TolerancePct: trend.DefaultParams.TolerancePct,
	}}
	if p.HasBirth {
		s.Birthdate = p.Birthdate.Format("2006-01-02")
	}
	if p.HasSex {
		s.Sex = "female"
		if p.Male {
			s.Sex = "male"
		}
	}
	return s
}

// Settings returns the current settings.
func (a *App) Settings(ctx context.Context) db.Settings {
	s, err := a.DB.Settings(ctx, a.defaults)
	if err != nil {
		a.Log.Warn("reading settings", "err", err)
	}
	return s
}

// UpdateSettings validates, saves and re-runs the filter.
func (a *App) UpdateSettings(ctx context.Context, s db.Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.DB.SaveSettings(ctx, s); err != nil {
		return err
	}
	_, err := a.DB.Reclassify(ctx, s.Filter.Params())
	return err
}

// Profile returns the user profile from the settings, with lastWeight as
// the weight hint for the scale.
func (a *App) Profile(ctx context.Context) config.Profile {
	return ProfileFrom(a.Settings(ctx))
}

// ProfileFrom converts settings to a profile.
func ProfileFrom(s db.Settings) config.Profile {
	p := config.Profile{HeightCm: s.HeightCm, UserID: 1}
	if t, err := time.ParseInLocation("2006-01-02", s.Birthdate, time.Local); err == nil {
		p.Birthdate, p.HasBirth = t, true
	}
	switch s.Sex {
	case "male":
		p.Male, p.HasSex = true, true
	case "female":
		p.HasSex = true
	}
	return p
}

// View is a measurement with derived values, as served by the API.
type View struct {
	db.Measurement
	Valid        bool                `json:"valid"`
	BMI          float64             `json:"bmi,omitempty"`
	ImpedanceOhm float64             `json:"impedance_ohm,omitempty"`
	Composition  *hagrid.Composition `json:"composition,omitempty"`
}

// ViewOf derives BMI, impedance and body composition using s.
func (a *App) ViewOf(m db.Measurement, s db.Settings) View {
	v := View{Measurement: m, Valid: m.Valid()}
	p := ProfileFrom(s)
	if p.HeightCm > 0 {
		h := p.HeightCm / 100
		v.BMI = round(m.WeightKg/(h*h), 1)
	}
	if len(m.ImpedanceRaw) == 0 || m.ImpedanceRaw[0] <= 0 {
		return v
	}
	z := m.ImpedanceRaw[0]
	switch {
	case a.Scale3:
		v.ImpedanceOhm = float64(z) / 10 // Scale 3 reports 0.1 Ω units
	case z < 4000:
		v.ImpedanceOhm = float64(z)
	case z < 40000:
		v.ImpedanceOhm = float64(z) / 10
	}
	if a.Scale3 && p.HasSex && p.HasBirth && v.ImpedanceOhm > 0 {
		if c := hagrid.CalculateComposition(p.HeightCm, m.WeightKg, p.Age(m.Time), p.Male, v.ImpedanceOhm); c != nil {
			roundComposition(c)
			v.Composition = c
		}
	}
	return v
}

// Add stores a new measurement unless it is a duplicate, re-runs the filter
// and returns the stored view.
func (a *App) Add(ctx context.Context, m *db.Measurement) (View, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if dup, err := a.DB.IsDuplicate(ctx, m); err != nil || dup {
		return View{}, false, err
	}
	ok, err := a.DB.Insert(ctx, m)
	if err != nil || !ok {
		return View{}, false, err
	}
	s := a.Settings(ctx)
	if _, err := a.DB.Reclassify(ctx, s.Filter.Params()); err != nil {
		return View{}, true, err
	}
	stored, err := a.DB.Get(ctx, m.ID)
	if err != nil {
		return View{}, true, err
	}
	return a.ViewOf(stored, s), true, nil
}

// SetManual records the owner's decision and re-runs the filter.
func (a *App) SetManual(ctx context.Context, id int64, v *bool) (View, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.DB.SetManual(ctx, id, v); err != nil {
		return View{}, err
	}
	s := a.Settings(ctx)
	if _, err := a.DB.Reclassify(ctx, s.Filter.Params()); err != nil {
		return View{}, err
	}
	m, err := a.DB.Get(ctx, id)
	if err != nil {
		return View{}, err
	}
	return a.ViewOf(m, s), nil
}

// Delete removes a measurement and re-runs the filter.
func (a *App) Delete(ctx context.Context, id int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.DB.Delete(ctx, id); err != nil {
		return err
	}
	_, err := a.DB.Reclassify(ctx, a.Settings(ctx).Filter.Params())
	return err
}

// List returns views for from <= time < to.
func (a *App) List(ctx context.Context, from, to time.Time) ([]View, error) {
	ms, err := a.DB.List(ctx, from, to)
	if err != nil {
		return nil, err
	}
	s := a.Settings(ctx)
	out := make([]View, 0, len(ms))
	for _, m := range ms {
		out = append(out, a.ViewOf(m, s))
	}
	return out, nil
}

// Latest returns the newest valid measurement view, or nil.
func (a *App) Latest(ctx context.Context) (*View, error) {
	m, err := a.DB.Latest(ctx)
	if err != nil || m == nil {
		return nil, err
	}
	v := a.ViewOf(*m, a.Settings(ctx))
	return &v, nil
}

// legacyRecord is the measurements.jsonl format of earlier versions.
type legacyRecord struct {
	Time            time.Time  `json:"time"`
	ScaleTime       *time.Time `json:"scale_time"`
	ReceivedAt      time.Time  `json:"received_at"`
	Source          string     `json:"source"`
	WeightKg        float64    `json:"weight_kg"`
	ScaleFatPercent float64    `json:"scale_fat_percent"`
	HeartRateBPM    int        `json:"heart_rate_bpm"`
	ImpedanceRaw    []int      `json:"impedance_raw"`
	HighImpedance   []int      `json:"high_impedance_raw"`
	Fingerprint     string     `json:"fingerprint"`
}

func (a *App) importJSONL(ctx context.Context, dir string) error {
	path := filepath.Join(dir, "measurements.jsonl")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var r legacyRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Fingerprint == "" || r.WeightKg <= 0 {
			continue
		}
		m := &db.Measurement{Time: r.Time, ScaleTime: r.ScaleTime, ReceivedAt: r.ReceivedAt, Source: r.Source,
			WeightKg: r.WeightKg, ScaleFatPercent: r.ScaleFatPercent, HeartRateBPM: r.HeartRateBPM,
			ImpedanceRaw: r.ImpedanceRaw, HighImpedance: r.HighImpedance, Fingerprint: r.Fingerprint}
		if _, ok, err := a.Add(ctx, m); err != nil {
			return err
		} else if ok {
			n++
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	f.Close()
	if err := os.Rename(path, path+".imported"); err != nil {
		return err
	}
	a.Log.Info("imported measurements from measurements.jsonl into the database", "records", n)
	return nil
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func roundComposition(c *hagrid.Composition) {
	c.BodyFatPercent = round(c.BodyFatPercent, 1)
	c.WaterPercent = round(c.WaterPercent, 1)
	c.SkeletalMuscleKg = round(c.SkeletalMuscleKg, 2)
	c.SkeletalMusclePct = round(c.SkeletalMusclePct, 1)
	c.BoneMineralKg = round(c.BoneMineralKg, 2)
	c.LeanBodyMassKg = round(c.LeanBodyMassKg, 2)
	c.ProteinPercent = round(c.ProteinPercent, 1)
}

// String formats a view for logs.
func (v View) String() string {
	return fmt.Sprintf("%s %.2fkg valid=%v", v.Time.Format(time.DateTime), v.WeightKg, v.Valid)
}
