package main

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

// objectsIn reads the objects of manifest files, several to a file.
func objectsIn(files []string) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, doc := range bytes.Split(raw, []byte("\n---")) {
			if len(bytes.TrimSpace(doc)) == 0 {
				continue
			}
			u := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(doc, &u.Object); err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			if len(u.Object) > 0 {
				out = append(out, u)
			}
		}
	}
	return out, nil
}

// conflicts are the objects of the change that the real cluster wrote
// after the fantasy branched off: merging over them would apply a change
// tried against what is no longer there.
func conflicts(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, objs []*unstructured.Unstructured, branch int64) ([]string, error) {
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(cs.Discovery()))
	var out []string
	for _, o := range objs {
		gvk := o.GroupVersionKind()
		m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, err
		}
		var ri dynamic.ResourceInterface = dyn.Resource(m.Resource)
		if m.Scope.Name() == meta.RESTScopeNameNamespace {
			ns := o.GetNamespace()
			if ns == "" {
				ns = "default"
			}
			ri = dyn.Resource(m.Resource).Namespace(ns)
		}
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

func mergeError(cs []string) error {
	return fmt.Errorf("not merged, the cluster moved on:\n  %s\nplay the fantasy again from now", strings.Join(cs, "\n  "))
}
