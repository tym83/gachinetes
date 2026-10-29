// Package sparring ranks services by how they take a punch: a pod of
// theirs is killed on schedule, and the round is won when they are fully
// ready again in time.
package sparring

import (
	"sort"
	"time"
)

// Round is one punch and how the service took it.
type Round struct {
	At     time.Time `json:"at"`
	Victim string    `json:"victim"`
	// Seconds to full readiness; Won when within the limit.
	Seconds float64 `json:"seconds,omitempty"`
	Won     bool    `json:"won"`
	Done    bool    `json:"done"`
}

// Rank is a service's standing in the gym.
func Rank(rounds []Round) (rank string, wins, losses int) {
	var times []float64
	for _, r := range rounds {
		if !r.Done {
			continue
		}
		if r.Won {
			wins++
			times = append(times, r.Seconds)
		} else {
			losses++
		}
	}
	total := wins + losses
	if total == 0 {
		return "Unranked", 0, 0
	}
	ratio := float64(wins) / float64(total)
	sort.Float64s(times)
	median := 0.0
	if len(times) > 0 {
		median = times[len(times)/2]
	}
	switch {
	case ratio == 1 && total >= 3 && median <= 10:
		return "Boss of this gym", wins, losses
	case ratio >= 0.9:
		return "Gold", wins, losses
	case ratio >= 0.7:
		return "Silver", wins, losses
	}
	return "Bronze", wins, losses
}

// Keep trims rounds to the last n.
func Keep(rounds []Round, n int) []Round {
	if len(rounds) > n {
		return rounds[len(rounds)-n:]
	}
	return rounds
}
