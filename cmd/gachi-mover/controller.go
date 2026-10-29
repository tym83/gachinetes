package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	"github.com/tym83/gachinetes/pkg/mover"
)

var migrationsGVR = schema.GroupVersionResource{Group: "gym.gachinetes.dev", Version: "v1", Resource: "migrations"}

// Controller carries Migrations out.
type Controller struct {
	cs        kubernetes.Interface
	dyn       dynamic.Interface
	namespace string
	agentPort string
	http      *http.Client
	// failedRound: when a pod's round last failed, to wait before the next.
	failedRound map[string]time.Time
}

func NewController(cfg *rest.Config, cs kubernetes.Interface, namespace, agentPort string) (*Controller, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	// Agents' certificates are their own, made at start; what keeps a
	// stranger out is the agent asking the API server about our token.
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}} //nolint:gosec
	return &Controller{cs: cs, dyn: dyn, namespace: namespace, agentPort: agentPort,
		http: &http.Client{Transport: tr, Timeout: 30 * time.Minute}, failedRound: map[string]time.Time{}}, nil
}

func (c *Controller) Run(ctx context.Context) {
	for {
		list, err := c.dyn.Resource(migrationsGVR).Namespace("").List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Printf("migrations: %v", err)
		} else {
			for i := range list.Items {
				m := &list.Items[i]
				if phase, _, _ := unstructured.NestedString(m.Object, "status", "phase"); phase == "" || phase == "Pending" {
					c.carryOut(ctx, m)
				}
			}
		}
		c.Rounds(ctx)
		time.Sleep(3 * time.Second)
	}
}

func (c *Controller) setStatus(ctx context.Context, m *unstructured.Unstructured, phase, msg string, extra map[string]any) {
	st := map[string]any{"phase": phase, "message": msg, "time": time.Now().UTC().Format(time.RFC3339)}
	for k, v := range extra {
		st[k] = v
	}
	m.Object["status"] = st
	updated, err := c.dyn.Resource(migrationsGVR).Namespace(m.GetNamespace()).UpdateStatus(ctx, m, metav1.UpdateOptions{FieldManager: "gachi-mover"})
	if err != nil {
		log.Printf("migration %s/%s: %v", m.GetNamespace(), m.GetName(), err)
	} else {
		// Later updates of this carrying out build on this one.
		m.SetResourceVersion(updated.GetResourceVersion())
	}
	log.Printf("migration %s/%s: %s %s", m.GetNamespace(), m.GetName(), phase, msg)
}

func (c *Controller) carryOut(ctx context.Context, m *unstructured.Unstructured) {
	ns := m.GetNamespace()
	podName, _, _ := unstructured.NestedString(m.Object, "spec", "pod")
	target, _, _ := unstructured.NestedString(m.Object, "spec", "node")
	fail := func(format string, args ...any) { c.setStatus(ctx, m, "Failed", fmt.Sprintf(format, args...), nil) }

	pod, err := c.cs.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		fail("pod %s: %v", podName, err)
		return
	}
	if ref := metav1.GetControllerOf(pod); ref != nil {
		fail("%s is %s %s's; pods with a controller are not moved yet", podName, ref.Kind, ref.Name)
		return
	}
	if pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
		fail("%s is not running", podName)
		return
	}
	source := pod.Spec.NodeName
	if target == "" {
		if target, err = c.otherNode(ctx, source); err != nil {
			fail("%v", err)
			return
		}
	}
	if target == source {
		fail("%s is on %s already", podName, target)
		return
	}
	srcAddr, err := c.agentAddr(ctx, source)
	if err != nil {
		fail("%v", err)
		return
	}
	dstAddr, err := c.agentAddr(ctx, target)
	if err != nil {
		fail("%v", err)
		return
	}
	token, err := c.token(ctx)
	if err != nil {
		fail("a token for the agents: %v", err)
		return
	}
	c.setStatus(ctx, m, "Checkpointing", fmt.Sprintf("%s on %s", podName, source), nil)
	now := time.Now()
	images := map[string]string{}
	for _, ctr := range pod.Spec.Containers {
		path, err := c.checkpoint(ctx, source, ns, podName, ctr.Name)
		if err != nil {
			fail("checkpoint of %s: %v", ctr.Name, err)
			return
		}
		ref := mover.ImageRef(ns, podName, ctr.Name, now)
		c.setStatus(ctx, m, "Moving", fmt.Sprintf("%s from %s to %s", ctr.Name, source, target), nil)
		if err := c.transfer(ctx, token, srcAddr, dstAddr, path, ref, ctr.Name); err != nil {
			fail("moving %s: %v", ctr.Name, err)
			return
		}
		images[ctr.Name] = ref
		_ = c.agent(ctx, token, http.MethodDelete, srcAddr, "/checkpoint", url.Values{"path": {path}}, nil)
	}

	// The same pod, on the target, from its checkpoints.
	c.setStatus(ctx, m, "Restoring", fmt.Sprintf("%s on %s", podName, target), nil)
	moved := pod.DeepCopy()
	moved.ObjectMeta = metav1.ObjectMeta{Name: pod.Name, Namespace: ns, Labels: pod.Labels, Annotations: pod.Annotations}
	if moved.Annotations == nil {
		moved.Annotations = map[string]string{}
	}
	moved.Annotations["gym.gachinetes.dev/moved-from"] = source
	moved.Spec.NodeName = target
	moved.Status = corev1.PodStatus{}
	withBaseImages(&moved.Spec, pod.Status.ContainerStatuses)
	for i := range moved.Spec.Containers {
		moved.Spec.Containers[i].Image = images[moved.Spec.Containers[i].Name]
		moved.Spec.Containers[i].ImagePullPolicy = corev1.PullIfNotPresent
	}
	if err := c.cs.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0))}); err != nil {
		fail("removing the old pod: %v", err)
		return
	}
	for i := 0; i < 60; i++ {
		if _, err := c.cs.CoreV1().Pods(ns).Get(ctx, pod.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			break
		}
		time.Sleep(time.Second)
	}
	if _, err := c.cs.CoreV1().Pods(ns).Create(ctx, moved, metav1.CreateOptions{FieldManager: "gachi-mover"}); err != nil {
		fail("the pod on %s: %v", target, err)
		return
	}
	c.setStatus(ctx, m, "Done", fmt.Sprintf("%s moved from %s to %s in %s", podName, source, target, time.Since(now).Round(time.Millisecond)),
		map[string]any{"source": source, "target": target})
}

// checkpoint asks the source node's kubelet, through the API server, to
// checkpoint a container, and returns the archive's path on the node.
func (c *Controller) checkpoint(ctx context.Context, node, ns, pod, container string) (string, error) {
	raw, err := c.cs.CoreV1().RESTClient().Post().AbsPath("/api/v1/nodes", node, "proxy", "checkpoint", ns, pod, container).
		Param("timeout", "120").DoRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, raw)
	}
	var res struct {
		Items []string `json:"items"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || len(res.Items) == 0 {
		return "", fmt.Errorf("the kubelet answered %s", raw)
	}
	return res.Items[0], nil
}

// transfer streams a checkpoint from the source agent to the target
// agent, which makes it an image.
func (c *Controller) transfer(ctx context.Context, token, src, dst, path, ref, container string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+src+"/checkpoint?"+url.Values{"path": {path}}.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("source agent: %s: %s", resp.Status, b)
	}
	return c.agent(ctx, token, http.MethodPost, dst, "/import", url.Values{"ref": {ref}, "container": {container}}, resp.Body)
}

func (c *Controller) agent(ctx context.Context, token, method, addr, path string, q url.Values, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, method, "https://"+addr+path+"?"+q.Encode(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent %s: %s: %s", addr, resp.Status, b)
	}
	return nil
}

// token is a short-lived token of the controller's service account, for
// the agents to check with the API server.
func (c *Controller) token(ctx context.Context) (string, error) {
	tr, err := c.cs.CoreV1().ServiceAccounts(c.namespace).CreateToken(ctx, "gachi-mover",
		&authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{ExpirationSeconds: ptr.To(int64(3600))}}, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return tr.Status.Token, nil
}

func (c *Controller) agentAddr(ctx context.Context, node string) (string, error) {
	n, err := c.cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return net.JoinHostPort(a.Address, c.agentPort), nil
		}
	}
	return "", fmt.Errorf("node %s has no internal address", node)
}

// otherNode is a Ready node other than from.
func (c *Controller) otherNode(ctx context.Context, from string) (string, error) {
	nodes, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for _, n := range nodes.Items {
		if n.Name == from || n.Spec.Unschedulable {
			continue
		}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				return n.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no other Ready node to move to")
}

// withBaseImages gives a pod to restore its containers' own images as
// image volumes, mounted nowhere: the runtime restores a checkpoint on top
// of the image it was taken of, so the kubelet has to pull that image on
// the new node first, with the pod's pull secrets, without running it.
// The digest the old node ran is the one asked for; a pod that was moved
// before has its volumes already.
func withBaseImages(spec *corev1.PodSpec, statuses []corev1.ContainerStatus) {
	have := map[string]bool{}
	for _, v := range spec.Volumes {
		have[v.Name] = true
	}
	for i, ctr := range spec.Containers {
		name := fmt.Sprintf("gachi-base-%d", i)
		if have[name] {
			continue
		}
		ref := ctr.Image
		for _, st := range statuses {
			if st.Name == ctr.Name && strings.Contains(st.ImageID, "@sha256:") {
				ref = strings.TrimPrefix(st.ImageID, "docker-pullable://")
			}
		}
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			Image: &corev1.ImageVolumeSource{Reference: ref, PullPolicy: corev1.PullIfNotPresent}}})
	}
}
