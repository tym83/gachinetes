package fantasy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"
)

// ObjectsIn reads the objects of manifest files, several to a file.
func ObjectsIn(files []string) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		objs, err := objectsOf(f, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, objs...)
	}
	return out, nil
}

// ObjectsFrom reads the objects of manifests held by key, in the order of
// keys.
func ObjectsFrom(manifests map[string]string, keys []string) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, k := range keys {
		objs, err := objectsOf(k, []byte(manifests[k]))
		if err != nil {
			return nil, err
		}
		out = append(out, objs...)
	}
	return out, nil
}

func objectsOf(name string, raw []byte) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, doc := range bytes.Split(raw, []byte("\n---")) {
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		u := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(doc, &u.Object); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if len(u.Object) == 0 {
			continue
		}
		// A List, as kubectl get -o yaml writes, applies item by item.
		if u.IsList() {
			l, err := u.ToList()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			for i := range l.Items {
				out = append(out, &l.Items[i])
			}
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

// Conflicts are the objects of the change that the real cluster wrote
// after the fantasy branched off: merging over them would apply a change
// tried against what is no longer there.
func Conflicts(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, objs []*unstructured.Unstructured, branch int64) ([]string, error) {
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(cs.Discovery()))
	var out []string
	for _, o := range objs {
		ri, err := resourceFor(mapper, dyn, o)
		if err != nil {
			return nil, err
		}
		gvk := o.GroupVersionKind()
		live, err := ri.Get(ctx, o.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		rv, err := strconv.ParseInt(live.GetResourceVersion(), 10, 64)
		if err != nil {
			return nil, err
		}
		if rv > branch {
			out = append(out, fmt.Sprintf("%s %s was written at revision %d, after the fantasy branched at %d",
				gvk.Kind, o.GetName(), rv, branch))
		}
	}
	return out, nil
}

// MergeError tells why a change was not merged.
func MergeError(cs []string) error {
	return fmt.Errorf("not merged, the cluster moved on:\n  %s\nplay the fantasy again from now", strings.Join(cs, "\n  "))
}

func resourceFor(mapper meta.RESTMapper, dyn dynamic.Interface, o *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	gvk := o.GroupVersionKind()
	m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}
	if m.Scope.Name() != meta.RESTScopeNameNamespace {
		return dyn.Resource(m.Resource), nil
	}
	ns := o.GetNamespace()
	if ns == "" {
		ns = "default"
	}
	return dyn.Resource(m.Resource).Namespace(ns), nil
}

// Merge applies the change to the real cluster, server-side as the
// fantasy did, when the cluster has not written the change's objects since
// the branch.
func Merge(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, objs []*unstructured.Unstructured, branch int64) ([]string, error) {
	cfls, err := Conflicts(ctx, cs, dyn, objs, branch)
	if err != nil {
		return nil, err
	}
	if len(cfls) > 0 {
		return nil, MergeError(cfls)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(cs.Discovery()))
	var done []string
	for _, o := range objs {
		ri, err := resourceFor(mapper, dyn, o)
		if err != nil {
			return done, err
		}
		if _, err := ri.Apply(ctx, o.GetName(), o, metav1.ApplyOptions{FieldManager: "fantasy", Force: true}); err != nil {
			return done, fmt.Errorf("%s %s: %w", o.GetKind(), o.GetName(), err)
		}
		done = append(done, o.GetKind()+" "+o.GetName())
	}
	return done, nil
}
