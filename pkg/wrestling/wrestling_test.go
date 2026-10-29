package wrestling

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var t0 = time.Date(2026, 10, 29, 13, 0, 0, 0, time.UTC)

// deployment is web with replicas written by managers at times.
func deployment(replicas int64, rv string, managers map[string]time.Time) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "shop", "resourceVersion": rv},
		"spec":     map[string]any{"replicas": replicas},
	}}
	var mf []metav1.ManagedFieldsEntry
	for m, at := range managers {
		mf = append(mf, metav1.ManagedFieldsEntry{Manager: m, Operation: metav1.ManagedFieldsOperationUpdate, Time: &metav1.Time{Time: at}})
	}
	u.SetManagedFields(mf)
	return u
}

func TestWritersAndChanged(t *testing.T) {
	old := deployment(2, "1", map[string]time.Time{"hpa": t0, "argocd": t0})
	new := deployment(5, "2", map[string]time.Time{"hpa": t0.Add(time.Minute), "argocd": t0})
	if w := Writers(old, new); len(w) != 1 || w[0] != "hpa" {
		t.Fatalf("writers: %v", w)
	}
	if c := Changed(old.Object, new.Object); len(c) != 1 || c[0] != "spec.replicas" {
		t.Fatalf("changed: %v", c)
	}
}

// Two managers taking replicas from each other make a bout; one manager
// writing it many times does not.
func TestRefereeCallsABout(t *testing.T) {
	r := &Referee{Window: 10 * time.Minute, Switches: 4}
	times := map[string]time.Time{"hpa": t0, "argocd": t0}
	prev := deployment(2, "0", times)
	for i := 1; i <= 6; i++ {
		who := "hpa"
		if i%2 == 0 {
			who = "argocd"
		}
		at := t0.Add(time.Duration(i) * time.Second * 10)
		times = map[string]time.Time{"hpa": times["hpa"], "argocd": times["argocd"]}
		times[who] = at
		next := deployment(int64(2+i%2*3), "x", times)
		r.Observe(prev, next, at)
		prev = next
	}
	bouts := r.Bouts(t0.Add(2 * time.Minute))
	if len(bouts) != 1 {
		t.Fatalf("bouts: %+v", bouts)
	}
	b := bouts[0]
	if b.Path != "spec.replicas" || b.Fighters["hpa"] != 3 || b.Fighters["argocd"] != 3 || b.Switches != 5 {
		t.Fatalf("bout: %+v", b)
	}
	// Out of the window, it is over.
	if bouts := r.Bouts(t0.Add(time.Hour)); len(bouts) != 0 {
		t.Fatalf("an hour later: %+v", bouts)
	}
	solo := &Referee{Switches: 2}
	times = map[string]time.Time{"hpa": t0}
	prev = deployment(1, "0", times)
	for i := 1; i <= 6; i++ {
		at := t0.Add(time.Duration(i) * time.Minute / 10)
		next := deployment(int64(i), "x", map[string]time.Time{"hpa": at})
		solo.Observe(prev, next, at)
		prev = next
	}
	if bouts := solo.Bouts(t0.Add(time.Minute)); len(bouts) != 0 {
		t.Fatalf("one manager alone: %+v", bouts)
	}
}

func TestSkippedPaths(t *testing.T) {
	r := &Referee{Switches: 1, Skip: []string{"spec"}}
	a := deployment(1, "1", map[string]time.Time{"x": t0})
	b := deployment(2, "2", map[string]time.Time{"x": t0, "y": t0.Add(time.Second)})
	r.Observe(a, b, t0)
	c := deployment(1, "3", map[string]time.Time{"x": t0.Add(2 * time.Second), "y": t0.Add(time.Second)})
	r.Observe(b, c, t0)
	if bouts := r.Bouts(t0); len(bouts) != 0 {
		t.Fatalf("skipped path fought over: %+v", bouts)
	}
}
