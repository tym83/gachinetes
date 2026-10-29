package coach

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 26, 13, 0, 0, 0, time.UTC) // a Monday

func TestProgressiveOverloadWithRest(t *testing.T) {
	p := Plan{Min: 100, Max: 2000, Rest: 5 * time.Minute}
	s, r := Train(p, 500, 450, Record{}, t0)
	if s.Request != 600 || !strings.Contains(s.Why, "progressive overload") {
		t.Fatalf("a heavy set: %+v", s)
	}
	s, r = Train(p, 600, 590, r, t0.Add(time.Minute))
	if s.Request != 600 || !strings.Contains(s.Why, "resting") {
		t.Fatalf("during rest: %+v", s)
	}
	s, r = Train(p, 600, 590, r, t0.Add(6*time.Minute))
	if s.Request != 720 {
		t.Fatalf("after rest: %+v", s)
	}
	if r.Peak != 590 {
		t.Fatalf("personal record: %d", r.Peak)
	}
	s, _ = Train(Plan{Min: 100, Max: 700}, 700, 690, Record{}, t0)
	if s.Request != 700 || !strings.Contains(s.Why, "maximum") {
		t.Fatalf("at the max: %+v", s)
	}
}

func TestDeloadAfterALightTime(t *testing.T) {
	p := Plan{Min: 100, Max: 2000, DeloadAfter: 30 * time.Minute}
	s, r := Train(p, 1000, 100, Record{}, t0)
	if s.Request != 1000 || !strings.Contains(s.Why, "watching") {
		t.Fatalf("first light set: %+v", s)
	}
	s, _ = Train(p, 1000, 100, r, t0.Add(31*time.Minute))
	if s.Request != 800 || !strings.Contains(s.Why, "deload") {
		t.Fatalf("after a light time: %+v", s)
	}
	// The record of this hour holds the weight.
	r.Week = map[string]int64{HourOfWeek(t0.Add(31 * time.Minute)): 760}
	s, _ = Train(p, 1000, 100, r, t0.Add(31*time.Minute))
	if s.Request != 950 {
		t.Fatalf("deload above this hour's record: %+v", s)
	}
}

func TestMuscleMemory(t *testing.T) {
	p := Plan{Min: 100, Max: 4000}
	// Last Monday at 14:00 it lifted 1600; at 13:55 the coach raises it.
	r := Record{Week: map[string]int64{"Mon-14": 1600}}
	s, _ := Train(p, 500, 300, r, t0.Add(55*time.Minute))
	if s.Request != 2000 || !strings.Contains(s.Why, "muscle memory") {
		t.Fatalf("before a remembered peak: %+v", s)
	}
}
