package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/tym83/gachinetes/pkg/belts"
	"github.com/tym83/gachinetes/pkg/coach"
)

var (
	plansGVR   = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "trainingplans"}
	podMetrics = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}
)

// trainer carries out TrainingPlans: the requests of a Deployment's pods,
// resized in place as the coach calls the sets.
type trainer struct {
	cs  kubernetes.Interface
	dyn dynamic.Interface
}

type bounds struct {
	Min string `json:"min"`
	Max string `json:"max"`
}

type planSpec struct {
	Deployment  string  `json:"deployment"`
	Container   string  `json:"container"`
	CPU         *bounds `json:"cpu,omitempty"`
	Memory      *bounds `json:"memory,omitempty"`
	Step        float64 `json:"step,omitempty"`
	Rest        string  `json:"rest,omitempty"`
	DeloadAfter string  `json:"deloadAfter,omitempty"`
}

func (t *trainer) run(ctx context.Context) {
	list, err := t.dyn.Resource(plansGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for i := range list.Items {
		if err := t.train(ctx, &list.Items[i]); err != nil {
			log.Printf("training plan %s/%s: %v", list.Items[i].GetNamespace(), list.Items[i].GetName(), err)
		}
	}
}

func (t *trainer) train(ctx context.Context, u *unstructured.Unstructured) error {
	var spec planSpec
	raw, _ := json.Marshal(u.Object["spec"])
	if err := json.Unmarshal(raw, &spec); err != nil {
		return err
	}
	ns := u.GetNamespace()
	dep, err := t.cs.AppsV1().Deployments(ns).Get(ctx, spec.Deployment, metav1.GetOptions{})
	if err != nil {
		return err
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return err
	}
	pods, err := t.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return err
	}
	var running []corev1.Pod
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			running = append(running, p)
		}
	}
	if len(running) == 0 {
		return nil
	}
	// Use: the heaviest pod's; request: as the first pod has it.
	use := map[corev1.ResourceName]int64{}
	for _, p := range running {
		m, err := t.dyn.Resource(podMetrics).Namespace(ns).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		ctrs, _, _ := unstructured.NestedSlice(m.Object, "containers")
		for _, c := range ctrs {
			cm, _ := c.(map[string]any)
			if cm["name"] != spec.Container {
				continue
			}
			usage, _ := cm["usage"].(map[string]any)
			for _, r := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
				if s, ok := usage[string(r)].(string); ok {
					q := resource.MustParse(s)
					use[r] = max(use[r], value(r, q))
				}
			}
		}
	}
	if len(use) == 0 {
		return fmt.Errorf("no metrics for %s yet: is metrics-server running?", spec.Deployment)
	}
	var ctr *corev1.Container
	for i := range running[0].Spec.Containers {
		if running[0].Spec.Containers[i].Name == spec.Container {
			ctr = &running[0].Spec.Containers[i]
		}
	}
	if ctr == nil {
		return fmt.Errorf("no container %s", spec.Container)
	}
	records := map[string]coach.Record{}
	if st, ok := u.Object["status"].(map[string]any); ok {
		raw, _ := json.Marshal(st["records"])
		_ = json.Unmarshal(raw, &records)
	}
	rest, _ := time.ParseDuration(spec.Rest)
	deload, _ := time.ParseDuration(spec.DeloadAfter)
	now := time.Now()
	want := corev1.ResourceList{}
	whys := map[string]any{}
	for r, b := range map[corev1.ResourceName]*bounds{corev1.ResourceCPU: spec.CPU, corev1.ResourceMemory: spec.Memory} {
		if b == nil {
			continue
		}
		minQ, maxQ := resource.MustParse(b.Min), resource.MustParse(b.Max)
		p := coach.Plan{Min: value(r, minQ), Max: value(r, maxQ), Step: spec.Step, Rest: rest, DeloadAfter: deload}
		req := ctr.Resources.Requests[r]
		set, rec := coach.Train(p, value(r, req), use[r], records[string(r)], now)
		records[string(r)] = rec
		whys[string(r)] = set.Why
		if set.Request != value(r, req) {
			want[r] = quantity(r, set.Request)
		}
	}
	if len(want) > 0 {
		for _, p := range running {
			if err := t.resize(ctx, &p, spec.Container, want); err != nil {
				whys["resize"] = err.Error()
			}
		}
	}
	u.Object["status"] = map[string]any{"records": toMap(records), "why": whys, "time": now.UTC().Format(time.RFC3339),
		"requests": map[string]any{"cpu": quantityString(ctr.Resources.Requests, want, corev1.ResourceCPU), "memory": quantityString(ctr.Resources.Requests, want, corev1.ResourceMemory)}}
	_, err = t.dyn.Resource(plansGVR).Namespace(ns).UpdateStatus(ctx, u, metav1.UpdateOptions{FieldManager: "gachi-referee"})
	return err
}

// resize changes a container's requests in place, raising its limits
// where they would fall below.
func (t *trainer) resize(ctx context.Context, p *corev1.Pod, container string, want corev1.ResourceList) error {
	var c corev1.Container
	for _, x := range p.Spec.Containers {
		if x.Name == container {
			c = x
		}
	}
	req, lim := map[string]string{}, map[string]string{}
	for r, q := range want {
		req[string(r)] = q.String()
		if l, ok := c.Resources.Limits[r]; ok && l.Cmp(q) < 0 {
			lim[string(r)] = q.String()
		}
	}
	res := map[string]any{"requests": req}
	if len(lim) > 0 {
		res["limits"] = lim
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": container, "resources": res}}}})
	_, err := t.cs.CoreV1().Pods(p.Namespace).Patch(ctx, p.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: "gachi-coach"}, "resize")
	return err
}

// value is a quantity in the coach's units: millicores, bytes.
func value(r corev1.ResourceName, q resource.Quantity) int64 {
	if r == corev1.ResourceCPU {
		return q.MilliValue()
	}
	return q.Value()
}

func quantity(r corev1.ResourceName, v int64) resource.Quantity {
	if r == corev1.ResourceCPU {
		return *resource.NewMilliQuantity(v, resource.DecimalSI)
	}
	return *resource.NewQuantity(v, resource.BinarySI)
}

func quantityString(cur, want corev1.ResourceList, r corev1.ResourceName) string {
	if q, ok := want[r]; ok {
		return q.String()
	}
	q := cur[r]
	return q.String()
}

func toMap(v any) map[string]any {
	raw, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}
