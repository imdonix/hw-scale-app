package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/tamasmagyar/sscale/internal/app"
	"github.com/tamasmagyar/sscale/internal/config"
	"github.com/tamasmagyar/sscale/internal/db"
	"github.com/tamasmagyar/sscale/internal/live"
	"github.com/tamasmagyar/sscale/internal/store"
)

type fixture struct {
	t   *testing.T
	srv *httptest.Server
	key string
	app *app.App
}

func setup(t *testing.T) *fixture {
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.Model = "HUAWEI SCALE 3"
	cfg.Profile = config.Profile{HeightCm: 178, Birthdate: time.Date(2000, 5, 1, 0, 0, 0, 0, time.Local), HasBirth: true, Male: true, HasSex: true}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := app.Open(context.Background(), &cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := a.DB.APIKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{App: a, State: func() store.State { return store.State{} }, Log: log,
		UI: fstest.MapFS{
			"index.html":    {Data: []byte("<html>app</html>")},
			"assets/app.js": {Data: []byte("js")},
		}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.Close(); a.Close() })
	return &fixture{t: t, srv: srv, key: key, app: a}
}

func (f *fixture) do(method, path string, body any, out any) int {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+f.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func (f *fixture) add(ts time.Time, kg float64, imp int) {
	f.t.Helper()
	m := &db.Measurement{Time: ts, ReceivedAt: ts, Source: "live", WeightKg: kg, ImpedanceRaw: []int{imp, 0, 0, 0, 0, 0},
		Fingerprint: ts.String() + "|" + time.Duration(kg*1000).String()}
	if _, ok, err := f.app.Add(context.Background(), m); err != nil || !ok {
		f.t.Fatalf("add: %v %v", ok, err)
	}
}

func TestAuth(t *testing.T) {
	f := setup(t)
	resp, _ := http.Get(f.srv.URL + "/api/measurements")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", f.srv.URL+"/api/status", nil)
	req.Header.Set("X-API-Key", "wrong")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d", resp.StatusCode)
	}
	if code := f.do("GET", "/api/status", nil, nil); code != http.StatusOK {
		t.Fatalf("good key: %d", code)
	}
	resp, _ = http.Get(f.srv.URL + "/api/health")
	if resp.StatusCode != http.StatusOK {
		t.Fatal("health must be public")
	}
}

func TestMeasurementsFilterAndOverride(t *testing.T) {
	f := setup(t)
	d := time.Date(2026, 10, 10, 7, 0, 0, 0, time.Local)
	f.add(d, 77.5, 5143)
	f.add(d.Add(24*time.Hour), 77.3, 5100)
	f.add(d.Add(24*time.Hour+time.Hour), 58.0, 6200) // someone else
	f.add(d.Add(48*time.Hour), 77.1, 5120)

	var list []app.View
	f.do("GET", "/api/measurements?from=2026-10-01&to=2026-11-01", nil, &list)
	if len(list) != 3 {
		t.Fatalf("expected 3 valid readings, got %d", len(list))
	}
	if c := list[0].Composition; c == nil || c.BMRKcal == 0 || list[0].BMI != 24.5 || list[0].ImpedanceOhm != 514.3 {
		t.Fatalf("derived values missing: %+v", list[0])
	}
	f.do("GET", "/api/measurements?from=2026-10-01&to=2026-11-01&include_invalid=true", nil, &list)
	if len(list) != 4 || list[2].Valid || list[2].FilterReason == "" {
		t.Fatalf("filtered reading not reported: %+v", list)
	}

	// The owner says the 58 kg reading was theirs.
	var v app.View
	yes := true
	if code := f.do("PATCH", "/api/measurements/"+itoa(list[2].ID), map[string]*bool{"valid": &yes}, &v); code != 200 || !v.Valid {
		t.Fatalf("patch: %d %+v", code, v)
	}
	if code := f.do("PATCH", "/api/measurements/9999", map[string]*bool{"valid": nil}, nil); code != http.StatusNotFound {
		t.Fatalf("patch unknown: %d", code)
	}
	if code := f.do("DELETE", "/api/measurements/"+itoa(list[2].ID), nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	f.do("GET", "/api/measurements?include_invalid=1", nil, &list)
	if len(list) != 3 {
		t.Fatalf("after delete: %d", len(list))
	}
}

func TestSettings(t *testing.T) {
	f := setup(t)
	var s db.Settings
	f.do("GET", "/api/settings", nil, &s)
	if s.HeightCm != 178 || s.Sex != "male" || s.Birthdate != "2000-05-01" || !s.Filter.Enabled {
		t.Fatalf("seeded settings %+v", s)
	}
	s.HeightCm = 180
	s.Filter.Enabled = false
	if code := f.do("PUT", "/api/settings", s, &s); code != 200 || s.HeightCm != 180 {
		t.Fatalf("put: %d %+v", code, s)
	}
	s.HeightCm = 20
	var e map[string]string
	if code := f.do("PUT", "/api/settings", s, &e); code != http.StatusBadRequest || e["error"] == "" {
		t.Fatalf("invalid height accepted: %d", code)
	}
	if code := f.do("PUT", "/api/settings", map[string]any{"bogus": 1}, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown field accepted: %d", code)
	}
}

func TestSPA(t *testing.T) {
	f := setup(t)
	for path, want := range map[string]string{"/": "<html>app</html>", "/some/route": "<html>app</html>", "/assets/app.js": "js"} {
		resp, err := http.Get(f.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != want {
			t.Errorf("%s: got %q", path, b)
		}
	}
	resp, _ := http.Get(f.srv.URL + "/api/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown api path: %d", resp.StatusCode)
	}
}

func itoa(v int64) string { b, _ := json.Marshal(v); return string(b) }

func TestLiveStream(t *testing.T) {
	f := setup(t)
	hub := live.New()
	s := &Server{App: f.app, State: func() store.State { return store.State{} }, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Live: hub, LiveHeartbeat: 50 * time.Millisecond}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/api/live")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("live without key: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/live", nil)
	req.Header.Set("Authorization", "Bearer "+f.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	nextEvent := func() live.Event {
		t.Helper()
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") {
				var e live.Event
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
					t.Fatal(err)
				}
				return e
			}
		}
		t.Fatal("stream ended")
		return live.Event{}
	}
	if e := nextEvent(); e.Phase != live.Idle {
		t.Fatalf("first event %+v", e)
	}
	hub.Start("Connecting to the scale")
	if e := nextEvent(); e.Phase != live.Connecting || e.Message == "" {
		t.Fatalf("second event %+v", e)
	}
	hub.Update(func(e *live.Event) { e.Phase, e.WeightKg = live.Weighing, 77.5 })
	if e := nextEvent(); e.WeightKg != 77.5 {
		t.Fatalf("third event %+v", e)
	}
}
