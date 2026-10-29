package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	"github.com/tym83/gachinetes/pkg/belts"
)

var awayGVR = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "awaymatches"}

// coachBus takes teams to away matches: when a Deployment's pods find no
// place at home for a while, as many play in another cluster, and come
// home when there is room again.
type coachBus struct {
	cs  kubernetes.Interface
	dyn dynamic.Interface
}

type awaySpec struct {
	Deployment       string `json:"deployment"`
	KubeconfigSecret string `json:"kubeconfigSecret"`
	After            string `json:"after"`
}

func (b *coachBus) run(ctx context.Context) {
	list, err := b.dyn.Resource(awayGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for i := range list.Items {
		if err := b.match(ctx, &list.Items[i]); err != nil {
			log.Printf("away match %s/%s: %v", list.Items[i].GetNamespace(), list.Items[i].GetName(), err)
		}
	}
}

func unschedulable(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodPending || p.Spec.NodeName != "" {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return true
		}
	}
	return false
}

func (b *coachBus) match(ctx context.Context, u *unstructured.Unstructured) error {
	var spec awaySpec
	raw, _ := json.Marshal(u.Object["spec"])
	if err := json.Unmarshal(raw, &spec); err != nil {
		return err
	}
	after, err := time.ParseDuration(spec.After)
	if err != nil {
		after = 2 * time.Minute
	}
	ns, now := u.GetNamespace(), time.Now()
	dep, err := b.cs.AppsV1().Deployments(ns).Get(ctx, spec.Deployment, metav1.GetOptions{})
	if err != nil {
		return err
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return err
	}
	pods, err := b.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return err
	}
	pending := 0
	for i := range pods.Items {
		if unschedulable(&pods.Items[i]) {
			pending++
		}
	}
	status, _ := u.Object["status"].(map[string]any)
	if status == nil {
		status = map[string]any{}
	}
	away, _ := status["away"].(int64)
	if f, ok := status["away"].(float64); ok {
		away = int64(f)
	}
	since, _ := time.Parse(time.RFC3339, fmt.Sprint(status["since"]))
	// since: when the home side last changed between needing help and not.
	needs := pending > 0
	was, _ := status["needs"].(bool)
	if needs != was || since.IsZero() {
		since = now
	}
	// Away replicas follow what finds no place at home: home pods stay
	// pending as the claim on room there, the away ones play meanwhile.
	want := away
	switch {
	case needs && now.Sub(since) >= after:
		want = int64(pending)
	case !needs && now.Sub(since) >= after:
		want = 0
	}
	msg := fmt.Sprintf("%d without a place at home, %d away", pending, want)
	if want != away {
		if err := b.field(ctx, ns, spec, dep, int32(want)); err != nil {
			msg = err.Error()
			want = away
		} else {
			log.Printf("♂ away match %s/%s: %d away", ns, u.GetName(), want)
		}
	}
	u.Object["status"] = map[string]any{"away": want, "pending": int64(pending), "needs": needs,
		"since": since.UTC().Format(time.RFC3339), "message": msg}
	_, err = b.dyn.Resource(awayGVR).Namespace(ns).UpdateStatus(ctx, u, metav1.UpdateOptions{FieldManager: "gachi-referee"})
	return err
}

// field sets how many of the team play away: a copy of the Deployment in
// the other cluster, in a namespace of the same name.
func (b *coachBus) field(ctx context.Context, ns string, spec awaySpec, home *appsv1.Deployment, replicas int32) error {
	sec, err := b.cs.CoreV1().Secrets(ns).Get(ctx, spec.KubeconfigSecret, metav1.GetOptions{})
	if err != nil {
		return err
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(sec.Data["kubeconfig"])
	if err != nil {
		return err
	}
	remote, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	if _, err := remote.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	labels := map[string]string{"gym.gachinetes.dev/away-from": home.Name}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: home.Name + "-away", Namespace: ns, Labels: labels},
		Spec:       *home.Spec.DeepCopy(),
	}
	d.Spec.Replicas = ptr.To(replicas)
	api := remote.AppsV1().Deployments(ns)
	cur, err := api.Get(ctx, d.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = api.Create(ctx, d, metav1.CreateOptions{FieldManager: "gachi-referee"})
	case err == nil:
		d.ResourceVersion = cur.ResourceVersion
		_, err = api.Update(ctx, d, metav1.UpdateOptions{FieldManager: "gachi-referee"})
	}
	return err
}
