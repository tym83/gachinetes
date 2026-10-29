package tape

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

// stored encodes an object as the API server stores it.
func stored(t *testing.T, obj runtime.Object) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := protoDecoder.Encode(obj, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func meta(name, ns, uid string, owner ...metav1.OwnerReference) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid), ResourceVersion: "42", OwnerReferences: owner}
}

func controlledBy(kind, name, uid string) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "apps/v1", Kind: kind, Name: name, UID: types.UID(uid), Controller: ptr.To(true)}
}

// A namespace "shop" deleted with what was in it: a deployment, its
// replica set and pod, a service, a secret, a token, an event.
func shopGraves(t *testing.T) []Grave {
	t.Helper()
	gv := func(o runtime.Object, kind string) runtime.Object {
		o.GetObjectKind().SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(kind))
		return o
	}
	apps := func(o runtime.Object, kind string) runtime.Object {
		o.GetObjectKind().SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind(kind))
		return o
	}
	ns := &corev1.Namespace{ObjectMeta: meta("shop", "", "ns-1")}
	now := metav1.Now()
	ns.DeletionTimestamp = &now
	objs := map[string]runtime.Object{
		"/registry/namespaces/shop":                  gv(ns, "Namespace"),
		"/registry/deployments/shop/web":             apps(&appsv1.Deployment{ObjectMeta: meta("web", "shop", "dep-1")}, "Deployment"),
		"/registry/replicasets/shop/web-1":           apps(&appsv1.ReplicaSet{ObjectMeta: meta("web-1", "shop", "rs-1", controlledBy("Deployment", "web", "dep-1"))}, "ReplicaSet"),
		"/registry/pods/shop/web-1-a":                gv(&corev1.Pod{ObjectMeta: meta("web-1-a", "shop", "pod-1", controlledBy("ReplicaSet", "web-1", "rs-1"))}, "Pod"),
		"/registry/services/specs/shop/web":          gv(&corev1.Service{ObjectMeta: meta("web", "shop", "svc-1"), Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.7", ClusterIPs: []string{"10.96.0.7"}}}, "Service"),
		"/registry/secrets/shop/db":                  gv(&corev1.Secret{ObjectMeta: meta("db", "shop", "sec-1"), Data: map[string][]byte{"password": []byte("hunter2")}}, "Secret"),
		"/registry/secrets/shop/token":               gv(&corev1.Secret{ObjectMeta: meta("token", "shop", "sec-2"), Type: corev1.SecretTypeServiceAccountToken}, "Secret"),
		"/registry/events/shop/web.1":                gv(&corev1.Event{ObjectMeta: meta("web.1", "shop", "ev-1")}, "Event"),
		"/registry/configmaps/shop/kube-root-ca.crt": gv(&corev1.ConfigMap{ObjectMeta: meta("kube-root-ca.crt", "shop", "cm-1")}, "ConfigMap"),
	}
	var out []Grave
	rev := int64(100)
	for key, o := range objs {
		rev++
		out = append(out, Grave{Key: key, DeletedAt: rev, Time: rev * 1000,
			Last: &KV{Key: key, Value: stored(t, o), ModRevision: rev - 50}})
	}
	return out
}

func TestDecodeAndRedact(t *testing.T) {
	for _, g := range shopGraves(t) {
		u, err := Decode(g.Last.Value)
		if err != nil {
			t.Fatalf("%s: %v", g.Key, err)
		}
		if u.GetKind() == "" || u.GetName() == "" {
			t.Fatalf("%s: decoded without kind or name: %v", g.Key, u.Object)
		}
		if u.GetName() == "db" {
			Redact(u)
			if v, _, _ := unstructured.NestedString(u.Object, "data", "password"); v != "" {
				t.Fatalf("redacted secret still holds %q", v)
			}
		}
	}
	if _, err := Decode([]byte("k8s:enc:aescbc:v1:key1:xxx")); err != ErrEncrypted {
		t.Fatalf("encrypted: %v", err)
	}
	if u, err := Decode([]byte(`{"apiVersion":"x.io/v1","kind":"Thing","metadata":{"name":"a"}}`)); err != nil || u.GetKind() != "Thing" {
		t.Fatalf("custom resource: %v %v", u, err)
	}
	if r := Resource("/registry/x.io/things/ns/a"); r != "things.x.io" {
		t.Fatalf("resource of a custom resource key: %s", r)
	}
}

func objectsOf(t *testing.T, gs []Grave) []Object {
	t.Helper()
	var out []Object
	for _, g := range gs {
		o := object(*g.Last, true)
		o.DeletedAt = g.DeletedAt
		out = append(out, o)
	}
	return out
}

// A namespace comes back: the namespace first, owners before what they
// own, without what the cluster makes again, with owners rewired.
func TestPlanRestoresANamespace(t *testing.T) {
	steps, skipped := Plan(objectsOf(t, shopGraves(t)))
	var order []string
	for _, s := range steps {
		order = append(order, s.Object.GetKind()+"/"+s.Object.GetName())
		if s.Object.GetResourceVersion() != "" || s.Object.GetUID() != "" {
			t.Errorf("%s keeps its old identity", s.Object.GetName())
		}
	}
	if len(steps) != 4 || order[0] != "Namespace/shop" {
		t.Fatalf("restore order: %v", order)
	}
	for _, s := range steps {
		if s.Object.GetKind() == "Service" {
			if ip, ok, _ := unstructured.NestedString(s.Object.Object, "spec", "clusterIP"); ok {
				t.Errorf("service keeps its cluster IP %s", ip)
			}
		}
		if s.Object.GetKind() == "Namespace" && s.Object.GetDeletionTimestamp() != nil {
			t.Error("namespace comes back being deleted")
		}
	}
	if len(skipped) != 5 {
		t.Fatalf("skipped: %+v", skipped)
	}
}

// A replica set restored alone keeps no reference to its deployment,
// which is gone: the garbage collector would delete it again.
func TestPlanDropsReferencesToOwnersNotRestored(t *testing.T) {
	var rs []Object
	for _, o := range objectsOf(t, shopGraves(t)) {
		if (&unstructured.Unstructured{Object: o.Object}).GetKind() == "ReplicaSet" {
			rs = append(rs, o)
		}
	}
	steps, _ := Plan(rs)
	if len(steps) != 1 || len(steps[0].Object.GetOwnerReferences()) != 0 {
		t.Fatalf("replica set alone: %+v", steps)
	}
}

func TestRewire(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Deployment", Name: "web", UID: "old"}})
	Rewire(u, map[types.UID]types.UID{"old": "new"})
	if u.GetOwnerReferences()[0].UID != "new" {
		t.Fatal("owner not rewired")
	}
}

// fakeStore serves a store's tape: the graveyard, and two ranges.
func fakeStore(t *testing.T) *httptest.Server {
	graves := shopGraves(t)
	then := Range{Revision: 10, KVs: []KV{
		{Key: "/registry/configmaps/a/x", Value: stored(t, cm("x", "1")), ModRevision: 5},
		{Key: "/registry/configmaps/a/y", Value: stored(t, cm("y", "1")), ModRevision: 6},
	}}
	now := Range{Revision: 20, KVs: []KV{
		{Key: "/registry/configmaps/a/x", Value: stored(t, cm("x", "2")), ModRevision: 15},
		{Key: "/registry/configmaps/a/z", Value: stored(t, cm("z", "1")), ModRevision: 16},
	}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v any
		switch r.URL.Path {
		case "/tape/v1/graveyard":
			v = graves
		case "/tape/v1/range":
			v = now
			if r.URL.Query().Get("rev") == "10" {
				v = then
			}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
}

func cm(name, v string) *corev1.ConfigMap {
	c := &corev1.ConfigMap{ObjectMeta: meta(name, "a", "cm-"+name), Data: map[string]string{"v": v}}
	c.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
	return c
}

func TestDiffAndFilters(t *testing.T) {
	srv := fakeStore(t)
	defer srv.Close()
	s := &Store{URL: srv.URL}
	d, err := DiffBetween(context.Background(), s, "/registry/", Point{Rev: 10}, Point{}, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Added) != 1 || len(d.Removed) != 1 || len(d.Changed) != 1 || d.Changed[0].Key != "/registry/configmaps/a/x" {
		t.Fatalf("diff: %+v", d)
	}
	gs, err := Graves(context.Background(), s, "/registry/", Filter{Namespace: "shop", Kind: "Deployment.apps"})
	if err != nil || len(gs) != 1 || gs[0].DeletedAt == 0 {
		t.Fatalf("graves of a kind: %+v %v", gs, err)
	}
	gs, _ = Graves(context.Background(), s, "/registry/", Filter{Namespace: "shop", Name: "db"})
	if v, _, _ := unstructured.NestedString(gs[0].Object, "data", "password"); v != "" {
		t.Fatalf("secret revealed without reveal: %q", v)
	}
}
