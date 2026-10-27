package trend

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)

func day(d int) time.Time { return t0.Add(time.Duration(d) * 24 * time.Hour) }

func valid(rs []Result) map[int64]bool {
	m := map[int64]bool{}
	for _, r := range rs {
		m[r.ID] = r.Valid
	}
	return m
}

func TestOtherPersonIsFiltered(t *testing.T) {
	pts := []Point{
		{1, day(0), 77.5, nil}, {2, day(1), 77.3, nil}, {3, day(2), 77.6, nil},
		{4, day(2).Add(time.Hour), 58.2, nil}, // partner
		{5, day(3), 77.1, nil},
		{6, day(4), 95.0, nil}, // guest
		{7, day(5), 76.9, nil},
	}
	v := valid(Classify(pts, DefaultParams))
	for id, want := range map[int64]bool{1: true, 2: true, 3: true, 4: false, 5: true, 6: false, 7: true} {
		if v[id] != want {
			t.Errorf("reading %d: valid=%v want %v", id, v[id], want)
		}
	}
}

func TestSlowRealChangeStaysValid(t *testing.T) {
	var pts []Point
	w := 80.0
	for d := 0; d < 60; d++ {
		pts = append(pts, Point{int64(d), day(d), w, nil})
		w -= 0.12 // ~7 kg over two months
	}
	for _, r := range Classify(pts, DefaultParams) {
		if !r.Valid {
			t.Fatalf("reading %d flagged: %s", r.ID, r.Reason)
		}
	}
}

func TestGapAllowsDrift(t *testing.T) {
	pts := []Point{{1, day(0), 80, nil}, {2, day(1), 80, nil}, {3, day(20), 84.5, nil}}
	if !valid(Classify(pts, DefaultParams))[3] {
		t.Fatal("4.5 kg after 19 days should be plausible")
	}
}

func TestManualOverrideWinsAndFeedsTrend(t *testing.T) {
	yes, no := true, false
	pts := []Point{
		{1, day(0), 77, nil},
		{2, day(1), 70, &yes}, // owner says: that was me
		{3, day(2), 70.2, nil},
		{4, day(3), 77, &no},
	}
	rs := Classify(pts, DefaultParams)
	if v := valid(rs); !v[3] {
		t.Fatalf("70.2 should follow the manually confirmed 70 kg: %+v", rs)
	}
}

func TestReanchorAfterLongBreak(t *testing.T) {
	pts := []Point{
		{1, day(0), 77, nil}, {2, day(1), 77, nil},
		// back after 20 days, much heavier (still within the 30 day window)
		{3, day(21), 88, nil}, {4, day(22), 88.2, nil}, {5, day(23), 88.1, nil},
	}
	v := valid(Classify(pts, DefaultParams))
	if !v[3] || !v[4] || !v[5] {
		t.Fatalf("new trend should be accepted: %v", v)
	}
}

func TestPartnerWeighingDailyNeverTakesOver(t *testing.T) {
	var pts []Point
	for d := 0; d < 30; d++ {
		pts = append(pts, Point{int64(2 * d), day(d), 77, nil}, Point{int64(2*d + 1), day(d).Add(time.Hour), 60, nil})
	}
	for _, r := range Classify(pts, DefaultParams) {
		if r.ID%2 == 1 && r.ID > 1 && r.Valid {
			t.Fatalf("partner reading %d accepted", r.ID)
		}
		if r.ID%2 == 0 && !r.Valid {
			t.Fatalf("owner reading %d rejected", r.ID)
		}
	}
}

func TestDisabled(t *testing.T) {
	pts := []Point{{1, day(0), 77, nil}, {2, day(1), 50, nil}}
	if !valid(Classify(pts, Params{Enabled: false}))[2] {
		t.Fatal("filter disabled must accept everything")
	}
}
