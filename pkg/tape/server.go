package tape

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Object is a stored object, decoded, as the service returns it.
type Object struct {
	Key      string `json:"key"`
	Resource string `json:"resource"`
	// Revision is the revision of the object's write; Time when it was.
	Revision int64 `json:"revision"`
	Time     int64 `json:"time,omitempty"`
	// DeletedAt is the revision that deleted it, for the graveyard.
	DeletedAt   int64          `json:"deletedAt,omitempty"`
	DeletedTime int64          `json:"deletedTime,omitempty"`
	Deleted     bool           `json:"deleted,omitempty"`
	Object      map[string]any `json:"object,omitempty"`
	// Error is why the value could not be decoded.
	Error string `json:"error,omitempty"`
}

// Change is an object that differs between two points.
type Change struct {
	Key      string  `json:"key"`
	Resource string  `json:"resource"`
	From     *Object `json:"from,omitempty"`
	To       *Object `json:"to,omitempty"`
}

// Diff is what changed under a prefix between two points.
type Diff struct {
	FromRev int64    `json:"fromRevision"`
	ToRev   int64    `json:"toRevision"`
	Added   []Change `json:"added"`
	Removed []Change `json:"removed"`
	Changed []Change `json:"changed"`
}

func object(kv KV, reveal bool) Object {
	o := Object{Key: kv.Key, Resource: Resource(kv.Key), Revision: kv.ModRevision, Time: kv.Time, Deleted: kv.Deleted}
	if kv.Deleted {
		return o
	}
	u, err := Decode(kv.Value)
	if err != nil {
		o.Error = err.Error()
		return o
	}
	if !reveal {
		Redact(u)
	}
	o.Object = u.Object
	return o
}

// Filter narrows objects down; empty fields match everything. Kind
// matches apiVersion's group too when given as Kind.group.
type Filter struct {
	Namespace, Kind, Name string
	Reveal                bool
}

func (f Filter) match(o Object) bool {
	if o.Object == nil {
		// Undecodable (encrypted) values match by key alone.
		return f.Kind == "" && f.Name == "" && (f.Namespace == "" || strings.Contains(o.Key, "/"+f.Namespace+"/"))
	}
	u := unstructured.Unstructured{Object: o.Object}
	if f.Namespace != "" && u.GetNamespace() != f.Namespace && !(u.GetKind() == "Namespace" && u.GetName() == f.Namespace) {
		return false
	}
	if f.Name != "" && u.GetName() != f.Name {
		return false
	}
	if f.Kind != "" {
		kind, group, _ := strings.Cut(f.Kind, ".")
		gvk := u.GroupVersionKind()
		if !strings.EqualFold(gvk.Kind, kind) || (group != "" && gvk.Group != group) {
			return false
		}
	}
	return true
}

// Objects are the objects under prefix at p that f matches.
func Objects(ctx context.Context, s *Store, prefix string, p Point, f Filter) (int64, []Object, error) {
	r, err := s.Range(ctx, prefix, p)
	if err != nil {
		return 0, nil, err
	}
	out := make([]Object, 0, len(r.KVs))
	for _, kv := range r.KVs {
		if o := object(kv, f.Reveal); f.match(o) {
			out = append(out, o)
		}
	}
	return r.Revision, out, nil
}

// DiffBetween compares the objects under prefix at two points. Status and
// metadata the API server keeps up to date (resourceVersion, managedFields)
// do not count as changes.
func DiffBetween(ctx context.Context, s *Store, prefix string, from, to Point, f Filter) (Diff, error) {
	fr, a, err := Objects(ctx, s, prefix, from, f)
	if err != nil {
		return Diff{}, err
	}
	tr, b, err := Objects(ctx, s, prefix, to, f)
	if err != nil {
		return Diff{}, err
	}
	d := Diff{FromRev: fr, ToRev: tr, Added: []Change{}, Removed: []Change{}, Changed: []Change{}}
	before := map[string]*Object{}
	for i := range a {
		before[a[i].Key] = &a[i]
	}
	for i := range b {
		o := &b[i]
		old, ok := before[o.Key]
		delete(before, o.Key)
		switch {
		case !ok:
			d.Added = append(d.Added, Change{Key: o.Key, Resource: o.Resource, To: o})
		case old.Revision != o.Revision && !reflect.DeepEqual(comparable(old.Object), comparable(o.Object)):
			d.Changed = append(d.Changed, Change{Key: o.Key, Resource: o.Resource, From: old, To: o})
		}
	}
	for _, o := range before {
		d.Removed = append(d.Removed, Change{Key: o.Key, Resource: o.Resource, From: o})
	}
	sort.Slice(d.Removed, func(i, j int) bool { return d.Removed[i].Key < d.Removed[j].Key })
	return d, nil
}

// comparable is an object without what changes on every write.
func comparable(obj map[string]any) map[string]any {
	if obj == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range obj {
		out[k] = v
	}
	if md, ok := obj["metadata"].(map[string]any); ok {
		m := map[string]any{}
		for k, v := range md {
			switch k {
			case "resourceVersion", "managedFields":
			default:
				m[k] = v
			}
		}
		out["metadata"] = m
	}
	return out
}

// Graves are the deleted objects under prefix that f matches, with what
// they last held.
func Graves(ctx context.Context, s *Store, prefix string, f Filter) ([]Object, error) {
	gs, err := s.Graveyard(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]Object, 0, len(gs))
	for _, g := range gs {
		o := Object{Key: g.Key, Resource: Resource(g.Key), DeletedAt: g.DeletedAt, DeletedTime: g.Time}
		if g.Last != nil {
			last := object(*g.Last, f.Reveal)
			o.Revision, o.Time, o.Object, o.Error = last.Revision, last.Time, last.Object, last.Error
		}
		if f.match(o) {
			out = append(out, o)
		}
	}
	return out, nil
}

// ParsePoint reads a revision or an RFC 3339 time; empty is now.
func ParsePoint(v string) (Point, error) {
	if v == "" {
		return Point{}, nil
	}
	if rev, err := strconv.ParseInt(v, 10, 64); err == nil {
		return Point{Rev: rev}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	return Point{At: t}, err
}

// Handler serves the tape decoded, read-only, as JSON, under Prefix:
//
//	GET info
//	GET objects?prefix=P&at=REV|RFC3339
//	GET history?key=K
//	GET graveyard?prefix=P
//	GET diff?prefix=P&from=REV|RFC3339&to=REV|RFC3339
//	GET raw?prefix=P&at=REV|RFC3339 (the stored values, for fantasies)
//
// namespace, kind and name narrow objects, graveyard and diff down.
// Secrets come with their data emptied unless reveal=1.
func Handler(s *Store) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any, err error) {
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	point := func(w http.ResponseWriter, v string) (Point, bool) {
		p, err := ParsePoint(v)
		if err != nil {
			http.Error(w, "a point is a revision or an RFC 3339 time: "+err.Error(), http.StatusBadRequest)
		}
		return p, err == nil
	}
	prefixOf := func(r *http.Request) string {
		if p := r.URL.Query().Get("prefix"); p != "" {
			return p
		}
		return "/registry/"
	}
	filter := func(r *http.Request) Filter {
		q := r.URL.Query()
		return Filter{Namespace: q.Get("namespace"), Kind: q.Get("kind"), Name: q.Get("name"), Reveal: q.Get("reveal") == "1"}
	}
	mux.HandleFunc("GET "+Prefix+"/info", func(w http.ResponseWriter, r *http.Request) {
		i, err := s.Info(r.Context())
		reply(w, i, err)
	})
	mux.HandleFunc("GET "+Prefix+"/objects", func(w http.ResponseWriter, r *http.Request) {
		p, ok := point(w, r.URL.Query().Get("at"))
		if !ok {
			return
		}
		rev, objs, err := Objects(r.Context(), s, prefixOf(r), p, filter(r))
		reply(w, map[string]any{"revision": rev, "objects": objs}, err)
	})
	mux.HandleFunc("GET "+Prefix+"/history", func(w http.ResponseWriter, r *http.Request) {
		kvs, err := s.History(r.Context(), r.URL.Query().Get("key"))
		out := make([]Object, 0, len(kvs))
		for _, kv := range kvs {
			out = append(out, object(kv, filter(r).Reveal))
		}
		reply(w, out, err)
	})
	mux.HandleFunc("GET "+Prefix+"/graveyard", func(w http.ResponseWriter, r *http.Request) {
		out, err := Graves(r.Context(), s, prefixOf(r), filter(r))
		reply(w, out, err)
	})
	// The stored values themselves, for seeding a fantasy: everything a
	// store held at a point, Secrets included.
	mux.HandleFunc("GET "+Prefix+"/raw", func(w http.ResponseWriter, r *http.Request) {
		p, ok := point(w, r.URL.Query().Get("at"))
		if !ok {
			return
		}
		res, err := s.Range(r.Context(), prefixOf(r), p)
		reply(w, res, err)
	})
	mux.HandleFunc("GET "+Prefix+"/diff", func(w http.ResponseWriter, r *http.Request) {
		from, ok := point(w, r.URL.Query().Get("from"))
		if !ok {
			return
		}
		to, ok := point(w, r.URL.Query().Get("to"))
		if !ok {
			return
		}
		d, err := DiffBetween(r.Context(), s, prefixOf(r), from, to, filter(r))
		reply(w, d, err)
	})
	return mux
}
