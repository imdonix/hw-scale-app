// Package daemon contains the scan/connect policy and the session runner
// shared by the daemon and the one-shot CLI commands.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/tamasmagyar/sscale/internal/app"
	"github.com/tamasmagyar/sscale/internal/bluez"
	"github.com/tamasmagyar/sscale/internal/config"
	"github.com/tamasmagyar/sscale/internal/db"
	"github.com/tamasmagyar/sscale/internal/hagrid"
	"github.com/tamasmagyar/sscale/internal/scale"
	"github.com/tamasmagyar/sscale/internal/store"
)

// Runner performs connection sessions with the scale.
type Runner struct {
	Cfg   *config.Config
	Store *store.Store // daemon/pairing state (state.json)
	App   *app.App     // measurements and settings (SQLite)
	Log   *slog.Logger
	// Dial overrides how the link is established (tests); default BlueZ.
	Dial func(ctx context.Context) (scale.Transport, error)

	profileNoted bool
}

// Mode selects what a session does.
type Mode struct {
	ReadInfo bool // query product/firmware/unit
	Measure  bool // wait for the weighing and read history
}

// Outcome summarises a session.
type Outcome struct {
	Device     scale.DeviceInfo
	HUIDSource string
	New        []app.View // newly captured records
	Duplicates int
	Connected  bool
}

// Run connects, authenticates, syncs time and profile and, depending on
// mode, captures measurements. It always disconnects before returning.
func (r *Runner) Run(ctx context.Context, adapter *bluez.Adapter, mode Mode) (Outcome, error) {
	var out Outcome
	ctx, cancel := context.WithTimeout(ctx, r.Cfg.SessionTimeout)
	defer cancel()

	r.Log.Info("connecting to scale", "address", r.Cfg.Address)
	octx, ocancel := context.WithTimeout(ctx, 40*time.Second)
	defer ocancel()
	dial := r.Dial
	if dial == nil {
		dial = func(ctx context.Context) (scale.Transport, error) { return scale.Dial(ctx, adapter, r.Cfg.Address) }
	}
	link, err := dial(octx)
	if err != nil {
		return out, err
	}
	sess, err := scale.NewSession(octx, link, scale.Options{
		Secrets: r.Cfg.Secrets,
		Address: r.Cfg.Address,
		Log:     r.Log,
		Trace:   r.Cfg.Trace,
	})
	if err != nil {
		link.Close()
		return out, err
	}
	out.Connected = true
	defer func() {
		sess.Close()
		r.Log.Info("disconnected from scale")
	}()

	if err := sess.Authenticate(ctx); err != nil {
		return out, err
	}
	r.Log.Info("authenticated")

	now := time.Now()
	if err := sess.SyncTime(ctx, now); err != nil {
		r.Log.Warn("time sync failed", "err", err)
	}

	huid := "sscale:" + strings.ReplaceAll(r.Cfg.Address, ":", "")
	out.HUIDSource = "local"
	if mi, err := sess.ManagerInfo(ctx); err != nil {
		r.Log.Warn("manager info read failed", "err", err)
	} else if mi.HUID != "" {
		huid, out.HUIDSource = mi.HUID, "scale"
		r.Log.Debug("scale is bound to an account", "device_id", mi.DeviceID)
	} else {
		r.Log.Debug("manager info: scale is not bound to an account")
	}
	if err := sess.SetUserInfo(ctx, r.userInfo(ctx, huid, now)); err != nil {
		switch {
		case out.HUIDSource == "scale":
			r.Log.Warn("user profile sync failed", "err", err)
		case !r.profileNoted:
			// Observed on an unbound Scale 3: it answers 01. Recording is not
			// affected; body composition is computed by sscale.
			r.profileNoted = true
			r.Log.Info("scale is not bound to a Huawei Health account and declined the user profile; measurements are not affected", "err", err)
		default:
			r.Log.Debug("user profile declined", "err", err)
		}
	} else {
		r.Log.Debug("user profile synced", "huid_source", out.HUIDSource)
	}

	st := r.Store.Snapshot()
	if mode.ReadInfo || st.Pairing.Device.Firmware == "" {
		out.Device = sess.ReadDeviceInfo(ctx)
	} else {
		out.Device = st.Pairing.Device
	}
	_ = r.Store.Update(func(s *store.State) {
		if !s.Pairing.Paired {
			t := time.Now()
			s.Pairing.PairedAt = &t
		}
		s.Pairing.Paired = true
		s.Pairing.HUIDSource = out.HUIDSource
		if out.Device.Firmware != "" || out.Device.ProductInfo != "" {
			s.Pairing.Device = out.Device
		}
	})

	if !mode.Measure {
		return out, nil
	}

	lastLive := -1.0
	res, merr := sess.Measure(ctx, r.Cfg.IdleGrace, func(m *hagrid.Measurement) {
		if m.WeightKg != lastLive {
			lastLive = m.WeightKg
			r.Log.Info("live weight", "weight", fmt.Sprintf("%.2fkg", m.WeightKg))
		}
	})
	if res.Measurement != nil {
		// "live": weighed during this connection; "latest": the scale's last
		// result, read while it was idle.
		src := "live"
		if !res.Active {
			src = "latest"
		}
		r.capture(ctx, makeRecord(res.Measurement, src, res.ReceivedAt, res.Active), &out)
	}
	if merr != nil {
		return out, fmt.Errorf("measurement: %w", merr)
	}

	hist, herr := sess.ReadHistory(ctx)
	if len(hist) > 0 {
		r.Log.Info("read stored history", "records", len(hist))
	}
	for _, m := range hist {
		r.capture(ctx, makeRecord(m, "history", time.Now(), false), &out)
	}
	if herr != nil {
		return out, fmt.Errorf("history: %w", herr)
	}
	return out, nil
}

func (r *Runner) capture(ctx context.Context, m *db.Measurement, out *Outcome) {
	v, added, err := r.App.Add(ctx, m)
	if err != nil {
		r.Log.Error("failed to store measurement", "err", err)
		return
	}
	if !added {
		out.Duplicates++
		r.Log.Debug("skipping already recorded measurement", "source", m.Source, "time", m.Time)
		return
	}
	out.New = append(out.New, v)
	LogRecord(r.Log, v)
}

func (r *Runner) userInfo(ctx context.Context, huid string, now time.Time) hagrid.UserInfo {
	p := r.App.Profile(ctx)
	weight := r.Cfg.Profile.WeightKg
	if lm, err := r.App.Latest(ctx); err == nil && lm != nil {
		weight = lm.WeightKg
	}
	if weight <= 0 && p.HeightCm > 0 {
		h := p.HeightCm / 100
		weight = math.Round(22*h*h*10) / 10 // BMI 22 estimate
	}
	gender := 0
	if r.App.Scale3 {
		if p.Male {
			gender = 1 // Scale 3: male=1, female=0
		}
	} else if !p.Male {
		gender = 1 // other Hagrid models: male=0, female=1
	}
	return hagrid.UserInfo{
		HUID:     huid,
		UID:      fmt.Sprintf("u:%08X", p.UserID),
		Gender:   gender,
		Age:      p.Age(now),
		HeightCm: int(math.Round(p.HeightCm)),
		WeightKg: weight,
	}
}

func makeRecord(m *hagrid.Measurement, source string, receivedAt time.Time, live bool) *db.Measurement {
	return &db.Measurement{
		Time:            recordTime(m, receivedAt, live),
		ScaleTime:       m.Timestamp,
		ReceivedAt:      receivedAt,
		Source:          source,
		WeightKg:        math.Round(m.WeightKg*100) / 100,
		ScaleFatPercent: m.FatPercent,
		HeartRateBPM:    m.HeartRate,
		ImpedanceRaw:    m.LowImpedance,
		HighImpedance:   m.HighImp,
		Fingerprint:     m.Fingerprint(),
	}
}

// recordTime picks the best timestamp: the scale's clock when it is
// plausible, otherwise the time we received the record.
func recordTime(m *hagrid.Measurement, receivedAt time.Time, live bool) time.Time {
	if m.Timestamp == nil {
		return receivedAt
	}
	ts := *m.Timestamp
	if d := ts.Sub(receivedAt); d > -10*time.Minute && d < 10*time.Minute {
		return ts
	}
	if live {
		return receivedAt // active weighing but the scale clock is off
	}
	if ts.Year() >= 2020 && ts.Before(receivedAt.Add(10*time.Minute)) {
		return ts
	}
	return receivedAt
}

// LogRecord prints a captured measurement.
func LogRecord(log *slog.Logger, r app.View) {
	attrs := []any{
		"source", r.Source,
		"time", r.Time.Format(time.DateTime),
		"weight", fmt.Sprintf("%.2fkg", r.WeightKg),
	}
	if r.BMI > 0 {
		attrs = append(attrs, "bmi", fmt.Sprintf("%.1f", r.BMI))
	}
	if c := r.Composition; c != nil {
		attrs = append(attrs,
			"fat", fmt.Sprintf("%.1f%%", c.BodyFatPercent),
			"water", fmt.Sprintf("%.1f%%", c.WaterPercent),
			"muscle", fmt.Sprintf("%.1fkg", c.SkeletalMuscleKg),
			"bone", fmt.Sprintf("%.2fkg", c.BoneMineralKg),
			"lean_mass", fmt.Sprintf("%.1fkg", c.LeanBodyMassKg),
			"protein", fmt.Sprintf("%.1f%%", c.ProteinPercent),
			"bmr", fmt.Sprintf("%dkcal", c.BMRKcal),
			"visceral_fat", c.VisceralFatLevel,
		)
	} else if r.ScaleFatPercent > 0 {
		attrs = append(attrs, "fat", fmt.Sprintf("%.1f%%", r.ScaleFatPercent))
	}
	if r.ImpedanceOhm > 0 {
		attrs = append(attrs, "impedance", fmt.Sprintf("%.1fohm", r.ImpedanceOhm))
	}
	if r.HeartRateBPM > 0 {
		attrs = append(attrs, "heart_rate", fmt.Sprintf("%dbpm", r.HeartRateBPM))
	}
	if !r.Valid {
		attrs = append(attrs, "filtered", r.FilterReason)
	}
	log.Info("MEASUREMENT", attrs...)
}

// IsAuthError reports whether err means the keys are wrong.
func IsAuthError(err error) bool { return errors.Is(err, scale.ErrAuthFailed) }
