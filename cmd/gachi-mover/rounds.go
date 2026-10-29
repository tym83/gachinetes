package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/tym83/gachinetes/pkg/mover"
)

const (
	// RoundAnno asks for a checkpoint of a pod every so often, such as
	// 10m; the pod comes back from its last round if it fails or its
	// node is lost.
	RoundAnno = "gym.gachinetes.dev/round"
	// roundsLabel marks the records of rounds, ConfigMaps in the mover's
	// namespace.
	roundsLabel = "gym.gachinetes.dev/rounds-of"
)

// round is the record of a pod's last round: where its checkpoints are
// held as images, and the pod to make again from them.
type round struct {
	Namespace string            `json:"namespace"`
	Pod       string            `json:"pod"`
	Node      string            `json:"node"`
	Every     string            `json:"every"`
	Images    map[string]string `json:"images"`
	At        time.Time         `json:"at"`
	Spec      corev1.PodSpec    `json:"spec"`
	Labels    map[string]string `json:"labels,omitempty"`
}

func recordName(ns, pod string) string { return "round-" + ns + "-" + pod }

// Rounds takes the rounds that are due and brings back the pods that went.
func (c *Controller) Rounds(ctx context.Context) {
	pods, err := c.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("rounds: %v", err)
		return
	}
	seen, plain := map[string]bool{}, map[string]bool{}
	for i := range pods.Items {
		p := &pods.Items[i]
		every, err := time.ParseDuration(p.Annotations[RoundAnno])
		if err != nil || every <= 0 {
			plain[recordName(p.Namespace, p.Name)] = true
			continue
		}
		seen[recordName(p.Namespace, p.Name)] = true
		switch p.Status.Phase {
		case corev1.PodRunning:
			c.takeRound(ctx, p, every)
		case corev1.PodFailed:
			c.comeBack(ctx, recordName(p.Namespace, p.Name), p)
		}
	}
	// Pods that went with their node: their records outlive them.
	records, err := c.cs.CoreV1().ConfigMaps(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: roundsLabel})
	if err != nil {
		return
	}
	for _, cm := range records.Items {
		switch {
		case plain[cm.Name]:
			// Its pod takes rounds no more.
			_ = c.cs.CoreV1().ConfigMaps(c.namespace).Delete(ctx, cm.Name, metav1.DeleteOptions{})
		case !seen[cm.Name]:
			c.comeBack(ctx, cm.Name, nil)
		}
	}
}

func (c *Controller) loadRound(ctx context.Context, name string) (*round, error) {
	cm, err := c.cs.CoreV1().ConfigMaps(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var r round
	return &r, json.Unmarshal([]byte(cm.Data["round"]), &r)
}

func (c *Controller) saveRound(ctx context.Context, r *round) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	name := recordName(r.Namespace, r.Pod)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.namespace, Labels: map[string]string{roundsLabel: r.Namespace + "." + r.Pod}},
		Data:       map[string]string{"round": string(raw)},
	}
	api := c.cs.CoreV1().ConfigMaps(c.namespace)
	cur, err := api.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, cm, metav1.CreateOptions{FieldManager: "gachi-mover"})
		return err
	}
	if err != nil {
		return err
	}
	cm.ResourceVersion = cur.ResourceVersion
	_, err = api.Update(ctx, cm, metav1.UpdateOptions{FieldManager: "gachi-mover"})
	return err
}

// takeRound checkpoints a pod when its round is due, and keeps the
// checkpoints as images on another node, where it would come back.
func (c *Controller) takeRound(ctx context.Context, p *corev1.Pod, every time.Duration) {
	name := recordName(p.Namespace, p.Name)
	if last, err := c.loadRound(ctx, name); err == nil && time.Since(last.At) < every {
		return
	}
	// A round that failed is tried again a round later, not at once.
	if t, ok := c.failedRound[name]; ok && time.Since(t) < every {
		return
	}
	failed := true
	defer func() {
		if failed {
			c.failedRound[name] = time.Now()
		} else {
			delete(c.failedRound, name)
		}
	}()
	spare, err := c.otherNode(ctx, p.Spec.NodeName)
	if err != nil {
		log.Printf("round of %s/%s: %v", p.Namespace, p.Name, err)
		return
	}
	src, err := c.agentAddr(ctx, p.Spec.NodeName)
	if err != nil {
		return
	}
	dst, err := c.agentAddr(ctx, spare)
	if err != nil {
		return
	}
	token, err := c.token(ctx)
	if err != nil {
		return
	}
	now := time.Now()
	r := &round{Namespace: p.Namespace, Pod: p.Name, Node: spare, Every: p.Annotations[RoundAnno],
		Images: map[string]string{}, At: now, Spec: p.Spec, Labels: p.Labels}
	for _, ctr := range p.Spec.Containers {
		path, err := c.checkpoint(ctx, p.Spec.NodeName, p.Namespace, p.Name, ctr.Name)
		if err != nil {
			log.Printf("round of %s/%s: %v", p.Namespace, p.Name, err)
			return
		}
		ref := mover.ImageRef(p.Namespace, p.Name, ctr.Name, now)
		if err := c.transfer(ctx, token, src, dst, path, ref, ctr.Name); err != nil {
			log.Printf("round of %s/%s: %v", p.Namespace, p.Name, err)
			return
		}
		_ = c.agent(ctx, token, http.MethodDelete, src, "/checkpoint", url.Values{"path": {path}}, nil)
		r.Images[ctr.Name] = ref
	}
	if err := c.saveRound(ctx, r); err != nil {
		log.Printf("round of %s/%s: %v", p.Namespace, p.Name, err)
		return
	}
	failed = false
	log.Printf("♂ round of %s/%s: held on %s (%s)", p.Namespace, p.Name, spare, time.Since(now).Round(time.Millisecond))
}

// comeBack makes a pod again from its last round, on the node that holds
// it; failed is the failed pod, or nil when the pod is gone.
func (c *Controller) comeBack(ctx context.Context, name string, failed *corev1.Pod) {
	r, err := c.loadRound(ctx, name)
	if err != nil {
		return
	}
	if failed != nil {
		if err := c.cs.CoreV1().Pods(r.Namespace).Delete(ctx, r.Pod, metav1.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0))}); err != nil && !apierrors.IsNotFound(err) {
			return
		}
		if _, err := c.cs.CoreV1().Pods(r.Namespace).Get(ctx, r.Pod, metav1.GetOptions{}); err == nil {
			return // still going; next time
		}
	}
	spec := r.Spec.DeepCopy()
	spec.NodeName = r.Node
	for i := range spec.Containers {
		if ref, ok := r.Images[spec.Containers[i].Name]; ok {
			spec.Containers[i].Image = ref
			spec.Containers[i].ImagePullPolicy = corev1.PullNever
		}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: r.Pod, Namespace: r.Namespace, Labels: r.Labels,
		Annotations: map[string]string{
			// The pod keeps taking rounds as before.
			RoundAnno:                       r.Every,
			"gym.gachinetes.dev/came-back":  r.At.UTC().Format(time.RFC3339),
			"gym.gachinetes.dev/round-node": r.Node,
		}}, Spec: *spec}
	if _, err := c.cs.CoreV1().Pods(r.Namespace).Create(ctx, pod, metav1.CreateOptions{FieldManager: "gachi-mover"}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			log.Printf("bringing %s/%s back: %v", r.Namespace, r.Pod, err)
		}
		return
	}
	// The record goes with the round it was: the pod takes new ones.
	_ = c.cs.CoreV1().ConfigMaps(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	log.Printf("♂ %s/%s came back on %s from its round of %s", r.Namespace, r.Pod, r.Node, r.At.Format(time.TimeOnly))
}
