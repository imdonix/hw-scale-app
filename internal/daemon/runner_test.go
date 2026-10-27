package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tamasmagyar/sscale/internal/app"
	"github.com/tamasmagyar/sscale/internal/config"
	"github.com/tamasmagyar/sscale/internal/hagrid"
	"github.com/tamasmagyar/sscale/internal/scale"
	"github.com/tamasmagyar/sscale/internal/store"
)

func init() { scale.SetTiming(20*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond) }

func rec(weightKg float64, ts time.Time, imp, hr int, extra ...byte) []byte {
	p := make([]byte, 26)
	w := int(weightKg*100 + 0.5)
	p[0], p[1] = byte(w), byte(w>>8)
	p[4], p[5] = byte(ts.Year()), byte(ts.Year()>>8)
	p[6], p[7], p[8], p[9], p[10] = byte(ts.Month()), byte(ts.Day()), byte(ts.Hour()), byte(ts.Minute()), byte(ts.Second())
	p[12], p[13] = byte(imp), byte(imp>>8)
	p[24], p[25] = byte(hr), byte(hr>>8)
	return append(p, extra...)
}

func testConfig(t *testing.T) *config.Config {
	sec, err := hagrid.ParseSecrets("0F1E2D3C4B5A69788796A5B4C3D2E1F0", "CA4946D061C9FE534F6044F930EBB69B", "FBCE6E2B4BAF80ED969BA26B4A4B9325")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Address = "08:B8:D0:C2:9D:CD"
	cfg.Model = "HUAWEI SCALE 3"
	cfg.Secrets = sec
	cfg.IdleGrace = 50 * time.Millisecond
	cfg.Profile = config.Profile{HeightCm: 178, Birthdate: time.Date(2000, 5, 1, 0, 0, 0, 0, time.Local), HasBirth: true, Male: true, HasSex: true, UserID: 1}
	return &cfg
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func openApp(t *testing.T, cfg *config.Config) *app.App {
	t.Helper()
	cfg.DataDir = t.TempDir()
	a, err := app.Open(context.Background(), cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestRunnerRecordsAndDeduplicates(t *testing.T) {
	cfg := testConfig(t)
	a := openApp(t, cfg)
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	newFake := func() *scale.FakeScale {
		return &scale.FakeScale{
			T: t, Secrets: cfg.Secrets, Address: cfg.Address, HUID: "HUID-1",
			Statuses: []byte{1, 1, 0},
			Realtime: rec(86.25, now, 5098, 97),
			History: [][]byte{
				rec(85.10, now.Add(-48*time.Hour), 5000, 0, 1, 1),
				rec(86.25, now, 5098, 97, 1, 0), // the live record again
			},
		}
	}
	fake := newFake()
	r := &Runner{Cfg: cfg, Store: st, App: a, Log: quiet(),
		Dial: func(context.Context) (scale.Transport, error) { return fake, nil }}

	out, err := r.Run(context.Background(), nil, Mode{Measure: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.New) != 2 || out.Duplicates != 1 {
		t.Fatalf("new=%d dup=%d", len(out.New), out.Duplicates)
	}
	l := out.New[0]
	if l.Source != "live" || l.WeightKg != 86.25 || l.HeartRateBPM != 97 || l.ImpedanceOhm != 509.8 || !l.Time.Equal(now) {
		t.Fatalf("live record %+v", l)
	}
	if l.Composition == nil || l.Composition.BMRKcal < 1500 || l.BMI != 27.2 {
		t.Fatalf("composition %+v bmi %v", l.Composition, l.BMI)
	}
	if !fake.Closed {
		t.Fatal("session not closed")
	}
	// User info sent to the scale carries the scale's HUID and Scale 3 gender.
	if string(fake.UserInfo[:6]) != "HUID-1" || fake.UserInfo[62] != 1 || int(fake.UserInfo[63]) != cfg.Profile.Age(time.Now()) {
		t.Fatalf("user info %x", fake.UserInfo)
	}

	snap := st.Snapshot()
	if !snap.Pairing.Paired || snap.Pairing.Device.Firmware != "2.0.0.70" || snap.Pairing.HUIDSource != "scale" {
		t.Fatalf("state %+v", snap)
	}
	if total, _, _ := a.DB.Count(context.Background()); total != 2 {
		t.Fatalf("db has %d records", total)
	}
	if lm, _ := a.Latest(context.Background()); lm == nil || lm.WeightKg != 86.25 {
		t.Fatalf("latest %+v", lm)
	}

	// A second session that sees the same records stores nothing new.
	fake = newFake()
	out, err = r.Run(context.Background(), nil, Mode{Measure: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.New) != 0 || out.Duplicates != 3 {
		t.Fatalf("second run new=%d dup=%d", len(out.New), out.Duplicates)
	}
	if total, _, _ := a.DB.Count(context.Background()); total != 2 {
		t.Fatalf("db has %d records after second run", total)
	}
}

func TestRunnerAuthFailure(t *testing.T) {
	cfg := testConfig(t)
	a := openApp(t, cfg)
	st, _ := store.Open(cfg.DataDir)
	good := cfg.Secrets
	cfg.Secrets.CAK = make([]byte, 16)
	fake := &scale.FakeScale{T: t, Secrets: good, Address: cfg.Address}
	r := &Runner{Cfg: cfg, Store: st, App: a, Log: quiet(),
		Dial: func(context.Context) (scale.Transport, error) { return fake, nil }}
	_, err := r.Run(context.Background(), nil, Mode{Measure: true})
	if !IsAuthError(err) {
		t.Fatalf("expected auth error, got %v", err)
	}
	if st.Snapshot().Pairing.Paired {
		t.Fatal("must not be marked paired")
	}
}

func TestRecordTime(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	mk := func(ts *time.Time) *hagrid.Measurement { return &hagrid.Measurement{Timestamp: ts} }
	old := now.Add(-30 * 24 * time.Hour)
	bogus := time.Date(2001, 1, 1, 0, 0, 0, 0, time.Local)
	future := now.Add(48 * time.Hour)
	cases := []struct {
		name string
		m    *hagrid.Measurement
		live bool
		want time.Time
	}{
		{"no ts", mk(nil), true, now},
		{"close ts", mk(&now), true, now},
		{"live with stale clock uses receive time", mk(&old), true, now},
		{"stored record keeps its time", mk(&old), false, old},
		{"implausible year", mk(&bogus), false, now},
		{"future", mk(&future), false, now},
	}
	for _, c := range cases {
		if got := recordTime(c.m, now, c.live); !got.Equal(c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestAge(t *testing.T) {
	p := config.Profile{Birthdate: time.Date(2000, 5, 1, 0, 0, 0, 0, time.Local), HasBirth: true}
	if a := p.Age(time.Date(2026, 4, 30, 0, 0, 0, 0, time.Local)); a != 25 {
		t.Fatal(a)
	}
	if a := p.Age(time.Date(2026, 5, 1, 0, 0, 0, 0, time.Local)); a != 26 {
		t.Fatal(a)
	}
}

func TestRetryDelay(t *testing.T) {
	if retryDelay(1) != 5*time.Second || retryDelay(100) != 5*time.Minute {
		t.Fatal("unexpected backoff")
	}
}

func TestCooldownSurvivesRestart(t *testing.T) {
	cfg := testConfig(t)
	now := time.Now()
	var st store.State
	if !initialLoopState(st, cfg, now).armed {
		t.Fatal("fresh daemon must be armed")
	}
	st.LastSession = &store.Session{Result: "ok", End: now.Add(-10 * time.Second)}
	if ls := initialLoopState(st, cfg, now); ls.armed || !ls.cooldownStart.Equal(st.LastSession.End) {
		t.Fatal("recent session must resume cooldown")
	}
	st.LastSession.End = now.Add(-cfg.MaxCooldown - time.Second)
	if !initialLoopState(st, cfg, now).armed {
		t.Fatal("old session must not block")
	}
	st.LastSession = &store.Session{Result: "error", End: now}
	if !initialLoopState(st, cfg, now).armed {
		t.Fatal("failed session must not block a retry")
	}
}
