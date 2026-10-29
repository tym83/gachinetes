// Package wrestling finds controller wars: field managers that keep
// rewriting the same field of the same object, each undoing the other.
// It reads only what every write already carries, the object before and
// after and its managedFields, so it works on any cluster.
package wrestling

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Ref names an object.
type Ref struct {
	Group     string `json:"group,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

func (r Ref) String() string {
	k := r.Kind
	if r.Group != "" {
		k += "." + r.Group
	}
	if r.Namespace != "" {
		return k + " " + r.Namespace + "/" + r.Name
	}
	return k + " " + r.Name
}

// RefOf is the reference of an object.
func RefOf(u *unstructured.Unstructured) Ref {
	gvk := u.GroupVersionKind()
	return Ref{Group: gvk.Group, Kind: gvk.Kind, Namespace: u.GetNamespace(), Name: u.GetName()}
}

// Writers are the field managers that wrote new, as old became new: those
// whose managedFields entry is new or newer. When the clock of
// managedFields (seconds) cannot tell, the managers of the newest entries.
func Writers(old, new *unstructured.Unstructured) []string {
	before := map[string]time.Time{}
	for _, e := range old.GetManagedFields() {
		if e.Time != nil {
			before[key(e)] = e.Time.Time
		}
	}
	var out []string
	var newest time.Time
	var newestOf []string
	for _, e := range new.GetManagedFields() {
		if e.Time == nil {
			continue
		}
		t := e.Time.Time
		if b, ok := before[key(e)]; !ok || t.After(b) {
			out = append(out, e.Manager)
		}
		switch {
		case t.After(newest):
			newest, newestOf = t, []string{e.Manager}
		case t.Equal(newest):
			newestOf = append(newestOf, e.Manager)
		}
	}
	if len(out) == 0 {
		out = newestOf
	}
	sort.Strings(out)
	return dedupe(out)
}

func key(e metav1.ManagedFieldsEntry) string {
	return e.Manager + "\x00" + string(e.Operation) + "\x00" + e.Subresource
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// ignored are paths every write changes, or that the API server keeps.
var ignored = map[string]bool{
	"metadata.resourceVersion": true,
	"metadata.managedFields":   true,
	"metadata.generation":      true,
}

// Changed are the leaf paths whose values differ between old and new,
// such as spec.replicas or metadata.annotations.x/y.
func Changed(old, new map[string]any) []string {
	var out []string
	diff("", old, new, &out)
	sort.Strings(out)
	return out
}

func diff(path string, a, b any, out *[]string) {
	if ignored[path] {
		return
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		for k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			diff(p, am[k], bm[k], out)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, path)
	}
}

// Round is one write of a field in a bout.
type Round struct {
	Manager string    `json:"manager"`
	Time    time.Time `json:"time"`
}

// Bout is a field two or more managers keep taking from each other.
type Bout struct {
	Object Ref    `json:"object"`
	Path   string `json:"path"`
	// Fighters are the managers, with how many rounds each won in the
	// window.
	Fighters map[string]int `json:"fighters"`
	// Switches: how many times the field changed hands in the window.
	Switches  int       `json:"switches"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// Name is a stable name for the bout, for an object to keep it in.
func (b Bout) Name() string {
	return fmt.Sprintf("%x", fnv(b.Object.String()+"\x00"+b.Path))
}

func fnv(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// Referee watches writes and calls bouts.
type Referee struct {
	// Window: rounds older than this are forgotten. Switches: how many
	// changes of hands within the window make a bout.
	Window   time.Duration
	Switches int
	// Skip leaves paths out by prefix, such as status.conditions where
	// several controllers report by design.
	Skip []string

	mu     sync.Mutex
	rounds map[string][]Round
	refs   map[string]Ref
	paths  map[string]string
}

func (r *Referee) init() {
	if r.rounds == nil {
		r.rounds, r.refs, r.paths = map[string][]Round{}, map[string]Ref{}, map[string]string{}
	}
	if r.Window == 0 {
		r.Window = 5 * time.Minute
	}
	if r.Switches == 0 {
		r.Switches = 4
	}
}

// Observe takes one write: the object before and after.
func (r *Referee) Observe(old, new *unstructured.Unstructured, at time.Time) {
	writers := Writers(old, new)
	if len(writers) != 1 {
		// A write two managers might have made says nothing about who
		// took the field.
		return
	}
	ref := RefOf(new)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	for _, p := range Changed(old.Object, new.Object) {
		if r.skipped(p) {
			continue
		}
		k := ref.String() + "\x00" + p
		r.rounds[k] = append(r.rounds[k], Round{Manager: writers[0], Time: at})
		r.refs[k], r.paths[k] = ref, p
	}
}

func (r *Referee) skipped(p string) bool {
	for _, s := range r.Skip {
		if p == s || strings.HasPrefix(p, s+".") {
			return true
		}
	}
	return false
}

// Bouts are the bouts going on at now, by object and path; it forgets
// rounds older than the window.
func (r *Referee) Bouts(now time.Time) []Bout {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	var out []Bout
	for k, rounds := range r.rounds {
		kept := rounds[:0]
		for _, rd := range rounds {
			if now.Sub(rd.Time) <= r.Window {
				kept = append(kept, rd)
			}
		}
		if len(kept) == 0 {
			delete(r.rounds, k)
			delete(r.refs, k)
			delete(r.paths, k)
			continue
		}
		r.rounds[k] = kept
		b := Bout{Object: r.refs[k], Path: r.paths[k], Fighters: map[string]int{}, FirstSeen: kept[0].Time, LastSeen: kept[len(kept)-1].Time}
		for i, rd := range kept {
			b.Fighters[rd.Manager]++
			if i > 0 && kept[i-1].Manager != rd.Manager {
				b.Switches++
			}
		}
		if len(b.Fighters) > 1 && b.Switches >= r.Switches {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
