package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/tym83/gachinetes/pkg/belts"
	"github.com/tym83/gachinetes/pkg/wrestling"
)

// keepOver is how long a bout that is over stays to be read.
const keepOver = time.Hour

// announcer keeps a Bout object for every bout, and tells the object's
// watchers with an event when one starts.
type announcer struct {
	cs     kubernetes.Interface
	dyn    dynamic.Interface
	active map[string]bool
}

func (a *announcer) sync(ctx context.Context, bouts []wrestling.Bout) {
	now := map[string]bool{}
	for _, b := range bouts {
		name := b.Name()
		now[name] = true
		if err := a.upsert(ctx, name, b, "Fighting"); err != nil {
			log.Printf("bout %s: %v", name, err)
			continue
		}
		if !a.active[name] {
			a.event(ctx, b)
		}
	}
	// Bouts no longer fought are over, and go after a while.
	list, err := a.dyn.Resource(boutsGVR).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, item := range list.Items {
			if now[item.GetName()] {
				continue
			}
			phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
			last, _, _ := unstructured.NestedString(item.Object, "status", "lastSeen")
			t, _ := time.Parse(time.RFC3339, last)
			switch {
			case phase == "Fighting":
				_ = unstructured.SetNestedField(item.Object, "Over", "status", "phase")
				_, err = a.dyn.Resource(boutsGVR).UpdateStatus(ctx, &item, metav1.UpdateOptions{FieldManager: "gachi-referee"})
			case time.Since(t) > keepOver:
				err = a.dyn.Resource(boutsGVR).Delete(ctx, item.GetName(), metav1.DeleteOptions{})
			}
			if err != nil {
				log.Printf("bout %s: %v", item.GetName(), err)
			}
		}
	}
	a.active = now
}

func (a *announcer) upsert(ctx context.Context, name string, b wrestling.Bout, phase string) error {
	fighters := map[string]any{}
	for m, n := range b.Fighters {
		fighters[m] = int64(n)
	}
	obj := map[string]any{
		"group": b.Object.Group, "kind": b.Object.Kind, "namespace": b.Object.Namespace, "name": b.Object.Name,
	}
	spec := map[string]any{"object": obj, "path": b.Path}
	status := map[string]any{
		"phase": phase, "fighters": fighters, "switches": int64(b.Switches),
		"firstSeen": b.FirstSeen.UTC().Format(time.RFC3339), "lastSeen": b.LastSeen.UTC().Format(time.RFC3339),
		"summary": summary(b),
	}
	r := a.dyn.Resource(boutsGVR)
	cur, err := r.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": belts.Group + "/" + belts.Version, "kind": "Bout",
			"metadata": map[string]any{"name": name}, "spec": spec,
		}}
		if cur, err = r.Create(ctx, u, metav1.CreateOptions{FieldManager: "gachi-referee"}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	cur.Object["status"] = status
	_, err = r.UpdateStatus(ctx, cur, metav1.UpdateOptions{FieldManager: "gachi-referee"})
	return err
}

// summary is the bout in a line: who, how many rounds each.
func summary(b wrestling.Bout) string {
	var names []string
	for m := range b.Fighters {
		names = append(names, m)
	}
	sort.Strings(names)
	var parts []string
	for _, m := range names {
		parts = append(parts, fmt.Sprintf("%s ×%d", m, b.Fighters[m]))
	}
	return strings.Join(parts, " vs ")
}

func (a *announcer) event(ctx context.Context, b wrestling.Bout) {
	ns := b.Object.Namespace
	if ns == "" {
		ns = "default"
	}
	apiVersion := "v1"
	if b.Object.Group != "" {
		apiVersion = b.Object.Group + "/v1"
	}
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: b.Object.Name + ".wrestling.", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{APIVersion: apiVersion, Kind: b.Object.Kind,
			Namespace: b.Object.Namespace, Name: b.Object.Name},
		Reason:  "Wrestling",
		Message: fmt.Sprintf("♂ %s is fought over: %s, %d changes of hands. See bout %s.", b.Path, summary(b), b.Switches, b.Name()),
		Type:    corev1.EventTypeWarning, Source: corev1.EventSource{Component: "gachi-referee"},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	if _, err := a.cs.CoreV1().Events(ns).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		log.Printf("event for bout %s: %v", b.Name(), err)
	}
}
