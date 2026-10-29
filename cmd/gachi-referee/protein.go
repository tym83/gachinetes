package main

import (
	"context"
	"log"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/tym83/gachinetes/pkg/protein"
)

// kitchen makes what protein shakes ask for, in the referee's namespace,
// and clears away what no shake asks for any more.
type kitchen struct {
	cs        kubernetes.Interface
	namespace string
	shakes    cache.Store
}

func (k *kitchen) reconcile(ctx context.Context) {
	want := map[string]bool{}
	items := k.shakes.List()
	if len(items) > 0 {
		if err := k.priorityClass(ctx); err != nil {
			log.Printf("protein priority class: %v", err)
		}
	}
	for _, o := range items {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		s, err := protein.From(u)
		if err != nil {
			log.Print(err)
			continue
		}
		want[s.Name] = true
		if err := k.serve(ctx, s); err != nil {
			log.Printf("ProteinShake %s: %v", s.Name, err)
		}
	}
	k.clear(ctx, want)
}

func (k *kitchen) priorityClass(ctx context.Context) error {
	pc := protein.PriorityClassObject()
	_, err := k.cs.SchedulingV1().PriorityClasses().Create(ctx, pc, metav1.CreateOptions{FieldManager: "gachi-referee"})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (k *kitchen) serve(ctx context.Context, s protein.Shake) error {
	d, err := s.Placeholders(k.namespace)
	if err != nil {
		return err
	}
	deps := k.cs.AppsV1().Deployments(k.namespace)
	if d != nil {
		cur, err := deps.Get(ctx, d.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			_, err = deps.Create(ctx, d, metav1.CreateOptions{FieldManager: "gachi-referee"})
		case err == nil:
			d.ResourceVersion = cur.ResourceVersion
			_, err = deps.Update(ctx, d, metav1.UpdateOptions{FieldManager: "gachi-referee"})
		}
		if err != nil {
			return err
		}
	} else if err := deps.Delete(ctx, "protein-"+s.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	dss := k.cs.AppsV1().DaemonSets(k.namespace)
	if ds := s.Holders(k.namespace); ds != nil {
		cur, err := dss.Get(ctx, ds.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			_, err = dss.Create(ctx, ds, metav1.CreateOptions{FieldManager: "gachi-referee"})
		case err == nil:
			ds.ResourceVersion = cur.ResourceVersion
			_, err = dss.Update(ctx, ds, metav1.UpdateOptions{FieldManager: "gachi-referee"})
		}
		return err
	}
	if err := dss.Delete(ctx, "protein-"+s.Name+"-images", metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// clear deletes what shakes no longer ask for.
func (k *kitchen) clear(ctx context.Context, want map[string]bool) {
	req, _ := labels.NewRequirement(protein.Label, selection.Exists, nil)
	sel := labels.NewSelector().Add(*req).String()
	if list, err := k.cs.AppsV1().Deployments(k.namespace).List(ctx, metav1.ListOptions{LabelSelector: sel}); err == nil {
		for _, d := range list.Items {
			if !want[d.Labels[protein.Label]] {
				_ = k.cs.AppsV1().Deployments(k.namespace).Delete(ctx, d.Name, metav1.DeleteOptions{})
			}
		}
	}
	if list, err := k.cs.AppsV1().DaemonSets(k.namespace).List(ctx, metav1.ListOptions{LabelSelector: sel}); err == nil {
		for _, d := range list.Items {
			if !want[d.Labels[protein.Label]] {
				_ = k.cs.AppsV1().DaemonSets(k.namespace).Delete(ctx, d.Name, metav1.DeleteOptions{})
			}
		}
	}
}
