// Package coach trains pods' resources like a lifter: a little more at a
// time when the bar moves easily (progressive overload), rest between
// sets, personal records kept, the peaks of the week remembered (muscle
// memory), and now and then a lighter week to see whether the weight is
// still needed (deload).
package coach

import (
	"fmt"
	"math"
	"time"
)

// Plan is a TrainingPlan's spec for one resource of one container.
type Plan struct {
	// Min and Max bound the request, in the resource's base unit
	// (millicores for CPU, bytes for memory).
	Min, Max int64
	// Step is the share a request grows or shrinks by in one set.
	Step float64
	// Lift: use above this share of the request is a heavy set, and the
	// request grows. Light: use below it for DeloadAfter is a light time,
	// and the request shrinks.
	Lift, Light float64
	Rest        time.Duration
	DeloadAfter time.Duration
}

// Defaults fill what a plan leaves out.
func (p Plan) Defaults() Plan {
	if p.Step <= 0 {
		p.Step = 0.2
	}
	if p.Lift <= 0 {
		p.Lift = 0.8
	}
	if p.Light <= 0 {
		p.Light = 0.3
	}
	if p.Rest <= 0 {
		p.Rest = 5 * time.Minute
	}
	if p.DeloadAfter <= 0 {
		p.DeloadAfter = 30 * time.Minute
	}
	return p
}

// Record is what the coach remembers of one resource.
type Record struct {
	// Peak is the personal record, and when it was set.
	Peak   int64     `json:"peak"`
	PeakAt time.Time `json:"peakAt,omitempty"`
	// Week holds the peak of each hour of the week ("Mon-14"): muscle
	// memory for the peaks that come back.
	Week map[string]int64 `json:"week,omitempty"`
	// LastChange is when the request last moved; LightSince when use
	// fell below the light line, zero while it is not.
	LastChange time.Time `json:"lastChange,omitempty"`
	LightSince time.Time `json:"lightSince,omitempty"`
}

// HourOfWeek is the key of an hour in the week's memory.
func HourOfWeek(t time.Time) string {
	return fmt.Sprintf("%s-%02d", t.Weekday().String()[:3], t.Hour())
}

// Set is the coach's call for one resource.
type Set struct {
	Request int64
	Why     string
}

// Train takes the request, the use now, the record and the time, and
// returns the new request (or the same, with why) and the record updated.
func Train(p Plan, request, use int64, r Record, now time.Time) (Set, Record) {
	p = p.Defaults()
	if use > r.Peak {
		r.Peak, r.PeakAt = use, now
	}
	if r.Week == nil {
		r.Week = map[string]int64{}
	}
	if h := HourOfWeek(now); use > r.Week[h] {
		r.Week[h] = use
	}
	clamp := func(v int64) int64 { return max(p.Min, min(p.Max, v)) }
	resting := !r.LastChange.IsZero() && now.Sub(r.LastChange) < p.Rest

	// Muscle memory: an hour that was heavy before gets its weight ahead
	// of time, a step before the hour comes.
	next := HourOfWeek(now.Add(10 * time.Minute))
	if remembered := r.Week[next]; next != HourOfWeek(now) && float64(remembered) > p.Lift*float64(request) && !resting {
		want := clamp(int64(math.Ceil(float64(remembered) / p.Lift)))
		if want > request {
			r.LastChange, r.LightSince = now, time.Time{}
			return Set{want, fmt.Sprintf("muscle memory: %s peaked at %d last time", next, remembered)}, r
		}
	}
	ratio := 0.0
	if request > 0 {
		ratio = float64(use) / float64(request)
	}
	switch {
	case ratio >= p.Lift:
		r.LightSince = time.Time{}
		if resting {
			return Set{request, "heavy, but resting between sets"}, r
		}
		want := clamp(int64(math.Ceil(float64(request) * (1 + p.Step))))
		if want == request {
			return Set{request, "heavy, at the maximum"}, r
		}
		r.LastChange = now
		return Set{want, fmt.Sprintf("progressive overload: use %.0f%% of the request", 100*ratio)}, r
	case ratio < p.Light:
		if r.LightSince.IsZero() {
			r.LightSince = now
		}
		if now.Sub(r.LightSince) < p.DeloadAfter || resting {
			return Set{request, "light, watching"}, r
		}
		// Deload, never below the personal record of this hour.
		want := clamp(max(int64(float64(request)*(1-p.Step)), int64(math.Ceil(float64(r.Week[HourOfWeek(now)])/p.Lift))))
		if want >= request {
			return Set{request, "light, but the record of this hour holds the weight"}, r
		}
		r.LastChange, r.LightSince = now, time.Time{}
		return Set{want, fmt.Sprintf("deload: use %.0f%% of the request for %s", 100*ratio, p.DeloadAfter)}, r
	}
	r.LightSince = time.Time{}
	return Set{request, "in the zone"}, r
}
