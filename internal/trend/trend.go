// Package trend flags readings that do not fit the owner's weight trend,
// e.g. somebody else stepping on the scale.
package trend

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Params configures the filter.
type Params struct {
	Enabled bool
	// A reading is valid when it is within max(ToleranceKg,
	// TolerancePct% of the reference) of the reference weight, plus a drift
	// allowance for the days since the last valid reading.
	ToleranceKg  float64
	TolerancePct float64
}

// DefaultParams are the defaults used for new installations.
var DefaultParams = Params{Enabled: true, ToleranceKg: 3, TolerancePct: 4}

const (
	window       = 30 * 24 * time.Hour // reference readings must be this recent
	maxRefs      = 10                  // median over at most this many readings
	driftPerDay  = 0.2                 // kg a real weight can plausibly move per day
	maxDrift     = 5.0                 // cap of the drift allowance (kg)
	reanchorGap  = 14 * 24 * time.Hour // no valid reading for this long ...
	reanchorSize = 3                   // ... and this many consistent ones => new trend
)

// Point is one reading. Manual, when set, is the owner's decision and wins.
type Point struct {
	ID     int64
	Time   time.Time
	Weight float64
	Manual *bool
}

// Result is the automatic classification of a point.
type Result struct {
	ID     int64
	Valid  bool
	Reason string
}

// Classify evaluates all points (any order) and returns one result per point
// in chronological order.
func Classify(points []Point, p Params) []Result {
	pts := append([]Point(nil), points...)
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].Time.Before(pts[j].Time) })
	res := make([]Result, len(pts))
	effective := make([]bool, len(pts)) // manual ?? auto

	for i, pt := range pts {
		res[i] = Result{ID: pt.ID, Valid: true}
		if p.Enabled {
			res[i] = classifyOne(pts, effective, i, p)
		}
		effective[i] = res[i].Valid
		if pt.Manual != nil {
			effective[i] = *pt.Manual
		}
		if !effective[i] && pt.Manual == nil && p.Enabled {
			if cluster := reanchor(pts, effective, i, p); cluster != nil {
				for _, j := range cluster {
					res[j] = Result{ID: pts[j].ID, Valid: true, Reason: "new trend"}
					effective[j] = true
				}
			}
		}
	}
	return res
}

func classifyOne(pts []Point, effective []bool, i int, p Params) Result {
	pt := pts[i]
	var refs []float64
	var last time.Time
	for j := i - 1; j >= 0 && len(refs) < maxRefs; j-- {
		if pt.Time.Sub(pts[j].Time) > window {
			break
		}
		if effective[j] {
			refs = append(refs, pts[j].Weight)
			if last.IsZero() {
				last = pts[j].Time
			}
			if pts[j].Manual != nil {
				break // a confirmed reading re-anchors the trend
			}
		}
	}
	if len(refs) == 0 {
		return Result{ID: pt.ID, Valid: true, Reason: "no recent trend"}
	}
	ref := median(refs)
	tol := tolerance(ref, pt.Time.Sub(last), p)
	if d := pt.Weight - ref; math.Abs(d) > tol {
		return Result{ID: pt.ID, Valid: false,
			Reason: fmt.Sprintf("%+.1f kg from the trend (%.1f kg, allowed ±%.1f kg)", d, ref, tol)}
	}
	return Result{ID: pt.ID, Valid: true}
}

func tolerance(ref float64, since time.Duration, p Params) float64 {
	base := math.Max(p.ToleranceKg, ref*p.TolerancePct/100)
	days := math.Max(0, since.Hours()/24)
	return base + math.Min(maxDrift, driftPerDay*days)
}

// reanchor detects a genuine trend change: no valid reading during the last
// reanchorGap, but reanchorSize mutually consistent invalid readings (ending
// at i). It returns their indexes, or nil.
func reanchor(pts []Point, effective []bool, i int, p Params) []int {
	pt := pts[i]
	cluster := []int{i}
	for j := i - 1; j >= 0; j-- {
		if pt.Time.Sub(pts[j].Time) > reanchorGap {
			break
		}
		if effective[j] {
			return nil // the current trend is still alive
		}
		if pts[j].Manual == nil && math.Abs(pts[j].Weight-pt.Weight) <= tolerance(pt.Weight, 0, p) {
			cluster = append(cluster, j)
		}
	}
	if len(cluster) < reanchorSize {
		return nil
	}
	return cluster
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
