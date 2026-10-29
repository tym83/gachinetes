package main

import (
	"context"
	"encoding/json"
	"log"
	"math/rand/v2"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/tym83/gachinetes/pkg/belts"
	"github.com/tym83/gachinetes/pkg/sparring"
)

var sparringsGVR = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "sparrings"}

// partner spars: on schedule it kills a pod of a Deployment and times
// how long the Deployment takes to be fully ready again.
type partner struct {
	cs  kubernetes.Interface
	dyn dynamic.Interface
}

type sparSpec struct {
	Deployment    string `json:"deployment"`
	Every         string `json:"every"`
	RecoverWithin string `json:"recoverWithin"`
}

func (p *partner) run(ctx context.Context) {
	list, err := p.dyn.Resource(sparringsGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for i := range list.Items {
		if err := p.spar(ctx, &list.Items[i]); err != nil {
			log.Printf("sparring %s/%s: %v", list.Items[i].GetNamespace(), list.Items[i].GetName(), err)
		}
	}
}

func (p *partner) spar(ctx context.Context, u *unstructured.Unstructured) error {
	var spec sparSpec
	raw, _ := json.Marshal(u.Object["spec"])
	if err := json.Unmarshal(raw, &spec); err != nil {
		return err
	}
	every, err := time.ParseDuration(spec.Every)
	if err != nil {
		every = 10 * time.Minute
	}
	limit, err := time.ParseDuration(spec.RecoverWithin)
	if err != nil {
		limit = time.Minute
	}
	var rounds []sparring.Round
	if st, ok := u.Object["status"].(map[string]any); ok {
		raw, _ := json.Marshal(st["rounds"])
		_ = json.Unmarshal(raw, &rounds)
	}
	ns, now := u.GetNamespace(), time.Now()
	dep, err := p.cs.AppsV1().Deployments(ns).Get(ctx, spec.Deployment, metav1.GetOptions{})
	if err != nil {
		return err
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	changed := false
	if n := len(rounds); n > 0 && !rounds[n-1].Done {
		r := &rounds[n-1]
		_, err := p.cs.CoreV1().Pods(ns).Get(ctx, r.Victim, metav1.GetOptions{})
		gone := apierrors.IsNotFound(err)
		switch {
		case gone && dep.Status.ReadyReplicas >= want && dep.Status.UpdatedReplicas >= want:
			r.Done, r.Won, r.Seconds = true, true, now.Sub(r.At).Seconds()
			changed = true
		case now.Sub(r.At) > limit:
			r.Done, r.Seconds = true, now.Sub(r.At).Seconds()
			changed = true
		}
	} else if n == 0 || now.Sub(rounds[n-1].At) >= every {
		// A punch: a running pod of the Deployment, at random.
		sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
		if err != nil {
			return err
		}
		pods, err := p.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
		if err != nil {
			return err
		}
		var targets []corev1.Pod
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil {
				targets = append(targets, pod)
			}
		}
		if len(targets) > 0 && dep.Status.ReadyReplicas >= want {
			victim := targets[rand.IntN(len(targets))]
			if err := p.cs.CoreV1().Pods(ns).Delete(ctx, victim.Name, metav1.DeleteOptions{}); err != nil {
				return err
			}
			rounds = append(rounds, sparring.Round{At: now, Victim: victim.Name})
			changed = true
			log.Printf("♂ sparring: punched %s/%s", ns, victim.Name)
		}
	}
	if !changed {
		return nil
	}
	rounds = sparring.Keep(rounds, 20)
	rank, wins, losses := sparring.Rank(rounds)
	u.Object["status"] = map[string]any{"rounds": toMapList(rounds), "rank": rank, "wins": int64(wins), "losses": int64(losses)}
	_, err = p.dyn.Resource(sparringsGVR).Namespace(ns).UpdateStatus(ctx, u, metav1.UpdateOptions{FieldManager: "gachi-referee"})
	return err
}

func toMapList(v any) []any {
	raw, _ := json.Marshal(v)
	var out []any
	_ = json.Unmarshal(raw, &out)
	return out
}

