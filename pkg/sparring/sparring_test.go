package sparring

import "testing"

func TestRank(t *testing.T) {
	won := func(s float64) Round { return Round{Done: true, Won: true, Seconds: s} }
	lost := Round{Done: true}
	for want, rounds := range map[string][]Round{
		"Unranked":         {{Done: false}},
		"Boss of this gym": {won(3), won(5), won(4)},
		"Gold":             {won(30), won(40), won(20)},
		"Silver":           {won(3), won(3), won(3), lost},
		"Bronze":           {won(3), lost, lost},
	} {
		if got, _, _ := Rank(rounds); got != want {
			t.Errorf("%s: %s", want, got)
		}
	}
	if k := Keep(make([]Round, 30), 20); len(k) != 20 {
		t.Fatal("keep")
	}
}
